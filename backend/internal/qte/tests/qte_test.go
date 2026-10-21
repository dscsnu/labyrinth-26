package tests

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/qte"
	"labyrinth/internal/user"
)

// start is the fake clock's starting time.
var start = time.Date(2026, 10, 24, 11, 0, 0, 0, time.UTC)

// clock is a fake qte.Clock: timers fire only when at moves time past them,
// in order, never from inside AfterFunc (the store calls it holding its
// lock, and the callback takes it).
type clock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
	// leaky makes stop miss: the timer still fires, as a real one whose
	// callback was already on its way when stopped.
	leaky bool
}

// fakeTimer is one pending AfterFunc call.
type fakeTimer struct {
	due     time.Time
	f       func()
	stopped bool
}

// Now returns the fake time.
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// AfterFunc records f to run once the fake time reaches now + d.
func (c *clock) AfterFunc(d time.Duration, f func()) func() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ft := &fakeTimer{due: c.t.Add(d), f: f}
	c.timers = append(c.timers, ft)
	return func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.leaky {
			return false
		}
		was := !ft.stopped
		ft.stopped = true
		return was
	}
}

// at sets the fake time to d after start, then runs every timer now due,
// earliest first, including ones those runs schedule.
func (c *clock) at(d time.Duration) {
	c.mu.Lock()
	c.t = start.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var next *fakeTimer
		for _, ft := range c.timers {
			if !ft.stopped && !ft.due.After(c.t) && (next == nil || ft.due.Before(next.due)) {
				next = ft
			}
		}
		if next != nil {
			next.stopped = true
		}
		c.mu.Unlock()
		if next == nil {
			return
		}
		next.f()
	}
}

// fireNow runs every pending timer at once, before it's due: a real timer
// firing early.
func (c *clock) fireNow() {
	c.mu.Lock()
	var due []*fakeTimer
	for _, ft := range c.timers {
		if !ft.stopped {
			ft.stopped = true
			due = append(due, ft)
		}
	}
	c.mu.Unlock()
	for _, ft := range due {
		ft.f()
	}
}

// pending counts timers not yet fired or stopped.
func (c *clock) pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, ft := range c.timers {
		if !ft.stopped {
			n++
		}
	}
	return n
}

// fixture is a qte store on a fresh schema, a hub with one subscriber, an
// admin, and the fake clock.
type fixture struct {
	store *qte.Store
	db    *postgres.DB
	dbURL string
	hub   *live.Hub
	sub   *live.Subscription
	rules qte.Rules
	clock *clock
	admin uuid.UUID
}

// powerups are the kinds a QTE may earn, as teamstate lists them.
var powerups = []string{"damage_multiplier", "heart", "short_qr_cooldown", "instant_cast"}

// setup starts a qte store from config.toml's [qte] on an empty schema.
func setup(t *testing.T) *fixture {
	t.Helper()
	ctx := context.Background()
	set := configtest.Settings(t)
	dbURL := pgtest.URL(t, "qte_test")
	db := pgtest.Open(t, dbURL)
	admin := pgtest.Must(user.New(db).SignInWithGoogle(ctx, user.GoogleProfile{Subject: "qte-admin", Email: "admin@snu.edu.in", Name: "A"}))
	f := &fixture{db: db, dbURL: dbURL, hub: live.NewHub(set.Live.HubShards, set.Live.SubscriberBuffer), clock: &clock{t: start}, admin: admin.ID}
	f.rules = qte.Rules{AnswerWindow: set.QTE.AnswerWindow.Duration, MaxAnswerWindow: set.QTE.MaxAnswerWindow.Duration,
		Powerups: powerups, WriteTimeout: set.Storage.WriteTimeout.Duration, Clock: f.clock}
	f.store = pgtest.Must(qte.Load(ctx, db, f.hub, pgtest.Quiet, f.rules))
	f.sub = f.hub.Subscribe(uuid.New()) // any stream gets game-wide events
	t.Cleanup(f.sub.Close)
	t.Cleanup(f.store.Close)
	return f
}

// restart closes the store and loads a new one from the same database, as a
// backend restart would.
func (f *fixture) restart(t *testing.T) {
	t.Helper()
	f.store.Close()
	f.store = pgtest.Must(qte.Load(context.Background(), pgtest.Open(t, f.dbURL), f.hub, pgtest.Quiet, f.rules))
}

