package tests

// teamstate.Store-level tests against a real Postgres (TEST_DATABASE_URL), in schema
// teamstate_test, with the 2025 dev seed, config.toml's rules and a fake
// clock. In the seed the square T6, R, SARC, C1 is waypoints 1001, 1002, 1007,
// 1008, and every three waypoints form a triangle or a collinear pattern.
// Times are written with f.tick, a 60th of game.scan_window (1 s with
// config.toml as shipped), or relative to f.rules.Limit() (window + grace,
// 65 s as shipped), so the scenarios hold for any config. Times in comments
// are the shipped values, after the fake clock's start.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"labyrinth/internal/atlas"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// square is a square in the 2025 seed (T6, R, SARC, C1).
var square = []string{"1001", "1002", "1007", "1008"}

// start is the fake clock's starting time.
var start = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// clock is a fake teamstate.Clock that tests advance by hand. Timers fire
// only when at moves time past them, in order, never from inside AfterFunc
// (the store calls AfterFunc holding a team's lock, and the timer's callback
// takes that lock).
type clock struct {
	mu     sync.Mutex
	t      time.Time
	timers []*fakeTimer
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

// seedMaxPoints is the most waypoints in any 2025 seed pattern. With a
// smaller game.max_pattern_points the seed can't load, so these tests skip.
const seedMaxPoints = 4

// minTestTeam is the team size these tests are written for: they use up to
// six distinct members (the hexagon needs all six). With a smaller teams.size
// in config.toml they skip rather than fail.
const minTestTeam = 6

// fixtureEnv is a seeded database with one team, its stores and a fake clock.
type fixtureEnv struct {
	atlas  *atlas.Atlas
	db     *postgres.DB
	dbURL  string
	teams  *teams.Store
	state  *teamstate.Store
	hub    *live.Hub
	rules  teamstate.Rules
	clock  *clock
	teamID string
	users  []uuid.UUID
}

// fixtureOptions adjust started.
type fixtureOptions struct {
	// lobby leaves the team in the lobby instead of starting it.
	lobby bool
	// sql runs after the seed and before teamstate.Load, e.g. to add content.
	sql string
	// rules adjusts the rules from config.toml.
	rules func(*teamstate.Rules)
}

// started seeds the dev content and returns one IN_GAME team of teams.size
// players, adjusted by opts.
func started(t *testing.T, opts ...fixtureOptions) *fixtureEnv {
	t.Helper()
	var o fixtureOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	ctx := context.Background()
	set := configtest.Settings(t)
	if set.Game.MaxPatternPoints < seedMaxPoints {
		t.Skipf("the 2025 seed has %d-point patterns; game.max_pattern_points is %d", seedMaxPoints, set.Game.MaxPatternPoints)
	}
	if set.Teams.Size < minTestTeam {
		t.Skipf("teamstate tests need teams.size >= %d", minTestTeam)
	}
	dbURL := pgtest.URL(t, "teamstate_test")
	db := pgtest.Open(t, dbURL)
	if err := db.SeedDev2025(ctx); err != nil {
		t.Fatal(err)
	}
	if o.sql != "" {
		if _, err := db.Pool().Exec(ctx, o.sql); err != nil {
			t.Fatal(err)
		}
	}
	users := user.New(db)
	var ids []uuid.UUID
	for i := range set.Teams.Size {
		u := pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{
			Subject: fmt.Sprintf("s%d", i), Email: fmt.Sprintf("u%d@snu.edu.in", i), Name: fmt.Sprintf("U%d", i),
		}))
		ids = append(ids, u.ID)
	}

	hub := live.NewHub(set.Live.HubShards, set.Live.SubscriberBuffer)
	ts := pgtest.Must(teams.Load(ctx, db, hub, pgtest.Quiet, teams.Rules{
		Size: set.Teams.Size, NameMaxLength: set.Teams.NameMaxLength,
		WriteTimeout: set.Storage.WriteTimeout.Duration, CodeAttempts: set.Teams.CodeAttempts,
	}))
	team := pgtest.Must(ts.Create(ctx, ids[0], "Scanners"))
	if !o.lobby {
		for _, u := range ids[1:] {
			pgtest.Must(ts.Join(ctx, u, team.ID))
			pgtest.Must(ts.SetReady(ctx, u, true))
		}
		pgtest.Must(ts.Start(ctx, ids[0]))
	}
	rules := teamstate.Rules{
		ScanWindow:       set.Game.ScanWindow.Duration,
		ScanGrace:        set.Game.ScanGrace.Duration,
		MaxPatternPoints: set.Game.MaxPatternPoints,
		WriteTimeout:     set.Storage.WriteTimeout.Duration,
	}
	if o.rules != nil {
		o.rules(&rules)
	}
	clk := &clock{t: start}
	rules.Clock = clk
	at := pgtest.Must(atlas.Load(ctx, db, pgtest.Quiet, atlas.Rules{MaxPatternPoints: rules.MaxPatternPoints}))
	st := pgtest.Must(teamstate.Load(ctx, db, at, ts, hub, pgtest.Quiet, rules))
	return &fixtureEnv{atlas: at, db: db, dbURL: dbURL, teams: ts, state: st, hub: hub, rules: rules, clock: clk, teamID: team.ID, users: ids}
}

