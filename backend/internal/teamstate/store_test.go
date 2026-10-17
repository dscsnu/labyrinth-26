package teamstate

// Store-level tests against a real Postgres (TEST_DATABASE_URL), in schema
// teamstate_test, with the 2025 dev seed, config.toml's rules and a fake
// clock. In the seed the square T6, R, SARC, C1 is waypoints 1001, 1002, 1007,
// 1008, and every three waypoints form a triangle or a collinear pattern.
// Times are written with f.tick, a 60th of game.scan_window (1 s with
// config.toml as shipped), or relative to f.rules.limit() (window + grace,
// 65 s as shipped), so the scenarios hold for any config. Times in comments
// are the shipped values, after the fake clock's start.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teams"
	"labyrinth/internal/user"
)

// square is a square in the 2025 seed (T6, R, SARC, C1).
var square = []string{"1001", "1002", "1007", "1008"}

// start is the fake clock's starting time.
var start = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// clock is a fake clock tests set by hand.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// now returns the fake time.
func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// at sets the fake time to d after start.
func (c *clock) at(d time.Duration) {
	c.mu.Lock()
	c.t = start.Add(d)
	c.mu.Unlock()
}

// minTestTeam is the team size these tests are written for: they use up to
// six distinct members (the hexagon needs all six). With a smaller teams.size
// in config.toml they skip rather than fail.
const minTestTeam = 6

// fixtureEnv is a seeded database with one team, its stores and a fake clock.
type fixtureEnv struct {
	db     *postgres.DB
	dbURL  string
	teams  *teams.Store
	state  *Store
	hub    *live.Hub
	rules  Rules
	clock  *clock
	teamID string
	users  []uuid.UUID
}

// fixtureOptions adjust started.
type fixtureOptions struct {
	// lobby leaves the team in the lobby instead of starting it.
	lobby bool
	// sql runs after the seed and before Load, e.g. to add content.
	sql string
	// rules adjusts the rules from config.toml.
	rules func(*Rules)
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

	hub := live.NewHub(set.Live.SubscriberBuffer)
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
	rules := Rules{
		ScanWindow:       set.Game.ScanWindow.Duration,
		ScanGrace:        set.Game.ScanGrace.Duration,
		MaxPatternPoints: set.Game.MaxPatternPoints,
		WriteTimeout:     set.Storage.WriteTimeout.Duration,
	}
	if o.rules != nil {
		o.rules(&rules)
	}
	clk := &clock{t: start}
	st := pgtest.Must(Load(ctx, db, ts, hub, pgtest.Quiet, rules))
	st.now = clk.now
	return &fixtureEnv{db: db, dbURL: dbURL, teams: ts, state: st, hub: hub, rules: rules, clock: clk, teamID: team.ID, users: ids}
}

// tick returns n sixtieths of game.scan_window: n seconds with config.toml's
// shipped 1m window. Scenario times scale with the window, so they keep the
// same meaning under any config.
func (f *fixtureEnv) tick(n float64) time.Duration {
	return time.Duration(n * float64(f.rules.ScanWindow) / 60)
}