// draft is a valid quiz earning a heart, the right option being 2.
func draft() qte.Draft {
	return qte.Draft{Kind: qte.KindQuiz, Powerup: "heart",
		Quiz: &qte.Quiz{Question: "Which building has the clock tower?", Options: []string{"A", "B", "C", "D"}, CorrectOption: 2}}
}

// event is a received "qte" event.
type event struct {
	typ  live.EventType
	raw  string
	view *qte.View
}

// next reads the next qte event, failing if none arrives.
func (f *fixture) next(t *testing.T, what string) event {
	t.Helper()
	select {
	case ev := <-f.sub.C:
		if ev.Name() != "qte" {
			t.Fatalf("%s: event %q, want qte", what, ev.Name())
		}
		var env struct {
			Message *qte.View `json:"message"`
		}
		pgtest.Must(0, json.Unmarshal(ev.Data(), &env))
		return event{typ: ev.Type(), raw: string(ev.Data()), view: env.Message}
	case <-time.After(time.Second):
		t.Fatalf("%s: no qte event", what)
	}
	return event{}
}

// quiet fails if an event is waiting.
func (f *fixture) quiet(t *testing.T, what string) {
	t.Helper()
	select {
	case ev := <-f.sub.C:
		t.Fatalf("%s: unexpected %s %s", what, ev.Name(), ev.Type())
	default:
	}
}

// TestDraftThenStart checks a draft: it isn't public and sends nothing;
// starting it opens it for qte.answer_window, broadcasts started without the
// answer (correct_option null in the very bytes), and makes it the public
// QTE and the snapshot; the full QTE (ByID, List) keeps its answer.
func TestDraftThenStart(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	q := pgtest.Must(f.store.Create(ctx, f.admin, draft(), false))
	if q.Status(start) != qte.StatusDraft || f.store.Public() != nil || q.CreatedBy == nil || *q.CreatedBy != f.admin {
		t.Fatalf("a draft: %+v, public %v", q, f.store.Public())
	}
	f.quiet(t, "a draft")

	f.clock.at(5 * time.Second)
	q = pgtest.Must(f.store.Start(ctx, q.ID))
	if !q.EndsAt.Equal(start.Add(5*time.Second + f.rules.AnswerWindow)) {
		t.Fatalf("ends_at %v, want start + 5 s + qte.answer_window", q.EndsAt)
	}
	ev := f.next(t, "start")
	if ev.typ != qte.Started || ev.view == nil || ev.view.ID != q.ID || ev.view.Status != qte.StatusOpen ||
		ev.view.Quiz == nil || ev.view.Quiz.CorrectOption != nil || !strings.Contains(ev.raw, `"correct_option":null`) {
		t.Fatalf("started event: %s", ev.raw)
	}
	snap := pgtest.Must(f.store.Snapshot())
	if p := f.store.Public(); p == nil || p.ID != q.ID || p.Quiz.CorrectOption != nil || !strings.Contains(string(snap.Data()), `"correct_option":null`) {
		t.Fatalf("public %+v, snapshot %s", p, snap.Data())
	}
	if full, ok := f.store.ByID(q.ID); !ok || full.Quiz.CorrectOption != 2 || len(f.store.List()) != 1 {
		t.Fatalf("ByID %+v, List %d", full, len(f.store.List()))
	}
}

// TestCloseTimer checks the window's end: nothing 1 µs before ends_at;
// exactly at it the timer broadcasts closed, with correct_option revealed;
// then the public QTE and snapshot are null, nothing else is sent, and no
// timer is left.
func TestCloseTimer(t *testing.T) {
	f := setup(t)
	q := pgtest.Must(f.store.Create(context.Background(), f.admin, draft(), true))
	f.next(t, "start")
	f.clock.at(f.rules.AnswerWindow - time.Microsecond)
	f.quiet(t, "before ends_at")
	if f.store.Public() == nil {
		t.Fatal("closed before ends_at")
	}
	f.clock.at(f.rules.AnswerWindow)
	ev := f.next(t, "close")
	if ev.typ != qte.Closed || ev.view == nil || ev.view.ID != q.ID || ev.view.Status != qte.StatusClosed ||
		ev.view.Quiz.CorrectOption == nil || *ev.view.Quiz.CorrectOption != 2 {
		t.Fatalf("closed event: %s", ev.raw)
	}
	snap := pgtest.Must(f.store.Snapshot())
	if f.store.Public() != nil || !strings.Contains(string(snap.Data()), `"message":null`) || f.clock.pending() != 0 {
		t.Fatalf("after the close: public %v, snapshot %s, %d timers", f.store.Public(), snap.Data(), f.clock.pending())
	}
	f.quiet(t, "one close")
}