// tick returns n sixtieths of game.scan_window: n seconds with config.toml's
// shipped 1m window. Scenario times scale with the window, so they keep the
// same meaning under any config.
func (f *fixtureEnv) tick(n float64) time.Duration {
	return time.Duration(n * float64(f.rules.ScanWindow) / 60)
}

// scanAt scans waypoint as member i at d after start, failing on error.
func (f *fixtureEnv) scanAt(t *testing.T, d time.Duration, i int, waypoint string) teamstate.ScanResult {
	t.Helper()
	f.clock.at(d)
	res, err := f.state.Scan(context.Background(), f.users[i], waypoint)
	if err != nil {
		t.Fatalf("member %d scanning %s at %v: %v", i, waypoint, d, err)
	}
	return res
}

// scanErrAt scans waypoint as member i at d after start and returns the error.
func (f *fixtureEnv) scanErrAt(d time.Duration, i int, waypoint string) error {
	f.clock.at(d)
	_, err := f.state.Scan(context.Background(), f.users[i], waypoint)
	return err
}

// selectAt selects shape as member i at d after start, failing on error.
func (f *fixtureEnv) selectAt(t *testing.T, d time.Duration, i int, shape string) teamstate.View {
	t.Helper()
	f.clock.at(d)
	v, err := f.state.Select(context.Background(), f.users[i], shape)
	if err != nil {
		t.Fatalf("member %d selecting %s: %v", i, shape, err)
	}
	return v
}

// view returns the team's current view.
func (f *fixtureEnv) view(t *testing.T) teamstate.View {
	t.Helper()
	return pgtest.Must(f.state.ForUser(f.users[0]))
}

// loggedScans counts the team's team_scan rows.
func (f *fixtureEnv) loggedScans(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM team_scan WHERE team_id = $1", f.teamID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// stackWaypoints returns the waypoint IDs a view shows in the stack, bottom
// to top.
func stackWaypoints(v teamstate.View) []string {
	ids := []string{}
	for _, s := range v.Stack {
		ids = append(ids, s.WaypointID)
	}
	return ids
}

// expectOutcome fails unless res has the outcome.
func expectOutcome(t *testing.T, what string, res teamstate.ScanResult, outcome string) {
	t.Helper()
	if res.Outcome != outcome {
		t.Fatalf("%s: outcome %q, want %q (stack %v)", what, res.Outcome, outcome, stackWaypoints(res.State))
	}
}

// TestWritesRequireGame checks that selecting, clearing and scanning are
// refused outside a started team, and that input is validated first.
func TestWritesRequireGame(t *testing.T) {
	f := started(t, fixtureOptions{lobby: true})
	ctx := context.Background()

	if _, err := f.state.Scan(ctx, f.users[0], "9999"); !errors.Is(err, teamstate.ErrUnknownWaypoint) {
		t.Fatalf("unknown waypoint: %v", err)
	}
	if _, err := f.state.Select(ctx, f.users[0], "hexagon"); !errors.Is(err, teamstate.ErrUnknownShape) {
		t.Fatalf("unknown shape: %v", err)
	}
	if _, err := f.state.Scan(ctx, uuid.New(), "1001"); !errors.Is(err, teamstate.ErrNotInTeam) {
		t.Fatalf("no team: %v", err)
	}
	if _, err := f.state.Scan(ctx, f.users[0], "1001"); !errors.Is(err, teamstate.ErrNotInGame) {
		t.Fatalf("lobby scan: %v", err)
	}
	if _, err := f.state.Select(ctx, f.users[0], "square"); !errors.Is(err, teamstate.ErrNotInGame) {
		t.Fatalf("lobby select: %v", err)
	}
	if _, err := f.state.ClearSelection(ctx, f.users[0]); !errors.Is(err, teamstate.ErrNotInGame) {
		t.Fatalf("lobby clear: %v", err)
	}
}

// TestScanNeedsSelection checks that scanning before selecting a shape is
// refused and changes nothing.
func TestScanNeedsSelection(t *testing.T) {
	f := started(t)
	if err := f.scanErrAt(0, 0, "1001"); !errors.Is(err, teamstate.ErrNoShapeSelected) {
		t.Fatalf("scan without selection: %v", err)
	}
	if v := f.view(t); len(v.Waypoints) != 0 || len(v.Stack) != 0 || f.loggedScans(t) != 0 {
		t.Fatalf("refused scan changed state: %+v", v)
	}
}

// TestSquareNeedsDistinctMembers walks a square to completion: a member can't
// scan a second waypoint and a waypoint can't be scanned by a second member
// (rejections change nothing), three waypoints forming a triangle don't
// complete the selected square, and the fourth completes it and clears the
// selection and stack.
func TestSquareNeedsDistinctMembers(t *testing.T) {
	f := started(t)

	if v := f.selectAt(t, 0, 5, "square"); v.Selection == nil || v.Selection.SelectedBy != f.users[5] {
		t.Fatalf("selection: %+v", v.Selection)
	}
	res := f.scanAt(t, f.tick(1), 0, "1001")
	expectOutcome(t, "first scan", res, teamstate.OutcomeScanAdded)
	if !res.State.Stack[0].ExpiresAt.Equal(start.Add(f.tick(1) + f.rules.Limit())) {
		t.Fatalf("expires_at %v", res.State.Stack[0].ExpiresAt)
	}
	f.scanAt(t, f.tick(5), 1, "1002")

	if err := f.scanErrAt(f.tick(6), 0, "1007"); !errors.Is(err, teamstate.ErrMemberAlreadyScanned) {
		t.Fatalf("same member, another waypoint: %v", err)
	}
	if err := f.scanErrAt(f.tick(7), 2, "1001"); !errors.Is(err, teamstate.ErrWaypointAlreadyScanned) {
		t.Fatalf("another member, same waypoint: %v", err)
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"1001", "1002"}) || f.loggedScans(t) != 2 {
		t.Fatalf("rejections changed the stack: %v", got)
	}

	// 1001, 1002, 1007 is a triangle, but the team selected a square.
	expectOutcome(t, "third scan", f.scanAt(t, f.tick(10), 2, "1007"), teamstate.OutcomeScanAdded)
	res = f.scanAt(t, f.tick(12), 3, "1008")
	expectOutcome(t, "fourth scan", res, teamstate.OutcomePatternCompleted)
	if !slices.Equal(res.Pattern, square) || res.AlreadyCompleted {
		t.Fatalf("completion: %+v", res)
	}
	st := res.State
	if st.Selection != nil || len(st.Stack) != 0 || !slices.Equal(st.Shapes, []string{"square"}) ||
		len(st.Patterns) != 1 || !slices.Equal(st.Waypoints, square) {
		t.Fatalf("after completion: %+v", st)
	}
	if err := f.scanErrAt(f.tick(13), 4, "1009"); !errors.Is(err, teamstate.ErrNoShapeSelected) {
		t.Fatalf("selection should clear after completion: %v", err)
	}
}

