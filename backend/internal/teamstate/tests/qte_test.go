package tests

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/qte"
	"labyrinth/internal/teamstate"
)

// quiz starts a quiz now (member 0 writes it) earning powerup, the right
// option being 2 of 4, open for qte.answer_window.
func (f *fixtureEnv) quiz(t *testing.T, powerup string) qte.QTE {
	t.Helper()
	return pgtest.Must(f.qtes.Create(context.Background(), f.users[0], qte.Draft{Kind: qte.KindQuiz, Powerup: powerup,
		Quiz: &qte.Quiz{Question: "Which?", Options: []string{"A", "B", "C", "D"}, CorrectOption: 2}}, true))
}

// answer has member i answer QTE q with option, failing on an error.
func (f *fixtureEnv) answer(t *testing.T, i int, q qte.QTE, option int) teamstate.View {
	t.Helper()
	v, err := f.state.AnswerQTE(context.Background(), f.users[i], q.ID, option)
	if err != nil {
		t.Fatalf("member %d answering %d: %v", i, option, err)
	}
	return v
}

// TestAnswerQTE checks a right answer by a member who isn't the leader: qte
// shows it (correct, granted), the team now holds the powerup, members get
// qte_answered with the version up by one; then a teammate's resend is
// qte_already_answered with nothing changed. A wrong answer to the next
// quiz: correct and granted false, held unchanged.
func TestAnswerQTE(t *testing.T) {
	f := started(t)
	sub := f.hub.Subscribe(f.users[2])
	defer sub.Close()
	q := f.quiz(t, "heart")
	nextQTE(t, sub)
	seq := f.view(t).Version.Seq

	v := f.answer(t, 1, q, 2)
	if a := v.QTE; a == nil || a.QTEID != q.ID || a.Option != 2 || !a.Correct || !a.Granted || a.AnsweredBy != f.users[1] {
		t.Fatalf("a right answer: %+v", v.QTE)
	}
	if len(v.Powerups.Held) != 1 || v.Powerups.Held[0] != "heart" {
		t.Fatalf("held after a right answer: %v", v.Powerups.Held)
	}
	if ev := expectType(t, sub, "answer", teamstate.QTEAnswered); ev.Version.Seq != seq+1 || ev.QTE == nil {
		t.Fatalf("answer event: seq %d (was %d), qte %+v", ev.Version.Seq, seq, ev.QTE)
	}
	if _, err := f.state.AnswerQTE(context.Background(), f.users[3], q.ID, 1); !errors.Is(err, teamstate.ErrQTEAlreadyAnswered) {
		t.Fatalf("a teammate's resend: %v", err)
	}
	expectNoEvent(t, sub, "a refused resend")

	f.clock.at(q.EndsAt.Sub(start))
	nextQTE(t, sub)
	q2 := f.quiz(t, "damage_multiplier")
	nextQTE(t, sub)
	v = f.answer(t, 0, q2, 0)
	if a := v.QTE; a.QTEID != q2.ID || a.Correct || a.Granted || len(v.Powerups.Held) != 1 {
		t.Fatalf("a wrong answer: %+v, held %v", v.QTE, v.Powerups.Held)
	}
}

// nextQTE reads the next event on sub, failing unless it's a qte event.
func nextQTE(t *testing.T, sub *live.Subscription) {
	t.Helper()
	select {
	case ev := <-sub.C:
		if ev.Name() != "qte" {
			t.Fatalf("event %q, want qte", ev.Name())
		}
	case <-time.After(time.Second):
		t.Fatal("no qte event")
	}
}

// TestAnswerQTEWasted checks a right answer from a team already holding the
// QTE's powerup: correct, but granted false, and still one held.
func TestAnswerQTEWasted(t *testing.T) {
	f := started(t)
	f.grant(t, teamstate.PowerupHeart)
	v := f.answer(t, 0, f.quiz(t, "heart"), 2)
	if !v.QTE.Correct || v.QTE.Granted || len(v.Powerups.Held) != 1 {
		t.Fatalf("a right answer while holding it: %+v, held %v", v.QTE, v.Powerups.Held)
	}
}