// TestOneAtATime checks that a second QTE can't start (nor be created
// started) while one is open, refused from memory with nothing published, that the first can't run again, and that a
// second starts once the first has closed.
func TestOneAtATime(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	first := pgtest.Must(f.store.Create(ctx, f.admin, draft(), true))
	second := pgtest.Must(f.store.Create(ctx, f.admin, draft(), false))
	f.next(t, "the first starts")
	if _, err := f.store.Start(ctx, second.ID); !errors.Is(err, qte.ErrQTEOpen) {
		t.Fatalf("starting a second: %v", err)
	}
	f.quiet(t, "a refused start (memory refuses it; the database constraint is only the backstop)")
	if _, err := f.store.Create(ctx, f.admin, draft(), true); !errors.Is(err, qte.ErrQTEOpen) {
		t.Fatalf("creating one started: %v", err)
	}
	f.clock.at(f.rules.AnswerWindow)
	if _, err := f.store.Start(ctx, first.ID); !errors.Is(err, qte.ErrQTEAlreadyRun) {
		t.Fatalf("running the first again: %v", err)
	}
	if _, err := f.store.Start(ctx, second.ID); err != nil {
		t.Fatalf("the second after the first closed: %v", err)
	}
	if _, err := f.store.Start(ctx, 999); !errors.Is(err, qte.ErrUnknownQTE) {
		t.Fatalf("an unknown id: %v", err)
	}
}

// TestEndEarly checks End: an open QTE closes now (closed with the answer,
// its timer stopped, ends_at now), after which End is qte_closed; a draft
// is qte_not_started; an unknown id unknown_qte; a new QTE may start at once.
func TestEndEarly(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	q := pgtest.Must(f.store.Create(ctx, f.admin, draft(), true))
	f.next(t, "start")
	f.clock.at(10 * time.Second)
	q = pgtest.Must(f.store.End(ctx, q.ID))
	if ev := f.next(t, "end"); ev.typ != qte.Closed || ev.view.Quiz.CorrectOption == nil || !q.EndsAt.Equal(start.Add(10*time.Second)) {
		t.Fatalf("end: event %s, ends_at %v", ev.raw, q.EndsAt)
	}
	if f.store.Public() != nil || f.clock.pending() != 0 {
		t.Fatalf("after End: public %v, %d timers", f.store.Public(), f.clock.pending())
	}
	f.clock.at(f.rules.AnswerWindow) // the original end: nothing more
	f.quiet(t, "the original window's end")
	if _, err := f.store.End(ctx, q.ID); !errors.Is(err, qte.ErrQTEClosed) {
		t.Fatalf("ending it again: %v", err)
	}
	d := pgtest.Must(f.store.Create(ctx, f.admin, draft(), false))
	if _, err := f.store.End(ctx, d.ID); !errors.Is(err, qte.ErrQTENotStarted) {
		t.Fatalf("ending a draft: %v", err)
	}
	if _, err := f.store.End(ctx, 999); !errors.Is(err, qte.ErrUnknownQTE) {
		t.Fatalf("an unknown id: %v", err)
	}
	if _, err := f.store.Start(ctx, d.ID); err != nil {
		t.Fatalf("starting after an early end: %v", err)
	}
}