// TestEverySelectionPurges checks that every selection empties the stack,
// reselecting the same shape included, so scans from an earlier attempt never
// count toward a new one.
func TestEverySelectionPurges(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")
	for i, w := range square[:3] {
		f.scanAt(t, f.tick(float64(i+1)), i, w)
	}

	if v := f.selectAt(t, f.tick(4), 1, "square"); len(v.Stack) != 0 || v.Selection.SelectedBy != f.users[1] {
		t.Fatalf("reselecting the same shape should purge: %+v", v)
	}
	res := f.scanAt(t, f.tick(5), 3, "1008") // without the purge this would complete the square
	expectOutcome(t, "after reselect", res, teamstate.OutcomeScanAdded)
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"1008"}) {
		t.Fatalf("stack after reselect: %v", got)
	}
	// Members who scanned before the purge may scan again at once.
	expectOutcome(t, "member 0 after purge", f.scanAt(t, f.tick(6), 0, "1001"), teamstate.OutcomeScanAdded)

	if v := f.selectAt(t, f.tick(7), 2, "triangle"); len(v.Stack) != 0 || v.Selection.Shape != "triangle" {
		t.Fatalf("selecting another shape should purge: %+v", v)
	}
}

// TestRescanIsRejected checks that scanning a waypoint already in the stack
// is rejected and not pushed, whoever scans it (the same member's retry
// included): nothing logged, no event.
func TestRescanIsRejected(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, f.tick(1), 0, "1001")

	sub := f.hub.Subscribe(f.users[2])
	defer sub.Close()
	for _, member := range []int{0, 1} {
		if err := f.scanErrAt(f.tick(2), member, "1001"); !errors.Is(err, teamstate.ErrWaypointAlreadyScanned) {
			t.Fatalf("member %d rescanning 1001: %v", member, err)
		}
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"1001"}) || f.loggedScans(t) != 1 {
		t.Fatalf("rescans changed state: %v, %d logged", got, f.loggedScans(t))
	}
	select {
	case ev := <-sub.C:
		t.Fatalf("rejected rescan published %s", ev.Name())
	default:
	}
}

// TestConcurrentRescansAddOneScan checks that many simultaneous copies of one
// scan (a flaky network retrying) push exactly one scan.
func TestConcurrentRescansAddOneScan(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")

	const copies = 8
	errs := make([]error, copies)
	var wg sync.WaitGroup
	for i := range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.state.Scan(context.Background(), f.users[0], "1001")
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, teamstate.ErrWaypointAlreadyScanned):
			t.Fatalf("unexpected error %v", err)
		}
	}
	if ok != 1 || f.loggedScans(t) != 1 {
		t.Fatalf("%d accepted, %d logged; want 1 and 1", ok, f.loggedScans(t))
	}
}