// TestAnswerQTEWindow checks the window's edge: an answer 1 µs before
// ends_at is accepted; exactly at ends_at (the next quiz) is qte_closed;
// after an admin ends one early, qte_closed.
func TestAnswerQTEWindow(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	q := f.quiz(t, "heart")
	f.clock.at(q.EndsAt.Sub(start) - time.Microsecond)
	f.answer(t, 0, q, 1)

	f.clock.at(q.EndsAt.Sub(start))
	q2 := f.quiz(t, "heart")
	f.clock.at(q2.EndsAt.Sub(start))
	if _, err := f.state.AnswerQTE(ctx, f.users[0], q2.ID, 1); !errors.Is(err, teamstate.ErrQTEClosed) {
		t.Fatalf("exactly at ends_at: %v", err)
	}
	q3 := f.quiz(t, "heart")
	pgtest.Must(f.qtes.End(ctx, q3.ID))
	if _, err := f.state.AnswerQTE(ctx, f.users[0], q3.ID, 1); !errors.Is(err, teamstate.ErrQTEClosed) {
		t.Fatalf("after an admin ended it: %v", err)
	}
}

// TestAnswerQTERefusals checks the other refusals: an unknown QTE, a draft,
// an option past the last, someone in no team, a lobby team, a knocked-out
// team; none changes anything.
func TestAnswerQTERefusals(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	if _, err := f.state.AnswerQTE(ctx, f.users[0], 999, 0); !errors.Is(err, teamstate.ErrUnknownQTE) {
		t.Fatalf("an unknown qte: %v", err)
	}
	d := pgtest.Must(f.qtes.Create(ctx, f.users[0], qte.Draft{Kind: qte.KindQuiz, Powerup: "heart",
		Quiz: &qte.Quiz{Question: "Q", Options: []string{"A", "B"}, CorrectOption: 0}}, false))
	if _, err := f.state.AnswerQTE(ctx, f.users[0], d.ID, 0); !errors.Is(err, teamstate.ErrUnknownQTE) {
		t.Fatalf("a draft: %v", err)
	}
	q := f.quiz(t, "heart")
	if _, err := f.state.AnswerQTE(ctx, f.users[0], q.ID, 4); !errors.Is(err, teamstate.ErrInvalidOption) {
		t.Fatalf("option 4 of 4: %v", err)
	}
	if _, err := f.state.AnswerQTE(ctx, uuid.New(), q.ID, 0); !errors.Is(err, teamstate.ErrNotInTeam) {
		t.Fatalf("no team: %v", err)
	}
	if v := f.view(t); v.QTE != nil {
		t.Fatalf("refusals recorded an answer: %+v", v.QTE)
	}

	lobby := started(t, fixtureOptions{lobby: true})
	lq := lobby.quiz(t, "heart")
	if _, err := lobby.state.AnswerQTE(ctx, lobby.users[0], lq.ID, 2); !errors.Is(err, teamstate.ErrNotInGame) {
		t.Fatalf("a lobby team: %v", err)
	}

	ko := started(t, fixtureOptions{attacks: true, bossRules: func(r *bosses.Rules) { r.AttackDamage[1] = 100 }})
	ko.clock.at(ko.interval(1))
	kq := ko.quiz(t, "heart")
	if _, err := ko.state.AnswerQTE(ctx, ko.users[0], kq.ID, 2); !errors.Is(err, teamstate.ErrKnockedOut) {
		t.Fatalf("knocked out: %v", err)
	}
}

// TestAnswerQTERestart checks that a restart reproduces the answer and the
// powerup it granted (team_state JSON equal, version aside), and that the
// team still can't answer again.
func TestAnswerQTERestart(t *testing.T) {
	f := started(t)
	q := f.quiz(t, "instant_cast")
	f.answer(t, 2, q, 2)
	before := withoutVersionJSON(t, f.view(t))
	f.restart(t)
	if after := withoutVersionJSON(t, f.view(t)); after != before {
		t.Fatalf("across a restart\nbefore: %s\nafter:  %s", before, after)
	}
	if _, err := f.state.AnswerQTE(context.Background(), f.users[0], q.ID, 2); !errors.Is(err, teamstate.ErrQTEAlreadyAnswered) {
		t.Fatalf("answering again after a restart: %v", err)
	}
}