// TestValidation checks every draft refusal, in order, and the boundaries:
// 2 options and a window of exactly qte.max_answer_window pass; a QTE's own
// window sets its ends_at.
func TestValidation(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	one, zero, over := 1, 0, int(f.rules.MaxAnswerWindow/time.Second)+1
	maxW := over - 1
	wrap, huge := 9223372037, math.MaxInt // wrap: × 1 s overflows an int64 to negative
	cases := map[string]struct {
		edit func(*qte.Draft)
		want error
	}{
		"kind":                    {func(d *qte.Draft) { d.Kind = "race" }, qte.ErrUnknownKind},
		"no quiz":                 {func(d *qte.Draft) { d.Quiz = nil }, qte.ErrUnknownKind},
		"powerup":                 {func(d *qte.Draft) { d.Powerup = "shield" }, qte.ErrUnknownPowerup},
		"blank question":          {func(d *qte.Draft) { d.Quiz.Question = "  " }, qte.ErrInvalidQuestion},
		"one option":              {func(d *qte.Draft) { d.Quiz.Options = []string{"A"}; d.Quiz.CorrectOption = 0 }, qte.ErrInvalidOptions},
		"an empty option":         {func(d *qte.Draft) { d.Quiz.Options = []string{"A", " ", "C"}; d.Quiz.CorrectOption = 0 }, qte.ErrInvalidOptions},
		"a repeated option":       {func(d *qte.Draft) { d.Quiz.Options = []string{"A", "B", "A"}; d.Quiz.CorrectOption = 0 }, qte.ErrInvalidOptions},
		"correct option -1":       {func(d *qte.Draft) { d.Quiz.CorrectOption = -1 }, qte.ErrInvalidCorrectOption},
		"correct option past end": {func(d *qte.Draft) { d.Quiz.CorrectOption = 4 }, qte.ErrInvalidCorrectOption},
		"window 0":                {func(d *qte.Draft) { d.WindowS = &zero }, qte.ErrInvalidWindow},
		"window over the max":     {func(d *qte.Draft) { d.WindowS = &over }, qte.ErrInvalidWindow},
		"window that wraps":       {func(d *qte.Draft) { d.WindowS = &wrap }, qte.ErrInvalidWindow},
		"window of max int":       {func(d *qte.Draft) { d.WindowS = &huge }, qte.ErrInvalidWindow},
		"two options":             {func(d *qte.Draft) { d.Quiz.Options = []string{"A", "B"}; d.Quiz.CorrectOption = 1 }, nil},
		"window 1 s":              {func(d *qte.Draft) { d.WindowS = &one }, nil},
		"window at the max":       {func(d *qte.Draft) { d.WindowS = &maxW }, nil},
	}
	for name, c := range cases {
		d := draft()
		c.edit(&d)
		if _, err := f.store.Create(ctx, f.admin, d, false); !errors.Is(err, c.want) && !(c.want == nil && err == nil) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
	w := draft()
	w.WindowS = &wrap
	if _, err := f.store.Create(ctx, f.admin, w, true); !errors.Is(err, qte.ErrInvalidWindow) {
		t.Fatalf("starting with a window that wraps: %v", err)
	}
	f.quiet(t, "a refused start")
	d := draft()
	sixty := 60
	d.WindowS = &sixty
	q := pgtest.Must(f.store.Create(ctx, f.admin, d, true))
	if !q.EndsAt.Equal(start.Add(60 * time.Second)) {
		t.Fatalf("a 60 s window ends at %v", q.EndsAt)
	}
}

// TestRestart checks that a restart reproduces every QTE exactly (JSON
// equal), re-arms the timer of an open one (which then closes on time), and
// that a QTE which closed while the backend was down loads closed.
func TestRestart(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pgtest.Must(f.store.Create(ctx, f.admin, draft(), false))
	pgtest.Must(f.store.Create(ctx, f.admin, draft(), true))
	f.next(t, "start")
	before := pgtest.Must(json.Marshal(f.store.List()))
	f.restart(t)
	if after := pgtest.Must(json.Marshal(f.store.List())); string(after) != string(before) {
		t.Fatalf("across a restart\nbefore: %s\nafter:  %s", before, after)
	}
	if f.store.Public() == nil {
		t.Fatal("the open QTE didn't survive the restart")
	}
	f.clock.at(f.rules.AnswerWindow)
	if ev := f.next(t, "close after the restart"); ev.typ != qte.Closed {
		t.Fatalf("after the restart: %s", ev.raw)
	}

	pgtest.Must(f.store.Create(ctx, f.admin, draft(), true))
	f.next(t, "second start")
	f.store.Close() // down through its end
	f.clock.at(3 * f.rules.AnswerWindow)
	f.restart(t)
	if f.store.Public() != nil || f.clock.pending() != 0 {
		t.Fatalf("restarted after its end: public %v, %d timers", f.store.Public(), f.clock.pending())
	}
}

// TestReadsWithDatabaseClosed checks that every read is memory: List, ByID,
// Public and Snapshot work with the database closed; a write fails.
func TestReadsWithDatabaseClosed(t *testing.T) {
	f := setup(t)
	q := pgtest.Must(f.store.Create(context.Background(), f.admin, draft(), true))
	f.db.Close()
	if _, ok := f.store.ByID(q.ID); !ok || len(f.store.List()) != 1 || f.store.Public() == nil {
		t.Fatal("reads failed with the database closed")
	}
	if _, err := f.store.Snapshot(); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Create(context.Background(), f.admin, draft(), false); err == nil {
		t.Fatal("a write with the database closed should fail")
	}
}

// TestReconcile checks a failed write: a draft started by hand in Postgres
// makes Start fail (no row still a draft), and memory reloads (Postgres
// wins), broadcast as resync with the QTE now open.
func TestReconcile(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	q := pgtest.Must(f.store.Create(ctx, f.admin, draft(), false))
	if _, err := f.db.Pool().Exec(ctx, "UPDATE qte SET started_at = $2, ends_at = $3 WHERE id = $1",
		q.ID, start, start.Add(f.rules.AnswerWindow)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.Start(ctx, q.ID); err == nil {
		t.Fatal("starting a row started by hand should fail")
	}
	if ev := f.next(t, "reconcile"); ev.typ != live.Resync || ev.view == nil || ev.view.ID != q.ID {
		t.Fatalf("resync: %s", ev.raw)
	}
	if got, _ := f.store.ByID(q.ID); got.StartedAt == nil {
		t.Fatal("memory didn't take Postgres's start")
	}
}

// TestConcurrentStarts checks that 8 drafts started at once leave exactly
// one open.
func TestConcurrentStarts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	const n = 8
	ids := make([]int64, n)
	for i := range n {
		ids[i] = pgtest.Must(f.store.Create(ctx, f.admin, draft(), false)).ID
	}
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Go(func() { _, errs[i] = f.store.Start(ctx, ids[i]) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, qte.ErrQTEOpen):
			t.Fatalf("a losing start: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent starts opened a QTE, want 1", ok, n)
	}
}

// TestSeededDrafts checks the dev seed's quizzes: drafts 1–4, one per
// powerup, each valid; an admin's next QTE gets id 5 (the sequence moved
// past them); and a rerun of the seed refreshes a seeded draft but leaves
// one that has run alone.
func TestSeededDrafts(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pgtest.Must(0, f.db.Seed2026(ctx))
	f.restart(t)
	all := f.store.List()
	if len(all) != 4 {
		t.Fatalf("%d seeded QTEs, want 4", len(all))
	}
	kinds := map[string]bool{}
	for _, q := range all {
		if q.Status(start) != qte.StatusDraft || q.CreatedBy != nil || q.ID < 1 || q.ID > 4 {
			t.Fatalf("seeded QTE %+v", q)
		}
		kinds[q.Powerup] = true
	}
	if len(kinds) != 4 {
		t.Fatalf("seeded powerups %v, want one of each", kinds)
	}
	if q := pgtest.Must(f.store.Create(ctx, f.admin, draft(), false)); q.ID != 5 {
		t.Fatalf("the first admin QTE got id %d, want 5", q.ID)
	}

	pgtest.Must(f.store.Start(ctx, 1))
	if _, err := f.db.Pool().Exec(ctx, "UPDATE qte SET question = 'Changed' WHERE id IN (1, 2)"); err != nil {
		t.Fatal(err)
	}
	pgtest.Must(0, f.db.Seed2026(ctx))
	f.restart(t)
	ran, _ := f.store.ByID(1)
	fresh, _ := f.store.ByID(2)
	if ran.Quiz.Question != "Changed" || ran.StartedAt == nil || fresh.Quiz.Question == "Changed" {
		t.Fatalf("after a rerun: run %q (started %v), draft %q", ran.Quiz.Question, ran.StartedAt, fresh.Quiz.Question)
	}
}

// openByHand inserts a quiz open from start in Postgres only (memory doesn't
// know it), returning its id.
func (f *fixture) openByHand(t *testing.T) int64 {
	t.Helper()
	var id int64
	if err := f.db.Pool().QueryRow(context.Background(), `
		INSERT INTO qte (kind, powerup, question, options, correct_option, started_at, ends_at)
		VALUES ('quiz', 'heart', 'By hand?', ARRAY['A', 'B'], 0, $1, $2) RETURNING id`,
		start, start.Add(f.rules.AnswerWindow)).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestOverlapBackstop checks the exclusion constraint behind "one at a
// time": with a QTE open in Postgres that memory doesn't know, Create with
// start and Start both pass the memory check, are refused by Postgres
// (ErrQTEOpen), write nothing, and reload memory (resync, the hand-made
// QTE now open).
func TestOverlapBackstop(t *testing.T) {
	ctx := context.Background()
	t.Run("start", func(t *testing.T) {
		f := setup(t)
		d := pgtest.Must(f.store.Create(ctx, f.admin, draft(), false))
		hand := f.openByHand(t)
		if _, err := f.store.Start(ctx, d.ID); !errors.Is(err, qte.ErrQTEOpen) {
			t.Fatalf("start over a hand-made open QTE: %v", err)
		}
		if ev := f.next(t, "resync"); ev.typ != live.Resync || ev.view == nil || ev.view.ID != hand {
			t.Fatalf("resync: %s", ev.raw)
		}
		if got, _ := f.store.ByID(d.ID); got.StartedAt != nil {
			t.Fatalf("the refused draft started: %+v", got)
		}
	})
	t.Run("create", func(t *testing.T) {
		f := setup(t)
		hand := f.openByHand(t)
		if _, err := f.store.Create(ctx, f.admin, draft(), true); !errors.Is(err, qte.ErrQTEOpen) {
			t.Fatalf("create over a hand-made open QTE: %v", err)
		}
		if ev := f.next(t, "resync"); ev.typ != live.Resync || ev.view == nil || ev.view.ID != hand {
			t.Fatalf("resync: %s", ev.raw)
		}
		if all := f.store.List(); len(all) != 1 || all[0].ID != hand {
			t.Fatalf("after the refused create: %+v", all)
		}
	})
}

// TestEndPersistFails checks a failed End: the QTE closed by hand in
// Postgres (so no row is still open to end), End fails, and memory reloads
// it closed (resync with null: nothing open); nothing is revealed twice.
func TestEndPersistFails(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	q := pgtest.Must(f.store.Create(ctx, f.admin, draft(), true))
	f.next(t, "start")
	if _, err := f.db.Pool().Exec(ctx, "UPDATE qte SET ends_at = started_at WHERE id = $1", q.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.End(ctx, q.ID); err == nil || errors.Is(err, qte.ErrQTEClosed) {
		t.Fatalf("ending a QTE closed by hand: %v", err)
	}
	if ev := f.next(t, "resync"); ev.typ != live.Resync || ev.view != nil {
		t.Fatalf("resync: %s", ev.raw)
	}
	if got, _ := f.store.ByID(q.ID); got.Status(start) != qte.StatusClosed {
		t.Fatalf("after the reconcile: %+v", got)
	}
	f.quiet(t, "after the failed end")
}

// TestLoadRefusesUnknown checks that Load refuses a stored QTE whose kind or
// powerup this code doesn't have (content and code out of step): a powerup
// teamstate no longer lists, and a kind the CHECK would refuse (dropped
// here to plant one).
func TestLoadRefusesUnknown(t *testing.T) {
	ctx := context.Background()
	f := setup(t)
	pgtest.Must(f.store.Create(ctx, f.admin, draft(), false)) // a heart
	rules := f.rules
	rules.Powerups = []string{"instant_cast"}
	if _, err := qte.Load(ctx, pgtest.Open(t, f.dbURL), f.hub, pgtest.Quiet, rules); err == nil || !strings.Contains(err.Error(), `unknown powerup "heart"`) {
		t.Fatalf("load with heart unknown: %v", err)
	}

	if _, err := f.db.Pool().Exec(ctx, "ALTER TABLE qte DROP CONSTRAINT qte_kind_check; UPDATE qte SET kind = 'poll'"); err != nil {
		t.Fatal(err)
	}
	if _, err := qte.Load(ctx, pgtest.Open(t, f.dbURL), f.hub, pgtest.Quiet, f.rules); err == nil || !strings.Contains(err.Error(), `unknown kind "poll"`) {
		t.Fatalf("load with a poll: %v", err)
	}
}

// TestTimerFiresEarly checks a close timer that fires before ends_at (a
// real timer may): it re-arms instead of closing, and the QTE closes once,
// on time.
func TestTimerFiresEarly(t *testing.T) {
	f := setup(t)
	pgtest.Must(f.store.Create(context.Background(), f.admin, draft(), true))
	f.next(t, "start")
	f.clock.at(f.rules.AnswerWindow / 2)
	f.clock.fireNow()
	f.quiet(t, "an early fire")
	if f.store.Public() == nil || f.clock.pending() != 1 {
		t.Fatalf("after an early fire: public %v, %d timers", f.store.Public(), f.clock.pending())
	}
	f.clock.at(f.rules.AnswerWindow)
	if ev := f.next(t, "close"); ev.typ != qte.Closed {
		t.Fatalf("close: %s", ev.raw)
	}
	f.quiet(t, "after the close")
}

// TestStaleTimer checks a close timer that fires after its QTE was ended
// and another started (the stop came too late): it does nothing; no
// second closed for the ended QTE, and the new one stays open.
func TestStaleTimer(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	f.clock.leaky = true
	q1 := pgtest.Must(f.store.Create(ctx, f.admin, draft(), true))
	f.next(t, "start 1")
	pgtest.Must(f.store.End(ctx, q1.ID))
	f.next(t, "end 1")
	f.clock.at(time.Second)
	pgtest.Must(f.store.Create(ctx, f.admin, draft(), true))
	f.next(t, "start 2")
	f.clock.at(f.rules.AnswerWindow) // q1's timer fires, stale
	f.quiet(t, "the stale timer")
	if f.store.Public() == nil {
		t.Fatal("the stale timer closed the new QTE")
	}
}

// TestTimerAfterClose checks a close timer that fires after Close (the stop
// came too late): it does nothing.
func TestTimerAfterClose(t *testing.T) {
	f := setup(t)
	f.clock.leaky = true
	pgtest.Must(f.store.Create(context.Background(), f.admin, draft(), true))
	f.next(t, "start")
	f.store.Close()
	f.clock.at(f.rules.AnswerWindow)
	f.quiet(t, "a timer after Close")
}

// TestWriteAfterClose checks a write landing after Close (a request still in
// flight at shutdown): it's applied and published, but arms no timer.
func TestWriteAfterClose(t *testing.T) {
	f := setup(t)
	f.store.Close()
	pgtest.Must(f.store.Create(context.Background(), f.admin, draft(), true))
	f.next(t, "start")
	if n := f.clock.pending(); n != 0 {
		t.Fatalf("%d timers armed after Close", n)
	}
}

// TestLoadReadErrors checks that Load fails when its read does, either way:
// the qte table missing (refused when the query is prepared), or a row
// failing as it streams (a view over the table dividing by zero).
func TestLoadReadErrors(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	pgtest.Must(f.store.Create(ctx, f.admin, draft(), false))
	for name, sql := range map[string][2]string{
		"missing": {"ALTER TABLE qte RENAME TO qte_real", "ALTER TABLE qte_real RENAME TO qte"},
		"failing a row": {"ALTER TABLE qte RENAME TO qte_real; CREATE VIEW qte AS SELECT * FROM qte_real WHERE 1 / (random() * 0)::int = 1",
			"DROP VIEW qte; ALTER TABLE qte_real RENAME TO qte"},
	} {
		if _, err := f.db.Pool().Exec(ctx, sql[0]); err != nil {
			t.Fatal(err)
		}
		_, err := qte.Load(ctx, pgtest.Open(t, f.dbURL), f.hub, pgtest.Quiet, f.rules)
		if _, ferr := f.db.Pool().Exec(ctx, sql[1]); ferr != nil {
			t.Fatal(ferr)
		}
		if err == nil || !strings.HasPrefix(err.Error(), "load qte: ") {
			t.Errorf("%s: load: %v", name, err)
		}
	}
}