// TestLateRepeatDoesNotRejectThePattern checks that when one member's scan
// goes stale before the pattern is finished, only that scan is dropped: the
// member rescans the same waypoint and the pattern completes.
func TestLateRepeatDoesNotRejectThePattern(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, 0, 0, "1001")
	f.scanAt(t, f.tick(10), 1, "1002")
	f.scanAt(t, f.tick(20), 2, "1007")

	// At limit + 5 (70 s), member 0's scan (70 s old) is stale: dropped,
	// nothing else.
	limit := f.rules.Limit()
	res := f.scanAt(t, limit+f.tick(5), 3, "1008")
	expectOutcome(t, "fourth scan after the first went stale", res, teamstate.OutcomeScanAdded)
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"1002", "1007", "1008"}) {
		t.Fatalf("stack: %v", got)
	}

	// Member 0 scans 1001 again at limit + 6 (71 s): allowed, since the old
	// scan is stale; not rejected. Span 1002@10 to 1001@71 is 61 s, within
	// the limit.
	res = f.scanAt(t, limit+f.tick(6), 0, "1001")
	expectOutcome(t, "rescan", res, teamstate.OutcomePatternCompleted)
}

// TestGraceTolerance checks the window and its grace: spans up to window +
// grace complete, and a microsecond more drops the bottom scan.
func TestGraceTolerance(t *testing.T) {
	f := started(t)
	limit := f.rules.Limit()
	cases := []struct {
		name string
		span time.Duration
		want string
	}{
		{"within the window", f.rules.ScanWindow - f.tick(1), teamstate.OutcomePatternCompleted},
		{"over the window, inside the grace (don't punish)", f.rules.ScanWindow + f.rules.ScanGrace/2, teamstate.OutcomePatternCompleted},
		{"exactly window + grace", limit, teamstate.OutcomePatternCompleted},
		{"one microsecond over", limit + time.Microsecond, teamstate.OutcomeScanAdded},
	}
	t0 := time.Duration(0)
	for _, c := range cases {
		f.selectAt(t, t0, 0, "square") // purges the previous attempt
		f.scanAt(t, t0, 0, square[0])
		f.scanAt(t, t0+c.span/3, 1, square[1])
		f.scanAt(t, t0+2*c.span/3, 2, square[2])
		expectOutcome(t, c.name, f.scanAt(t, t0+c.span, 3, square[3]), c.want)
		t0 += 2 * limit // members' earlier scans are long stale
	}
}

// TestStaleScanFreesItsWaypointAndMember checks that once a scan goes stale
// it's dropped, so another member can scan its waypoint, and its member can
// scan again.
func TestStaleScanFreesItsWaypointAndMember(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, 0, 0, "1001")
	f.scanAt(t, f.tick(10), 1, "1002")

	limit := f.rules.Limit()
	res := f.scanAt(t, limit+f.tick(1), 2, "1001") // 66 s: member 0's 1001 is stale
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"1002", "1001"}) {
		t.Fatalf("stack: %v", got)
	}
	f.scanAt(t, limit+f.tick(2), 0, "1007") // member 0 is free again
	expectOutcome(t, "complete", f.scanAt(t, limit+f.tick(3), 3, "1008"), teamstate.OutcomePatternCompleted)
}

// TestWrongScanLocksMember checks that a waypoint that can't be part of any
// pattern of the selected shape, given the stack, is still pushed (marked
// wrong), and that it occupies the member's one scan and its waypoint slot.
func TestWrongScanLocksMember(t *testing.T) {
	f := started(t, fixtureOptions{sql: `
		INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('1012', 'Nowhere', 0, 0);`})
	f.selectAt(t, 0, 0, "square")

	res := f.scanAt(t, f.tick(1), 0, "1012") // in no square
	expectOutcome(t, "wrong waypoint", res, teamstate.OutcomeScanAdded)
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"1012"}) {
		t.Fatalf("stack %v", got)
	}
	if st := f.state.ForTeam(f.teamID); !st.Stack.At(0).Wrong {
		t.Fatal("wrong scan not marked wrong")
	}

	// The member is locked: they can't scan again to overwrite it.
	if err := f.scanErrAt(f.tick(2), 0, "1001"); !errors.Is(err, teamstate.ErrMemberAlreadyScanned) {
		t.Fatalf("member rescanned after a wrong scan: %v", err)
	}
	// The waypoint slot is taken too.
	if err := f.scanErrAt(f.tick(3), 1, "1012"); !errors.Is(err, teamstate.ErrWaypointAlreadyScanned) {
		t.Fatalf("another member rescanned the wrong waypoint: %v", err)
	}
}