// scanAt scans waypoint as member i at d after start, failing on error.
func (f *fixtureEnv) scanAt(t *testing.T, d time.Duration, i int, waypoint string) ScanResult {
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
func (f *fixtureEnv) selectAt(t *testing.T, d time.Duration, i int, shape string) View {
	t.Helper()
	f.clock.at(d)
	v, err := f.state.Select(context.Background(), f.users[i], shape)
	if err != nil {
		t.Fatalf("member %d selecting %s: %v", i, shape, err)
	}
	return v
}

// view returns the team's current view.
func (f *fixtureEnv) view(t *testing.T) View {
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
func stackWaypoints(v View) []string {
	ids := []string{}
	for _, s := range v.Stack {
		ids = append(ids, s.WaypointID)
	}
	return ids
}

// expectOutcome fails unless res has the outcome.
func expectOutcome(t *testing.T, what string, res ScanResult, outcome string) {
	t.Helper()
	if res.Outcome != outcome {
		t.Fatalf("%s: outcome %q, want %q (stack %v)", what, res.Outcome, outcome, stackWaypoints(res.State))
	}
}

// TestDevSeedLoads checks that the 2025 seed loads and validates under
// config.toml's rules, and can be applied twice.
func TestDevSeedLoads(t *testing.T) {
	f := started(t, fixtureOptions{lobby: true})
	c := f.state.Content()
	if len(c.Waypoints) != 11 || len(c.Shapes) != 5 || len(c.Patterns) != 359 {
		t.Fatalf("loaded %d waypoints, %d shapes, %d patterns", len(c.Waypoints), len(c.Shapes), len(c.Patterns))
	}
	set, _ := c.SetOf(square)
	if p, ok := c.PatternFor(set); !ok || c.Shapes[c.Patterns[p].Shape].ID != "square" {
		t.Fatalf("%v should be a square", square)
	}
	if err := f.db.SeedDev2025(context.Background()); err != nil {
		t.Fatalf("seeding again: %v", err)
	}
}

// TestWritesRequireGame checks that selecting, clearing and scanning are
// refused outside a started team, and that input is validated first.
func TestWritesRequireGame(t *testing.T) {
	f := started(t, fixtureOptions{lobby: true})
	ctx := context.Background()

	if _, err := f.state.Scan(ctx, f.users[0], "9999"); !errors.Is(err, ErrUnknownWaypoint) {
		t.Fatalf("unknown waypoint: %v", err)
	}
	if _, err := f.state.Select(ctx, f.users[0], "hexagon"); !errors.Is(err, ErrUnknownShape) {
		t.Fatalf("unknown shape: %v", err)
	}
	if _, err := f.state.Scan(ctx, uuid.New(), "1001"); !errors.Is(err, ErrNotInTeam) {
		t.Fatalf("no team: %v", err)
	}
	if _, err := f.state.Scan(ctx, f.users[0], "1001"); !errors.Is(err, ErrNotInGame) {
		t.Fatalf("lobby scan: %v", err)
	}
	if _, err := f.state.Select(ctx, f.users[0], "square"); !errors.Is(err, ErrNotInGame) {
		t.Fatalf("lobby select: %v", err)
	}
	if _, err := f.state.ClearSelection(ctx, f.users[0]); !errors.Is(err, ErrNotInGame) {
		t.Fatalf("lobby clear: %v", err)
	}
}

// TestScanNeedsSelection checks that scanning before selecting a shape is
// refused and changes nothing.
func TestScanNeedsSelection(t *testing.T) {
	f := started(t)
	if err := f.scanErrAt(0, 0, "1001"); !errors.Is(err, ErrNoShapeSelected) {
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
	expectOutcome(t, "first scan", res, OutcomeScanAdded)
	if !res.State.Stack[0].ExpiresAt.Equal(start.Add(f.tick(1) + f.rules.limit())) {
		t.Fatalf("expires_at %v", res.State.Stack[0].ExpiresAt)
	}
	f.scanAt(t, f.tick(5), 1, "1002")

	if err := f.scanErrAt(f.tick(6), 0, "1007"); !errors.Is(err, ErrMemberAlreadyScanned) {
		t.Fatalf("same member, another waypoint: %v", err)
	}
	if err := f.scanErrAt(f.tick(7), 2, "1001"); !errors.Is(err, ErrWaypointAlreadyScanned) {
		t.Fatalf("another member, same waypoint: %v", err)
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"1001", "1002"}) || f.loggedScans(t) != 2 {
		t.Fatalf("rejections changed the stack: %v", got)
	}

	// 1001, 1002, 1007 is a triangle, but the team selected a square.
	expectOutcome(t, "third scan", f.scanAt(t, f.tick(10), 2, "1007"), OutcomeScanAdded)
	res = f.scanAt(t, f.tick(12), 3, "1008")
	expectOutcome(t, "fourth scan", res, OutcomePatternCompleted)
	if !slices.Equal(res.Pattern, square) || res.AlreadyCompleted {
		t.Fatalf("completion: %+v", res)
	}
	st := res.State
	if st.Selection != nil || len(st.Stack) != 0 || !slices.Equal(st.Shapes, []string{"square"}) ||
		len(st.Patterns) != 1 || !slices.Equal(st.Waypoints, square) {
		t.Fatalf("after completion: %+v", st)
	}
	if err := f.scanErrAt(f.tick(13), 4, "1009"); !errors.Is(err, ErrNoShapeSelected) {
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
	expectOutcome(t, "after reselect", res, OutcomeScanAdded)
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"1008"}) {
		t.Fatalf("stack after reselect: %v", got)
	}
	// Members who scanned before the purge may scan again at once.
	expectOutcome(t, "member 0 after purge", f.scanAt(t, f.tick(6), 0, "1001"), OutcomeScanAdded)

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
		if err := f.scanErrAt(f.tick(2), member, "1001"); !errors.Is(err, ErrWaypointAlreadyScanned) {
			t.Fatalf("member %d rescanning 1001: %v", member, err)
		}
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"1001"}) || f.loggedScans(t) != 1 {
		t.Fatalf("rescans changed state: %v, %d logged", got, f.loggedScans(t))
	}
	select {
	case ev := <-sub.C:
		t.Fatalf("rejected rescan published %s", ev.Name)
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
		case !errors.Is(err, ErrWaypointAlreadyScanned):
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
	limit := f.rules.limit()
	res := f.scanAt(t, limit+f.tick(5), 3, "1008")
	expectOutcome(t, "fourth scan after the first went stale", res, OutcomeScanAdded)
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"1002", "1007", "1008"}) {
		t.Fatalf("stack: %v", got)
	}

	// Member 0 scans 1001 again at limit + 6 (71 s): allowed, since the old
	// scan is stale; not rejected. Span 1002@10 to 1001@71 is 61 s, within
	// the limit.
	res = f.scanAt(t, limit+f.tick(6), 0, "1001")
	expectOutcome(t, "rescan", res, OutcomePatternCompleted)
}