// TestConcurrentAnswers checks that every member answering at once leaves
// exactly one answer: the rest are qte_already_answered.
func TestConcurrentAnswers(t *testing.T) {
	f := started(t)
	q := f.quiz(t, "heart")
	errs := make([]error, len(f.users))
	var wg sync.WaitGroup
	for i := range f.users {
		wg.Go(func() { _, errs[i] = f.state.AnswerQTE(context.Background(), f.users[i], q.ID, i%4) })
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, teamstate.ErrQTEAlreadyAnswered):
			t.Fatalf("a losing answer: %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d of %d concurrent answers recorded, want 1", ok, len(f.users))
	}
}

// TestAnswerQTEReconcile checks a failed answer: a row inserted by hand
// (memory doesn't know it) makes the answer's insert fail on the primary
// key, and memory reloads it (Postgres wins), published as resync.
func TestAnswerQTEReconcile(t *testing.T) {
	f := started(t)
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	q := f.quiz(t, "heart")
	nextQTE(t, sub)
	if _, err := f.db.Pool().Exec(context.Background(), `
		INSERT INTO team_qte_answer (team_id, qte_id, option, correct, granted, answered_by, answered_at)
		VALUES ($1, $2, 0, false, false, $3, $4)`, f.teamID, q.ID, f.users[1], start); err != nil {
		t.Fatal(err)
	}
	if _, err := f.state.AnswerQTE(context.Background(), f.users[0], q.ID, 2); err == nil {
		t.Fatal("answering over a row inserted by hand should fail")
	}
	if v := expectType(t, sub, "reconcile", live.Resync); v.QTE == nil || v.QTE.Option != 0 || v.QTE.Correct {
		t.Fatalf("after the reconcile: %+v", v.QTE)
	}
}

// TestQTEStats checks the per-QTE counts across teams: answered, correct,
// granted and per option.
func TestQTEStats(t *testing.T) {
	f := started(t)
	q := f.quiz(t, "heart")
	f.answer(t, 0, q, 2)
	got := f.state.QTEStats()[q.ID]
	if got.Answered != 1 || got.Correct != 1 || got.Granted != 1 || got.ByOption[2] != 1 {
		t.Fatalf("stats: %+v", got)
	}
	if _, ok := f.state.QTEStats()[q.ID+1]; ok {
		t.Fatal("stats for a QTE nobody answered")
	}
	js := pgtest.Must(json.Marshal(got))
	if string(js) != `{"answered":1,"correct":1,"granted":1,"by_option":{"2":1}}` {
		t.Fatalf("stats JSON %s", js)
	}
}

// TestAnswerAfterEndInPostgres checks an answer landing after an admin's End
// committed but before memory caught up (Postgres closed, memory open, set up
// by hand): the stored ends_at refuses it, qte_closed, with nothing stored,
// granted or published, and no reload.
func TestAnswerAfterEndInPostgres(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	q := f.quiz(t, "heart")
	nextQTE(t, sub)
	if _, err := f.db.Pool().Exec(ctx, "UPDATE qte SET ends_at = started_at WHERE id = $1", q.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.state.AnswerQTE(ctx, f.users[0], q.ID, 2); !errors.Is(err, teamstate.ErrQTEClosed) {
		t.Fatalf("an answer after the stored ends_at: %v", err)
	}
	expectNoEvent(t, sub, "a refused late answer")
	var n int
	if err := f.db.Pool().QueryRow(ctx, "SELECT count(*) FROM team_qte_answer").Scan(&n); err != nil || n != 0 {
		t.Fatalf("answers stored: %d (%v)", n, err)
	}
	if v := f.view(t); v.QTE != nil || len(v.Powerups.Held) != 0 {
		t.Fatalf("after the refused answer: qte %+v, held %v", v.QTE, v.Powerups.Held)
	}
	f.restart(t)
	if v := f.view(t); len(v.Powerups.Held) != 0 {
		t.Fatalf("a powerup stored for the refused answer: %v", v.Powerups.Held)
	}
}