// TestWrongScanPoisonsSelection checks that once a wrong scan is in the stack,
// no later scan can complete the selection, and that reselecting purges it.
func TestWrongScanPoisonsSelection(t *testing.T) {
	f := started(t, fixtureOptions{sql: `
		INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('1012', 'Nowhere', 0, 0);`})
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, f.tick(1), 0, "1001")
	f.scanAt(t, f.tick(2), 1, "1002")
	// 1003 is in no square with 1001 and 1002: wrong, and it poisons the set.
	expectOutcome(t, "poisoning scan", f.scanAt(t, f.tick(3), 2, "1003"), teamstate.OutcomeScanAdded)

	// The remaining correct waypoints still can't complete it.
	f.scanAt(t, f.tick(4), 3, "1007")
	expectOutcome(t, "cannot complete a poisoned selection", f.scanAt(t, f.tick(5), 4, "1008"), teamstate.OutcomeScanAdded)

	// Reselecting purges the wrong scan; the square then completes.
	f.selectAt(t, f.tick(6), 0, "square")
	res := teamstate.ScanResult{}
	for i, w := range square {
		res = f.scanAt(t, f.tick(7)+f.tick(float64(i)), i, w)
	}
	expectOutcome(t, "completes after reselect", res, teamstate.OutcomePatternCompleted)
}

// TestWrongScanNeverAgesOut checks that a wrong scan stays in the stack past
// the scan limit, while a correct scan of the same age goes stale.
func TestWrongScanNeverAgesOut(t *testing.T) {
	f := started(t, fixtureOptions{sql: `
		INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('1012', 'Nowhere', 0, 0);`})
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, 0, 0, "1001")
	f.scanAt(t, f.tick(1), 1, "1012") // wrong

	limit := f.rules.Limit()
	res := f.scanAt(t, limit+f.tick(10), 2, "1007")
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"1012", "1007"}) {
		t.Fatalf("stack %v: the wrong 1012 should stay, the stale 1001 should drop", got)
	}

	// A restart reproduces the wrong scan and its never-stale flag.
	wp, _ := f.atlas.WaypointIndex("1012")
	restarted := pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.teams, f.hub, pgtest.Quiet, f.rules))
	st := restarted.ForTeam(f.teamID)
	wrong := false
	for i := range st.Stack.Len() {
		if sc := st.Stack.At(i); sc.Waypoint == wp {
			wrong = sc.Wrong
		}
	}
	if !wrong {
		t.Fatal("wrong flag lost across restart")
	}
}

// TestCompletionPurges checks that completing purges the stack and the
// selection: a rescan of the completing waypoint, or any other scan, needs a
// new selection, after which every member may scan again.
func TestCompletionPurges(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")
	for i, w := range square {
		f.scanAt(t, f.tick(float64(i)), i, w)
	}
	if v := f.view(t); v.Selection != nil || len(v.Stack) != 0 {
		t.Fatalf("not purged: %+v", v)
	}
	for _, c := range []struct {
		member   int
		waypoint string
	}{{3, "1008"}, {4, "1009"}} {
		if err := f.scanErrAt(f.tick(4), c.member, c.waypoint); !errors.Is(err, teamstate.ErrNoShapeSelected) {
			t.Fatalf("member %d scanning %s after completion: %v", c.member, c.waypoint, err)
		}
	}
	f.selectAt(t, f.tick(5), 0, "square")
	expectOutcome(t, "after a new selection", f.scanAt(t, f.tick(6), 3, "1008"), teamstate.OutcomeScanAdded)
}

// TestOutOfOrderScanning checks that a pattern completes whatever order the
// waypoints are scanned in. A pattern is an unordered set of waypoints (a
// WaypointSet bitmask): scanning 1001 then 1007 and scanning 1007 then 1001
// set the same bits, so the same pattern is found.
func TestOutOfOrderScanning(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")

	order := []string{"1008", "1007", "1002", "1001"} // square, scrambled
	res := teamstate.ScanResult{}
	for i, w := range order {
		res = f.scanAt(t, f.tick(float64(i)), i, w)
	}
	expectOutcome(t, "completes out of order", res, teamstate.OutcomePatternCompleted)
	if !slices.Equal(res.Pattern, square) { // Pattern is returned sorted
		t.Fatalf("pattern %v", res.Pattern)
	}
}

// TestSinglePointPattern checks that a 1-waypoint pattern completes on its
// only scan.
func TestSinglePointPattern(t *testing.T) {
	f := started(t, fixtureOptions{sql: `
		INSERT INTO shape (id, name) VALUES ('dot', 'Dot');
		INSERT INTO pattern (id, shape_id) VALUES (1001, 'dot');
		INSERT INTO pattern_waypoint (pattern_id, waypoint_id) VALUES (1001, '1011');`})
	f.selectAt(t, 0, 0, "dot")
	res := f.scanAt(t, f.tick(1), 0, "1011")
	expectOutcome(t, "single point", res, teamstate.OutcomePatternCompleted)
	if !slices.Equal(res.Pattern, []string{"1011"}) {
		t.Fatalf("pattern %v", res.Pattern)
	}
}