// TestGraceTolerance checks the window and its grace: spans up to window +
// grace complete, and a microsecond more drops the bottom scan.
func TestGraceTolerance(t *testing.T) {
	f := started(t)
	limit := f.rules.limit()
	cases := []struct {
		name string
		span time.Duration
		want string
	}{
		{"within the window", f.rules.ScanWindow - f.tick(1), OutcomePatternCompleted},
		{"over the window, inside the grace (don't punish)", f.rules.ScanWindow + f.rules.ScanGrace/2, OutcomePatternCompleted},
		{"exactly window + grace", limit, OutcomePatternCompleted},
		{"one microsecond over", limit + time.Microsecond, OutcomeScanAdded},
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

	limit := f.rules.limit()
	res := f.scanAt(t, limit+f.tick(1), 2, "1001") // 66 s: member 0's 1001 is stale
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"1002", "1001"}) {
		t.Fatalf("stack: %v", got)
	}
	f.scanAt(t, limit+f.tick(2), 0, "1007") // member 0 is free again
	expectOutcome(t, "complete", f.scanAt(t, limit+f.tick(3), 3, "1008"), OutcomePatternCompleted)
}

// TestOutsideScanIsRejected checks that a waypoint that can't be part of any
// pattern of the selected shape, given the stack, is rejected and not pushed;
// and that a waypoint in no pattern of the shape at all is rejected on an
// empty stack.
func TestOutsideScanIsRejected(t *testing.T) {
	f := started(t, fixtureOptions{sql: `
		INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('1012', 'Nowhere', 0, 0);`})
	c := f.state.Content()
	sq, _ := c.ShapeIndex("square")
	f.selectAt(t, 0, 0, "square")

	if err := f.scanErrAt(f.tick(1), 0, "1012"); !errors.Is(err, ErrNotInPattern) {
		t.Fatalf("waypoint in no square, empty stack: %v", err)
	}
	f.scanAt(t, f.tick(2), 0, "1001")
	f.scanAt(t, f.tick(3), 1, "1002")

	stack, _ := c.SetOf([]string{"1001", "1002"})
	var outside, inside []string
	for _, wp := range c.Waypoints {
		i, _ := c.WaypointIndex(wp.ID)
		if stack.Has(i) {
			continue
		}
		if c.FitsShape(sq, stack.With(i)) {
			inside = append(inside, wp.ID)
		} else {
			outside = append(outside, wp.ID)
		}
	}
	if len(outside) == 0 || !slices.Contains(inside, "1007") {
		t.Fatalf("seed has no outside waypoint for 1001+1002 (outside %v, inside %v)", outside, inside)
	}
	for _, wp := range outside {
		if err := f.scanErrAt(f.tick(4), 2, wp); !errors.Is(err, ErrNotInPattern) {
			t.Fatalf("outside waypoint %s: %v", wp, err)
		}
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"1001", "1002"}) || f.loggedScans(t) != 2 {
		t.Fatalf("rejected scans changed the stack: %v", got)
	}
	f.scanAt(t, f.tick(5), 2, "1007")
	expectOutcome(t, "complete after rejections", f.scanAt(t, f.tick(6), 3, "1008"), OutcomePatternCompleted)
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
		if err := f.scanErrAt(f.tick(4), c.member, c.waypoint); !errors.Is(err, ErrNoShapeSelected) {
			t.Fatalf("member %d scanning %s after completion: %v", c.member, c.waypoint, err)
		}
	}
	f.selectAt(t, f.tick(5), 0, "square")
	expectOutcome(t, "after a new selection", f.scanAt(t, f.tick(6), 3, "1008"), OutcomeScanAdded)
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
	expectOutcome(t, "single point", res, OutcomePatternCompleted)
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
		want := OutcomeScanAdded
		if i == len(hex)-1 {
			want = OutcomePatternCompleted
		}
		expectOutcome(t, fmt.Sprintf("hexagon point %d", i+1), f.scanAt(t, f.tick(float64(i)), i, w), want)
	}
}