// TestSixPointPatternAtCapacity checks that a pattern with
// max_pattern_points waypoints completes exactly as the stack fills.
func TestSixPointPatternAtCapacity(t *testing.T) {
	hex := []string{"1003", "1004", "1005", "1006", "1009", "1010"}
	f := started(t, fixtureOptions{sql: `
		INSERT INTO shape (id, name) VALUES ('hexagon', 'Hexagon');
		INSERT INTO pattern (id, shape_id) VALUES (1002, 'hexagon');
		INSERT INTO pattern_waypoint (pattern_id, waypoint_id) VALUES
			(1002, '1003'), (1002, '1004'), (1002, '1005'), (1002, '1006'), (1002, '1009'), (1002, '1010');`})
	if f.rules.MaxPatternPoints != len(hex) {
		t.Skipf("test assumes max_pattern_points = %d", len(hex))
	}
	f.selectAt(t, 0, 0, "hexagon")
	for i, w := range hex {
		want := teamstate.OutcomeScanAdded
		if i == len(hex)-1 {
			want = teamstate.OutcomePatternCompleted
		}
		expectOutcome(t, fmt.Sprintf("hexagon point %d", i+1), f.scanAt(t, f.tick(float64(i)), i, w), want)
	}
}

// TestCompletingAgain checks that completing an already completed pattern
// reports it and doesn't grow progress.
func TestCompletingAgain(t *testing.T) {
	f := started(t)
	for round := range 2 {
		t0 := time.Duration(round) * 2 * f.rules.Limit()
		f.selectAt(t, t0, 0, "square")
		for i, w := range square {
			res := f.scanAt(t, t0+f.tick(float64(i)), i, w)
			if i == 3 && res.AlreadyCompleted != (round == 1) {
				t.Fatalf("round %d: already_completed = %v", round, res.AlreadyCompleted)
			}
		}
	}
	if v := f.view(t); len(v.Patterns) != 1 {
		t.Fatalf("patterns after completing twice: %v", v.Patterns)
	}
}

// TestClearSelection checks clearing with and without a selection, and that
// visited waypoints survive it.
func TestClearSelection(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	if v := pgtest.Must(f.state.ClearSelection(ctx, f.users[0])); v.Selection != nil {
		t.Fatalf("clear with nothing selected: %+v", v)
	}
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, f.tick(1), 0, "1001")
	v := pgtest.Must(f.state.ClearSelection(ctx, f.users[3]))
	if v.Selection != nil || len(v.Stack) != 0 || !slices.Equal(v.Waypoints, []string{"1001"}) {
		t.Fatalf("after clear: %+v", v)
	}
	if err := f.scanErrAt(f.tick(2), 1, "1002"); !errors.Is(err, teamstate.ErrNoShapeSelected) {
		t.Fatalf("scan after clear: %v", err)
	}
}

// TestRestartRestoresEverything checks that progress, the selection and the
// live stack (rebuilt from team_scan) reload exactly, and that rescans are
// still rejected after the restart.
func TestRestartRestoresEverything(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")
	for i, w := range square {
		f.scanAt(t, f.tick(float64(i)), i, w)
	}
	later := f.rules.Limit() + f.tick(35) // 100 s: long after the square
	f.selectAt(t, later, 4, "triangle")
	f.scanAt(t, later, 4, "1003")
	f.scanAt(t, later+f.tick(1), 5, "1004")
	f.clock.at(later + f.tick(2))

	before := pgtest.Must(json.Marshal(f.view(t)))
	restarted := pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.teams, f.hub, pgtest.Quiet, f.rules))
	after := pgtest.Must(json.Marshal(pgtest.Must(restarted.ForUser(f.users[0]))))
	if string(after) != string(before) {
		t.Fatalf("state changed across restart\nbefore: %s\nafter:  %s", before, after)
	}
	if _, err := restarted.Scan(context.Background(), f.users[5], "1004"); !errors.Is(err, teamstate.ErrWaypointAlreadyScanned) {
		t.Fatalf("rescan after restart: %v", err)
	}
}

// TestConcurrentScans races two members at the same waypoint, and one member
// at two waypoints: in each race exactly one scan may be accepted.
func TestConcurrentScans(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")

	race := func(scans [2]func() error) (ok int, errs []error) {
		var wg sync.WaitGroup
		results := make([]error, 2)
		for i, scan := range scans {
			wg.Add(1)
			go func() { defer wg.Done(); results[i] = scan() }()
		}
		wg.Wait()
		for _, err := range results {
			if err == nil {
				ok++
			} else {
				errs = append(errs, err)
			}
		}
		return ok, errs
	}
	scan := func(i int, w string) func() error {
		return func() error { _, err := f.state.Scan(context.Background(), f.users[i], w); return err }
	}

	ok, errs := race([2]func() error{scan(0, "1001"), scan(1, "1001")})
	if ok != 1 || !errors.Is(errs[0], teamstate.ErrWaypointAlreadyScanned) {
		t.Fatalf("two members, one waypoint: %d ok, %v", ok, errs)
	}
	ok, errs = race([2]func() error{scan(2, "1002"), scan(2, "1007")})
	if ok != 1 || !errors.Is(errs[0], teamstate.ErrMemberAlreadyScanned) {
		t.Fatalf("one member, two waypoints: %d ok, %v", ok, errs)
	}
}

// TestFailedWriteReconciles checks that when memory and Postgres disagree (the
// selection was deleted by hand), a failed write reloads the team's state
// from Postgres and applies nothing.
func TestFailedWriteReconciles(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, f.tick(1), 0, "1001")

	conn := pgtest.Must(pgx.Connect(ctx, f.dbURL))
	if _, err := conn.Exec(ctx, "DELETE FROM team_selection WHERE team_id = $1", f.teamID); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	if _, err := f.state.ClearSelection(ctx, f.users[1]); err == nil {
		t.Fatal("clearing a selection deleted from Postgres should fail")
	}
	v := f.view(t)
	if v.Selection != nil || len(v.Stack) != 0 || !slices.Equal(v.Waypoints, []string{"1001"}) {
		t.Fatalf("not reconciled from Postgres: %+v", v)
	}
	f.selectAt(t, f.tick(2), 1, "square")
}

// TestReadsDoNotTouchDatabase checks that reads keep working with the
// database closed, and that a failed write leaves memory unchanged.
func TestReadsDoNotTouchDatabase(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, f.tick(1), 0, "1001")
	f.db.Close()

	if v := f.view(t); v.Selection == nil || !slices.Equal(stackWaypoints(v), []string{"1001"}) {
		t.Fatalf("read after DB close: %+v", v)
	}
	if err := f.scanErrAt(f.tick(2), 1, "1002"); err == nil {
		t.Fatal("scan with the database closed should fail")
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"1001"}) {
		t.Fatalf("failed write changed memory: %v", got)
	}
}

// TestStateIsPublished checks that selections and scans reach teammates as
// team_state events.
func TestStateIsPublished(t *testing.T) {
	f := started(t)
	sub := f.hub.Subscribe(f.users[3])
	defer sub.Close()

	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, f.tick(1), 0, "1010")
	var sel, scanned teamstate.View
	for _, v := range []*teamstate.View{&sel, &scanned} {
		ev := <-sub.C
		if ev.Name() != "team_state" {
			t.Fatalf("event %q", ev.Name())
		}
		if err := json.Unmarshal(ev.Data(), v); err != nil {
			t.Fatal(err)
		}
	}
	if sel.Selection == nil || sel.Selection.Shape != "square" || !slices.Equal(stackWaypoints(scanned), []string{"1010"}) {
		t.Fatalf("events: %+v / %+v", sel, scanned)
	}
}

// TestPublishTeam checks that PublishTeam sends the team's current state to
// every member without changing it (the empty state right after a start,
// then the state after a selection), and publishes nothing for a team still
// in the lobby or a team that doesn't exist.
func TestPublishTeam(t *testing.T) {
	f := started(t)
	subs := make([]*live.Subscription, len(f.users))
	for i, u := range f.users {
		subs[i] = f.hub.Subscribe(u)
		defer subs[i].Close()
	}

	f.state.PublishTeam(f.teamID)
	for i, sub := range subs {
		v := nextState(t, sub)
		if v.TeamID != f.teamID || v.Selection != nil || len(v.Stack) != 0 || len(v.Waypoints) != 0 {
			t.Fatalf("member %d: state after start %+v, want empty", i, v)
		}
	}

	f.selectAt(t, 0, 0, "square")
	nextState(t, subs[0]) // the selection's own event
	f.state.PublishTeam(f.teamID)
	if v := nextState(t, subs[0]); v.Selection == nil || v.Selection.Shape != "square" {
		t.Fatalf("republished state %+v, want the square selected", v)
	}
	if got, _ := json.Marshal(f.view(t)); f.loggedScans(t) != 0 || !strings.Contains(string(got), `"square"`) {
		t.Fatalf("PublishTeam changed state: %s", got)
	}

	f.state.PublishTeam("000000") // no such team
	lobby := started(t, fixtureOptions{lobby: true})
	lobbySub := lobby.hub.Subscribe(lobby.users[0])
	defer lobbySub.Close()
	lobby.state.PublishTeam(lobby.teamID)
	select {
	case ev := <-subs[0].C:
		t.Fatalf("unknown team published %s", ev.Name())
	case ev := <-lobbySub.C:
		t.Fatalf("lobby team published %s", ev.Name())
	default:
	}
}

// nextState waits briefly for the next team_state event on sub and decodes
// it, failing if none arrives.
func nextState(t *testing.T, sub *live.Subscription) teamstate.View {
	t.Helper()
	select {
	case ev := <-sub.C:
		if ev.Name() != "team_state" {
			t.Fatalf("event %q, want team_state", ev.Name())
		}
		var v teamstate.View
		if err := json.Unmarshal(ev.Data(), &v); err != nil {
			t.Fatal(err)
		}
		return v
	case <-time.After(time.Second):
		t.Fatal("no team_state event")
	}
	return teamstate.View{}
}