// TestCompletingAgain checks that completing an already completed pattern
// reports it and doesn't grow progress.
func TestCompletingAgain(t *testing.T) {
	f := started(t)
	for round := range 2 {
		t0 := time.Duration(round) * 2 * f.rules.limit()
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
	if err := f.scanErrAt(f.tick(2), 1, "1002"); !errors.Is(err, ErrNoShapeSelected) {
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
	later := f.rules.limit() + f.tick(35) // 100 s: long after the square
	f.selectAt(t, later, 4, "triangle")
	f.scanAt(t, later, 4, "1003")
	f.scanAt(t, later+f.tick(1), 5, "1004")
	f.clock.at(later + f.tick(2))

	before := pgtest.Must(json.Marshal(f.view(t)))
	restarted := pgtest.Must(Load(context.Background(), pgtest.Open(t, f.dbURL), f.teams, f.hub, pgtest.Quiet, f.rules))
	restarted.now = f.clock.now
	after := pgtest.Must(json.Marshal(pgtest.Must(restarted.ForUser(f.users[0]))))
	if string(after) != string(before) {
		t.Fatalf("state changed across restart\nbefore: %s\nafter:  %s", before, after)
	}
	if _, err := restarted.Scan(context.Background(), f.users[5], "1004"); !errors.Is(err, ErrWaypointAlreadyScanned) {
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
	if ok != 1 || !errors.Is(errs[0], ErrWaypointAlreadyScanned) {
		t.Fatalf("two members, one waypoint: %d ok, %v", ok, errs)
	}
	ok, errs = race([2]func() error{scan(2, "1002"), scan(2, "1007")})
	if ok != 1 || !errors.Is(errs[0], ErrMemberAlreadyScanned) {
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
	var sel, scanned View
	for _, v := range []*View{&sel, &scanned} {
		ev := <-sub.C
		if ev.Name != "team_state" {
			t.Fatalf("event %q", ev.Name)
		}
		if err := json.Unmarshal(ev.Data, v); err != nil {
			t.Fatal(err)
		}
	}
	if sel.Selection == nil || sel.Selection.Shape != "square" || !slices.Equal(stackWaypoints(scanned), []string{"1010"}) {
		t.Fatalf("events: %+v / %+v", sel, scanned)
	}
}

// Benchmark sinks: each benchmark stores its results here, so the compiler
// can't discard the lookups as unused.
var (
	// sinkBool holds boolean results.
	sinkBool bool
	// sinkInt holds index results.
	sinkInt int
)

// BenchmarkScanLookups measures the content lookups a scan relies on, on the
// small content_test.go fixture (four waypoints, three patterns). Numbers on
// the 2025 seed are in content.go's FitsShape comment and the README.
func BenchmarkScanLookups(b *testing.B) {
	c, err := newContent(fixture(), maxPoints(b))
	if err != nil {
		b.Fatal(err)
	}
	scanned, _ := c.SetOf([]string{"1001", "1002", "1003"})
	triangle, _ := c.ShapeIndex("triangle")

	b.Run("FitsShape", func(b *testing.B) {
		for b.Loop() {
			sinkBool = c.FitsShape(triangle, scanned)
		}
	})
	b.Run("PatternFor", func(b *testing.B) {
		for b.Loop() {
			sinkInt, sinkBool = c.PatternFor(scanned)
		}
	})
	b.Run("WaypointIndex+Has", func(b *testing.B) {
		for b.Loop() {
			i, _ := c.WaypointIndex("1002")
			sinkBool = scanned.Has(i)
		}
	})
}