// TestStaleScanIsPublished checks that when a correct scan goes stale, with
// nobody acting, the store drops it and publishes team_state at once: the
// first instant it's stale (limit + 1 µs), not before; then the next scan's
// expiry is armed, and nothing is left pending once the stack is empty.
func TestStaleScanIsPublished(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, 0, 0, "1001")
	f.scanAt(t, f.tick(10), 1, "1002")
	sub := f.hub.Subscribe(f.users[4])
	defer sub.Close()

	limit := f.rules.Limit()
	f.clock.at(limit) // exactly limit old: still live, nothing sent
	select {
	case ev := <-sub.C:
		t.Fatalf("published %s before the scan was stale", ev.Name())
	default:
	}
	f.clock.at(limit + time.Microsecond)
	if got := stackWaypoints(nextState(t, sub)); !slices.Equal(got, []string{"1002"}) {
		t.Fatalf("after the first expiry: %v", got)
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"1002"}) {
		t.Fatalf("memory after the first expiry: %v", got)
	}
	if f.clock.pending() != 1 {
		t.Fatalf("%d timers pending, want 1 (for 1002)", f.clock.pending())
	}

	f.clock.at(f.tick(10) + limit + time.Microsecond)
	if got := stackWaypoints(nextState(t, sub)); len(got) != 0 {
		t.Fatalf("after the second expiry: %v", got)
	}
	if f.clock.pending() != 0 {
		t.Fatalf("%d timers pending on an empty stack", f.clock.pending())
	}
	// Both members are free again, with the selection still active.
	f.scanAt(t, f.tick(10)+limit+f.tick(1), 0, "1001")
}

// TestExpiryTimerFollowsWrites checks that every write re-arms or cancels the
// team's timer: one per team, none after a cast or a purge, and none for a
// stack holding only wrong scans (they never go stale).
func TestExpiryTimerFollowsWrites(t *testing.T) {
	f := started(t, fixtureOptions{sql: `
		INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('1012', 'Nowhere', 0, 0);`})
	f.selectAt(t, 0, 0, "square")
	f.scanAt(t, f.tick(1), 0, "1001")
	f.scanAt(t, f.tick(2), 1, "1002")
	if n := f.clock.pending(); n != 1 {
		t.Fatalf("%d timers after two scans, want 1", n)
	}
	f.selectAt(t, f.tick(3), 0, "square") // purge
	if n := f.clock.pending(); n != 0 {
		t.Fatalf("%d timers after a purge", n)
	}
	f.scanAt(t, f.tick(4), 0, "1012") // wrong
	if n := f.clock.pending(); n != 0 {
		t.Fatalf("%d timers for a wrong-only stack", n)
	}
	f.selectAt(t, f.tick(5), 0, "square")
	for i, w := range square {
		f.scanAt(t, f.tick(6+float64(i)), i, w)
	}
	if n := f.clock.pending(); n != 0 {
		t.Fatalf("%d timers after a cast", n)
	}
}

// TestLastCast checks that a cast sets last_cast (pattern, shape, who, when),
// that a new selection keeps it, that a repeat cast is marked
// already_completed, that every cast is logged in team_cast, and that a
// restart reloads the newest one.
func TestLastCast(t *testing.T) {
	f := started(t)
	if v := f.view(t); v.LastCast != nil {
		t.Fatalf("last_cast before any cast: %+v", v.LastCast)
	}
	for round := range 2 {
		t0 := time.Duration(round) * 2 * f.rules.Limit()
		f.selectAt(t, t0, 0, "square")
		for i, w := range square {
			f.scanAt(t, t0+f.tick(float64(i)), i, w)
		}
		lc := f.view(t).LastCast
		if lc == nil || !slices.Equal(lc.Pattern, square) || lc.Shape != "square" || lc.CastBy != f.users[3] ||
			!lc.CastAt.Equal(start.Add(t0+f.tick(3))) || lc.AlreadyCompleted != (round == 1) {
			t.Fatalf("round %d: last_cast %+v", round, lc)
		}
	}
	f.selectAt(t, 5*f.rules.Limit(), 0, "triangle")
	if f.view(t).LastCast == nil {
		t.Fatal("a new selection cleared last_cast")
	}

	var casts int
	if err := f.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM team_cast WHERE team_id = $1", f.teamID).Scan(&casts); err != nil || casts != 2 {
		t.Fatalf("team_cast has %d rows (%v), want 2", casts, err)
	}
	restarted := pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.teams, f.hub, pgtest.Quiet, f.rules))
	before := pgtest.Must(json.Marshal(f.view(t).LastCast))
	after := pgtest.Must(json.Marshal(pgtest.Must(restarted.ForUser(f.users[0])).LastCast))
	if string(before) != string(after) {
		t.Fatalf("last_cast across restart\nbefore: %s\nafter:  %s", before, after)
	}
}
