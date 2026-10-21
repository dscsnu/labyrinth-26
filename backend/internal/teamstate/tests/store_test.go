package tests

// teamstate.Store-level tests against a real Postgres (TEST_DATABASE_URL), in schema
// teamstate_test, with the 2026 seed, config.toml's rules and a fake
// clock. In the 2026 seed every 2 to 6 of the 10 checkpoints (2001-2010) form a
// pattern of the shape of that size (line … hexagon), so every scan fits its
// shape; the fixture adds nowhere, in no pattern, for wrong scans. quad is
// one of the quadrilaterals.
// Times are written with f.tick (seconds after the fake clock's start). There
// is no time limit, so they only order and stamp events.

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
	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// triangle is a triangle in the 2026 seed (class_3): tests cast it between
// two quadrilaterals (class_4), since the spell cooldown refuses a spell of
// the last cast's class.
var triangle = []string{"2001", "2002", "2003"}

// nowhere is the waypoint the fixture adds in no pattern: scanning it is a
// wrong scan (the 2026 content has none of its own).
const nowhere = "2099"

// quad is a quadrilateral in the 2026 seed (G Block Junc, Arcade, Lake,
// Canopies; pattern 188, a concave one).
var quad = []string{"2001", "2002", "2007", "2008"}

// start is the fake clock's starting time.
var start = time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)

// clock is a fake teamstate.Clock that tests advance by hand. Timers (boss
// attacks) fire only when at moves time past them, in order, never from
// inside AfterFunc (the store calls AfterFunc holding a team's lock, and the
// timer's callback takes that lock).
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

// seedBosses is how many bosses the 2026 seed has; with another
// game.bosses the seed doesn't load, so these tests skip.
const seedBosses = 6

// passQR is a teamstate.QR that treats the token as the waypoint ID it names,
// so these tests exercise team state without the qr use case's signing and
// expiry. Real verification is covered by internal/qr and the server tests.
type passQR struct{}

// Verify returns the token as the waypoint ID.
func (passQR) Verify(token string) (string, error) { return token, nil }

// seedMaxPoints is the most waypoints in any 2026 seed pattern (its hexagons). With a
// smaller game.max_pattern_points the seed can't load, so these tests skip.
const seedMaxPoints = 6

// minTestTeam is the team size these tests are written for: the fixture's
// team has six members, and scenarios address them by index up to 5 (any
// member may scan any number of points, so this is about the fixture, not a
// rule). With a smaller teams.size in config.toml they skip rather than fail.
const minTestTeam = 6

// fixtureEnv is a seeded database with one team, its stores and a fake clock.
type fixtureEnv struct {
	atlas  *atlas.Atlas
	bosses *bosses.Bosses
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
	// attacks keeps config.toml's boss attack interval. Without it every
	// boss attacks only after neverAttack, so tests that move the clock by
	// minutes see no attacks.
	attacks bool
	// qrCooldown keeps config.toml's game.qr_cooldown. Without it the QR
	// cooldown is off, so tests that cast the same waypoints minutes apart
	// aren't refused.
	qrCooldown bool
	// bossRules adjusts the boss rules (attack interval and damage).
	bossRules func(*bosses.Rules)
}

// neverAttack is the attack interval every seed boss gets unless a fixture
// asks for attacks: far beyond any test's clock.
const neverAttack = 10 * 365 * 24 * time.Hour

// bossRules are config.toml's boss rules (game.bosses, game.boss_attack),
// with the interval set to neverAttack unless attacks.
func bossRules(set config.Settings, attacks bool) bosses.Rules {
	r := bosses.Rules{Count: set.Game.Bosses, AttackInterval: set.Game.BossAttack.Interval.Duration, AttackDamage: set.Game.BossAttack.BossDamage()}
	if !attacks {
		r.AttackInterval = neverAttack
	}
	return r
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
		t.Skipf("the 2026 seed has %d-point patterns; game.max_pattern_points is %d", seedMaxPoints, set.Game.MaxPatternPoints)
	}
	if set.Game.Bosses != seedBosses {
		t.Skipf("the 2026 seed has %d bosses; game.bosses is %d", seedBosses, set.Game.Bosses)
	}
	if set.Teams.Size < minTestTeam {
		t.Skipf("teamstate tests need teams.size >= %d", minTestTeam)
	}
	dbURL := pgtest.URL(t, "teamstate_test")
	db := pgtest.Open(t, dbURL)
	if err := db.Seed2026(ctx); err != nil {
		t.Fatal(err)
	}
	// In the 2026 content every combination of the 10 checkpoints is a
	// pattern, so every scan fits its shape. nowhere (in no pattern) is the
	// one waypoint whose scan is wrong.
	if _, err := db.Pool().Exec(ctx, "INSERT INTO waypoint (id, name, latitude, longitude) VALUES ($1, 'Nowhere', 0, 0)", nowhere); err != nil {
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
		// Start the team at the fake clock's start, not Postgres's now(),
		// so boss 1's attacks fall at start + k × its interval. teams reads
		// started_at at load, so reload it.
		if _, err := db.Pool().Exec(ctx, "UPDATE team SET started_at = $2 WHERE id = $1", team.ID, start); err != nil {
			t.Fatal(err)
		}
		ts = pgtest.Must(teams.Load(ctx, db, hub, pgtest.Quiet, teams.Rules{
			Size: set.Teams.Size, NameMaxLength: set.Teams.NameMaxLength,
			WriteTimeout: set.Storage.WriteTimeout.Duration, CodeAttempts: set.Teams.CodeAttempts,
		}))
	}
	rules := teamstate.Rules{
		MaxPatternPoints:    set.Game.MaxPatternPoints,
		Health:              set.Game.Health,
		LockedPatternPoints: set.Game.LockedPatternPoints,
		UnlockAfterBosses:   set.Game.UnlockAfterBosses,
		QRCooldown:          set.Game.QRCooldown.Duration,
		PowerupMultiplier:   set.Powerups.DamageMultiplier,
		PowerupHeart:        set.Powerups.Heart,
		ShortQRCooldown:     set.Powerups.ShortQRCooldown.Duration,
		ShortQRWindow:       set.Powerups.ShortQRWindow.Duration,
		WriteTimeout:        set.Storage.WriteTimeout.Duration,
		Epoch:               1, // a restart in a test passes another
	}
	if !o.qrCooldown {
		rules.QRCooldown, rules.ShortQRCooldown = 0, 0
	}
	if o.rules != nil {
		o.rules(&rules)
	}
	clk := &clock{t: start}
	rules.Clock = clk
	at := pgtest.Must(atlas.Load(ctx, db, pgtest.Quiet, atlas.Rules{MaxPatternPoints: rules.MaxPatternPoints}))
	br := bossRules(set, o.attacks)
	if o.bossRules != nil {
		o.bossRules(&br)
	}
	bs := pgtest.Must(bosses.Load(ctx, db, pgtest.Quiet, br))
	st := pgtest.Must(teamstate.Load(ctx, db, at, bs, ts, passQR{}, hub, pgtest.Quiet, rules))
	return &fixtureEnv{atlas: at, bosses: bs, db: db, dbURL: dbURL, teams: ts, state: st, hub: hub, rules: rules, clock: clk, teamID: team.ID, users: ids}
}

// tick returns n seconds: the time unit scenarios are written in. There's no
// time limit, so times only order events and stamp them.
func (f *fixtureEnv) tick(n float64) time.Duration {
	return time.Duration(n * float64(time.Second))
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
	if _, err := f.state.Select(ctx, f.users[0], "dodecagon"); !errors.Is(err, teamstate.ErrUnknownShape) {
		t.Fatalf("unknown shape: %v", err)
	}
	if _, err := f.state.Scan(ctx, uuid.New(), "2001"); !errors.Is(err, teamstate.ErrNotInTeam) {
		t.Fatalf("no team: %v", err)
	}
	if _, err := f.state.Scan(ctx, f.users[0], "2001"); !errors.Is(err, teamstate.ErrNotInGame) {
		t.Fatalf("lobby scan: %v", err)
	}
	if _, err := f.state.Select(ctx, f.users[0], "quadrilateral"); !errors.Is(err, teamstate.ErrNotInGame) {
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
	if err := f.scanErrAt(0, 0, "2001"); !errors.Is(err, teamstate.ErrNoShapeSelected) {
		t.Fatalf("scan without selection: %v", err)
	}
	if v := f.view(t); len(v.Waypoints) != 0 || len(v.Stack) != 0 || f.loggedScans(t) != 0 {
		t.Fatalf("refused scan changed state: %+v", v)
	}
}

// TestQuadCasts walks a quadrilateral to completion: a waypoint can't be scanned
// twice (the rejection changes nothing), one member may scan several of its
// waypoints, three waypoints forming a triangle don't complete the selected
// quad, and the fourth completes it, clears the selection and stack, and
// logs the cast.
func TestQuadCasts(t *testing.T) {
	f := started(t)

	if v := f.selectAt(t, 0, 0, "quadrilateral"); v.Selection == nil || v.Selection.SelectedBy != f.users[0] {
		t.Fatalf("selection: %+v", v.Selection)
	}
	expectOutcome(t, "first scan", f.scanAt(t, f.tick(1), 0, "2001"), teamstate.OutcomeScanAdded)
	if err := f.scanErrAt(f.tick(2), 1, "2001"); !errors.Is(err, teamstate.ErrWaypointAlreadyScanned) {
		t.Fatalf("same waypoint again: %v", err)
	}
	// The same member scans a second waypoint: allowed.
	expectOutcome(t, "member 0 again", f.scanAt(t, f.tick(3), 0, "2002"), teamstate.OutcomeScanAdded)
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"2001", "2002"}) || f.loggedScans(t) != 2 {
		t.Fatalf("stack: %v", got)
	}

	// 2001, 2002, 2007 is a triangle, but the team selected a quadrilateral.
	expectOutcome(t, "third scan", f.scanAt(t, f.tick(10), 2, "2007"), teamstate.OutcomeScanAdded)
	res := f.scanAt(t, f.tick(12), 0, "2008")
	expectOutcome(t, "fourth scan", res, teamstate.OutcomePatternCompleted)
	if !slices.Equal(res.Pattern, quad) || res.AlreadyCompleted {
		t.Fatalf("completion: %+v", res)
	}
	st := res.State
	if st.Selection != nil || len(st.Stack) != 0 || st.Poisoned || !slices.Equal(st.Shapes, []string{"quadrilateral"}) ||
		len(st.Patterns) != 1 || !slices.Equal(st.Waypoints, quad) || st.LastCast == nil || st.LastCast.CastBy != f.users[0] {
		t.Fatalf("after completion: %+v", st)
	}
	if err := f.scanErrAt(f.tick(13), 4, "2009"); !errors.Is(err, teamstate.ErrNoShapeSelected) {
		t.Fatalf("selection should clear after completion: %v", err)
	}
}

// TestOneMemberScansWholePattern checks that a single member can scan every
// waypoint of a pattern, the largest included.
func TestOneMemberScansWholePattern(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	res := teamstate.ScanResult{}
	for i, w := range quad {
		res = f.scanAt(t, f.tick(float64(i+1)), 3, w)
	}
	expectOutcome(t, "one member, whole quadrilateral", res, teamstate.OutcomePatternCompleted)
}

// TestNoTimeLimit checks that an attempt never expires: scans hours apart
// still complete a pattern, and nothing drops out of the stack with time.
func TestNoTimeLimit(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	for i, w := range quad[:3] {
		f.scanAt(t, time.Duration(i)*time.Hour, i, w)
	}
	f.clock.at(10 * time.Hour)
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, quad[:3]) {
		t.Fatalf("stack after 10 h: %v", got)
	}
	expectOutcome(t, "last point 10 h later", f.scanAt(t, 10*time.Hour, 3, quad[3]), teamstate.OutcomePatternCompleted)
}

// TestEverySelectionPurges checks that every selection empties the stack,
// reselecting the same shape included, so scans from an earlier attempt never
// count toward a new one.
func TestEverySelectionPurges(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	for i, w := range quad[:3] {
		f.scanAt(t, f.tick(float64(i+1)), i, w)
	}

	if v := f.selectAt(t, f.tick(4), 0, "quadrilateral"); len(v.Stack) != 0 || v.Selection.SelectedBy != f.users[0] {
		t.Fatalf("reselecting the same shape should purge: %+v", v)
	}
	res := f.scanAt(t, f.tick(5), 3, "2008") // without the purge this would complete the quadrilateral
	expectOutcome(t, "after reselect", res, teamstate.OutcomeScanAdded)
	if got := stackWaypoints(res.State); !slices.Equal(got, []string{"2008"}) {
		t.Fatalf("stack after reselect: %v", got)
	}
	// Members who scanned before the purge may scan again at once.
	expectOutcome(t, "member 0 after purge", f.scanAt(t, f.tick(6), 0, "2001"), teamstate.OutcomeScanAdded)

	if v := f.selectAt(t, f.tick(7), 0, "triangle"); len(v.Stack) != 0 || v.Selection.Shape != "triangle" {
		t.Fatalf("selecting another shape should purge: %+v", v)
	}
}

// TestRescanIsRejected checks that scanning a waypoint already in the stack
// is rejected and not pushed, whoever scans it (the same member's retry
// included): nothing logged, no event.
func TestRescanIsRejected(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	f.scanAt(t, f.tick(1), 0, "2001")

	sub := f.hub.Subscribe(f.users[2])
	defer sub.Close()
	for _, member := range []int{0, 1} {
		if err := f.scanErrAt(f.tick(2), member, "2001"); !errors.Is(err, teamstate.ErrWaypointAlreadyScanned) {
			t.Fatalf("member %d rescanning 2001: %v", member, err)
		}
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"2001"}) || f.loggedScans(t) != 1 {
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
	f.selectAt(t, 0, 0, "quadrilateral")

	const copies = 8
	errs := make([]error, copies)
	var wg sync.WaitGroup
	for i := range copies {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.state.Scan(context.Background(), f.users[0], "2001")
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

// TestWrongScanIsReported checks that a waypoint that can't be part of any
// pattern of the selected shape, given the stack, is pushed but reported:
// outcome wrong_scan, marked wrong in the stack, and the attempt poisoned, so
// every later scan, by anyone, is refused with selection_poisoned.
func TestWrongScanIsReported(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	f.scanAt(t, f.tick(1), 0, "2001")

	res := f.scanAt(t, f.tick(2), 1, nowhere) // in no pattern
	expectOutcome(t, "wrong waypoint", res, teamstate.OutcomeWrongScan)
	if !res.State.Poisoned || len(res.State.Stack) != 2 || res.State.Stack[0].Wrong || !res.State.Stack[1].Wrong {
		t.Fatalf("after a wrong scan: %+v", res.State)
	}
	if f.loggedScans(t) != 2 {
		t.Fatalf("a wrong scan is still logged: %d", f.loggedScans(t))
	}
	for _, member := range []int{1, 2} { // the scanner and a teammate
		if err := f.scanErrAt(f.tick(3), member, "2002"); !errors.Is(err, teamstate.ErrSelectionPoisoned) {
			t.Fatalf("member %d scanning a poisoned attempt: %v", member, err)
		}
	}
}

// TestWrongScanPoisonsSelection checks that a poisoned attempt can't cast,
// and that selecting again purges it so the quadrilateral then completes.
func TestWrongScanPoisonsSelection(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	f.scanAt(t, f.tick(1), 0, "2001")
	f.scanAt(t, f.tick(2), 1, "2002")
	// nowhere is in no pattern: wrong.
	expectOutcome(t, "poisoning scan", f.scanAt(t, f.tick(3), 2, nowhere), teamstate.OutcomeWrongScan)
	if err := f.scanErrAt(f.tick(4), 3, "2007"); !errors.Is(err, teamstate.ErrSelectionPoisoned) {
		t.Fatalf("the rest of the quadrilateral on a poisoned attempt: %v", err)
	}

	// Reselecting purges the wrong scan; the quadrilateral then completes.
	if v := f.selectAt(t, f.tick(6), 0, "quadrilateral"); v.Poisoned || len(v.Stack) != 0 {
		t.Fatalf("after reselect: %+v", v)
	}
	res := teamstate.ScanResult{}
	for i, w := range quad {
		res = f.scanAt(t, f.tick(7)+f.tick(float64(i)), i, w)
	}
	expectOutcome(t, "completes after reselect", res, teamstate.OutcomePatternCompleted)
}

// TestCompletionPurges checks that completing purges the stack and the
// selection: a rescan of the completing waypoint, or any other scan, needs a
// new selection, after which every member may scan again.
func TestCompletionPurges(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	for i, w := range quad {
		f.scanAt(t, f.tick(float64(i)), i, w)
	}
	if v := f.view(t); v.Selection != nil || len(v.Stack) != 0 {
		t.Fatalf("not purged: %+v", v)
	}
	for _, c := range []struct {
		member   int
		waypoint string
	}{{3, "2008"}, {4, "2009"}} {
		if err := f.scanErrAt(f.tick(4), c.member, c.waypoint); !errors.Is(err, teamstate.ErrNoShapeSelected) {
			t.Fatalf("member %d scanning %s after completion: %v", c.member, c.waypoint, err)
		}
	}
	// A new selection works again (the triangle: the quadrilateral's class is on
	// cooldown after its cast).
	f.selectAt(t, f.tick(5), 0, "triangle")
	expectOutcome(t, "after a new selection", f.scanAt(t, f.tick(6), 3, "2001"), teamstate.OutcomeScanAdded)
}

// TestOutOfOrderScanning checks that a pattern completes whatever order the
// waypoints are scanned in. A pattern is an unordered set of waypoints (a
// WaypointSet bitmask): scanning 2001 then 2007 and scanning 2007 then 2001
// set the same bits, so the same pattern is found.
func TestOutOfOrderScanning(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")

	order := []string{"2008", "2007", "2002", "2001"} // quad, scrambled
	res := teamstate.ScanResult{}
	for i, w := range order {
		res = f.scanAt(t, f.tick(float64(i)), i, w)
	}
	expectOutcome(t, "completes out of order", res, teamstate.OutcomePatternCompleted)
	if !slices.Equal(res.Pattern, quad) { // Pattern is returned sorted
		t.Fatalf("pattern %v", res.Pattern)
	}
}

// TestSinglePointPattern checks that a 1-waypoint pattern completes on its
// only scan.
func TestSinglePointPattern(t *testing.T) {
	f := started(t, fixtureOptions{sql: `
		INSERT INTO shape (id, name) VALUES ('dot', 'Dot');
		INSERT INTO pattern (id, shape_id, damage, spell_class_id) VALUES (1001, 'dot', 15, 'class_1');
		INSERT INTO pattern_waypoint (pattern_id, waypoint_id) VALUES (1001, '2010');`})
	f.selectAt(t, 0, 0, "dot")
	res := f.scanAt(t, f.tick(1), 0, "2010")
	expectOutcome(t, "single point", res, teamstate.OutcomePatternCompleted)
	if !slices.Equal(res.Pattern, []string{"2010"}) {
		t.Fatalf("pattern %v", res.Pattern)
	}
}

// TestSixPointPatternAtCapacity checks that a pattern with
// max_pattern_points waypoints completes exactly as the stack fills (big
// patterns unlocked from the start: unlock_after_bosses 0).
func TestSixPointPatternAtCapacity(t *testing.T) {
	hex := []string{"2003", "2004", "2005", "2006", "2009", "2010"}
	f := started(t, fixtureOptions{rules: func(r *teamstate.Rules) { r.UnlockAfterBosses = 0 }}) // a seed hexagon
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
// reports it and doesn't grow progress (a triangle cast in between: the
// spell cooldown refuses two quadrilaterals in a row).
func TestCompletingAgain(t *testing.T) {
	f := started(t)
	if res := f.castQuad(t, 0); res.AlreadyCompleted {
		t.Fatal("first quadrilateral: already_completed")
	}
	f.castTriangle(t, 1)
	if res := f.castQuad(t, 2); !res.AlreadyCompleted {
		t.Fatal("second quadrilateral: not already_completed")
	}
	if v := f.view(t); len(v.Patterns) != 2 || len(v.Shapes) != 2 {
		t.Fatalf("patterns after the quadrilateral twice and a triangle: %v, shapes %v", v.Patterns, v.Shapes)
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
	f.selectAt(t, 0, 0, "quadrilateral")
	f.scanAt(t, f.tick(1), 0, "2001")
	v := pgtest.Must(f.state.ClearSelection(ctx, f.users[0]))
	if v.Selection != nil || len(v.Stack) != 0 || !slices.Equal(v.Waypoints, []string{"2001"}) {
		t.Fatalf("after clear: %+v", v)
	}
	if err := f.scanErrAt(f.tick(2), 1, "2002"); !errors.Is(err, teamstate.ErrNoShapeSelected) {
		t.Fatalf("scan after clear: %v", err)
	}
}

// TestRestartRestoresEverything checks that progress, the selection and the
// live stack (rebuilt from team_scan) reload exactly, and that rescans are
// still rejected after the restart.
func TestRestartRestoresEverything(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	for i, w := range quad {
		f.scanAt(t, f.tick(float64(i)), i, w)
	}
	later := f.tick(100) // long after the quadrilateral
	f.selectAt(t, later, 0, "triangle")
	f.scanAt(t, later, 4, "2003")
	f.scanAt(t, later+f.tick(1), 5, "2004")
	f.clock.at(later + f.tick(2))

	beforeView := f.view(t)
	rules := f.rules
	rules.Epoch = 2 // a new process
	restarted := pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.bosses, f.teams, passQR{}, f.hub, pgtest.Quiet, rules))
	afterView := pgtest.Must(restarted.ForUser(f.users[0]))
	// Version is memory-only: a restart starts again at seq 0, in the new
	// epoch. Everything else reloads exactly.
	if afterView.Version != (live.Version{Epoch: 2}) {
		t.Fatalf("version after restart %+v, want seq 0 in epoch 2", afterView.Version)
	}
	beforeView.Version, afterView.Version = live.Version{}, live.Version{}
	before, after := pgtest.Must(json.Marshal(beforeView)), pgtest.Must(json.Marshal(afterView))
	if string(after) != string(before) {
		t.Fatalf("state changed across restart\nbefore: %s\nafter:  %s", before, after)
	}
	if _, err := restarted.Scan(context.Background(), f.users[5], "2004"); !errors.Is(err, teamstate.ErrWaypointAlreadyScanned) {
		t.Fatalf("rescan after restart: %v", err)
	}
}

// TestConcurrentScans races two members at the same waypoint (exactly one is
// accepted) and one member at two waypoints (both are: a member may scan any
// number).
func TestConcurrentScans(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")

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

	ok, errs := race([2]func() error{scan(0, "2001"), scan(1, "2001")})
	if ok != 1 || !errors.Is(errs[0], teamstate.ErrWaypointAlreadyScanned) {
		t.Fatalf("two members, one waypoint: %d ok, %v", ok, errs)
	}
	// One member may scan any number of waypoints: both are accepted.
	ok, errs = race([2]func() error{scan(2, "2002"), scan(2, "2007")})
	if ok != 2 {
		t.Fatalf("one member, two waypoints: %d ok, %v", ok, errs)
	}
}

// TestFailedWriteReconciles checks that when memory and Postgres disagree (the
// selection was deleted by hand), a failed write reloads the team's state
// from Postgres and applies nothing.
func TestFailedWriteReconciles(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	f.selectAt(t, 0, 0, "quadrilateral")
	f.scanAt(t, f.tick(1), 0, "2001")

	conn := pgtest.Must(pgx.Connect(ctx, f.dbURL))
	if _, err := conn.Exec(ctx, "DELETE FROM team_selection WHERE team_id = $1", f.teamID); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()
	seq := f.view(t).Version.Seq
	if _, err := f.state.ClearSelection(ctx, f.users[0]); err == nil {
		t.Fatal("clearing a selection deleted from Postgres should fail")
	}
	// The reload is published as resync, one version on: never back.
	if typ, pub := nextState(t, sub); typ != live.Resync || pub.Version.Seq != seq+1 {
		t.Fatalf("reload published %s at seq %d, want resync at %d", typ, pub.Version.Seq, seq+1)
	}
	v := f.view(t)
	if v.Selection != nil || len(v.Stack) != 0 || !slices.Equal(v.Waypoints, []string{"2001"}) {
		t.Fatalf("not reconciled from Postgres: %+v", v)
	}
	f.selectAt(t, f.tick(2), 0, "quadrilateral")
}

// TestReadsDoNotTouchDatabase checks that reads keep working with the
// database closed, and that a failed write leaves memory unchanged.
func TestReadsDoNotTouchDatabase(t *testing.T) {
	f := started(t)
	f.selectAt(t, 0, 0, "quadrilateral")
	f.scanAt(t, f.tick(1), 0, "2001")
	f.db.Close()

	if v := f.view(t); v.Selection == nil || !slices.Equal(stackWaypoints(v), []string{"2001"}) {
		t.Fatalf("read after DB close: %+v", v)
	}
	if err := f.scanErrAt(f.tick(2), 1, "2002"); err == nil {
		t.Fatal("scan with the database closed should fail")
	}
	if got := stackWaypoints(f.view(t)); !slices.Equal(got, []string{"2001"}) {
		t.Fatalf("failed write changed memory: %v", got)
	}
}

// TestStateIsPublished checks that selections and scans reach teammates as
// team_state events.
func TestStateIsPublished(t *testing.T) {
	f := started(t)
	sub := f.hub.Subscribe(f.users[3])
	defer sub.Close()

	f.selectAt(t, 0, 0, "quadrilateral")
	f.scanAt(t, f.tick(1), 0, "2010")
	_, sel := nextState(t, sub)
	_, scanned := nextState(t, sub)
	if sel.Selection == nil || sel.Selection.Shape != "quadrilateral" || !slices.Equal(stackWaypoints(scanned), []string{"2010"}) {
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
		_, v := nextState(t, sub)
		if v.TeamID != f.teamID || v.Selection != nil || len(v.Stack) != 0 || len(v.Waypoints) != 0 {
			t.Fatalf("member %d: state after start %+v, want empty", i, v)
		}
	}

	f.selectAt(t, 0, 0, "quadrilateral")
	nextState(t, subs[0]) // the selection's own event
	f.state.PublishTeam(f.teamID)
	if _, v := nextState(t, subs[0]); v.Selection == nil || v.Selection.Shape != "quadrilateral" {
		t.Fatalf("republished state %+v, want the quadrilateral selected", v)
	}
	if got, _ := json.Marshal(f.view(t)); f.loggedScans(t) != 0 || !strings.Contains(string(got), `"quadrilateral"`) {
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

// nextState waits briefly for the next team_state event on sub and returns
// its type and its message decoded, failing if none arrives.
func nextState(t *testing.T, sub *live.Subscription) (live.EventType, teamstate.View) {
	t.Helper()
	select {
	case ev := <-sub.C:
		if ev.Name() != "team_state" {
			t.Fatalf("event %q, want team_state", ev.Name())
		}
		var env struct {
			Type    live.EventType `json:"type"`
			Message teamstate.View `json:"message"`
		}
		if err := json.Unmarshal(ev.Data(), &env); err != nil {
			t.Fatal(err)
		}
		if env.Type != ev.Type() {
			t.Fatalf("data type %q, event type %q", env.Type, ev.Type())
		}
		return env.Type, env.Message
	case <-time.After(time.Second):
		t.Fatal("no team_state event")
	}
	return "", teamstate.View{}
}

// TestCastLog checks that every cast is logged in memory and in team_cast,
// repeats included and marked: the quadrilateral, a triangle (the cooldown forbids
// two quadrilaterals in a row), the quadrilateral again; last_cast is the newest,
// CastsForUser lists them newest first with their classes, a new selection
// keeps them, and a restart reloads the whole log.
func TestCastLog(t *testing.T) {
	f := started(t)
	if v := f.view(t); v.LastCast != nil {
		t.Fatalf("last_cast before any cast: %+v", v.LastCast)
	}
	if cs := pgtest.Must(f.state.CastsForUser(f.users[0])); len(cs) != 0 {
		t.Fatalf("casts before any cast: %v", cs)
	}
	for round := range 3 {
		t0 := time.Duration(round) * f.tick(100)
		shape, ids, class := "quadrilateral", quad, "class_4"
		if round == 1 {
			shape, ids, class = "triangle", triangle, "class_3"
		}
		f.cast(t, round, shape, ids)
		lc := f.view(t).LastCast
		if lc == nil || !slices.Equal(lc.Pattern, ids) || lc.Shape != shape || lc.Class != class || lc.CastBy != f.users[len(ids)-1] ||
			!lc.CastAt.Equal(start.Add(t0+f.tick(float64(len(ids))))) || lc.AlreadyCompleted != (round == 2) || lc.Damage <= 0 {
			t.Fatalf("round %d: last_cast %+v", round, lc)
		}
	}
	f.selectAt(t, f.tick(500), 0, "triangle")

	cs := pgtest.Must(f.state.CastsForUser(f.users[2]))
	if len(cs) != 3 || !cs[0].AlreadyCompleted || cs[1].AlreadyCompleted || cs[2].AlreadyCompleted ||
		!cs[0].CastAt.After(cs[1].CastAt) || !cs[1].CastAt.After(cs[2].CastAt) ||
		cs[0].Class != "class_4" || cs[1].Class != "class_3" || cs[2].Class != "class_4" {
		t.Fatalf("cast log (newest first): %+v", cs)
	}
	var rows int
	if err := f.db.Pool().QueryRow(context.Background(),
		"SELECT count(*) FROM team_cast WHERE team_id = $1", f.teamID).Scan(&rows); err != nil || rows != 3 {
		t.Fatalf("team_cast has %d rows (%v), want 3", rows, err)
	}
	restarted := pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, f.dbURL), f.atlas, f.bosses, f.teams, passQR{}, f.hub, pgtest.Quiet, f.rules))
	before := pgtest.Must(json.Marshal(cs))
	after := pgtest.Must(json.Marshal(pgtest.Must(restarted.CastsForUser(f.users[2]))))
	if string(before) != string(after) {
		t.Fatalf("cast log across restart\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestVersionAndTypes checks the type of every team_state publish and its
// version: PublishTeam (game_started) resends the current version; select,
// scans (added, wrong, cast) and clear each go up by one; a rejected scan and
// a clear with nothing selected publish nothing and keep it; cast_count
// counts the cast.
func TestVersionAndTypes(t *testing.T) {
	f := started(t, fixtureOptions{sql: "UPDATE boss SET hp = 1000000 WHERE serial = 1"}) // the cast is a plain spell_cast
	ctx := context.Background()
	sub := f.hub.Subscribe(f.users[0])
	defer sub.Close()

	var seq uint64
	want := func(what string, typ live.EventType, step uint64, casts int) {
		t.Helper()
		seq += step
		gotTyp, v := nextState(t, sub)
		if gotTyp != typ || v.Version != (live.Version{Epoch: f.rules.Epoch, Seq: seq}) || v.CastCount != casts {
			t.Fatalf("%s: %s %+v cast_count %d, want %s seq %d cast_count %d", what, gotTyp, v.Version, v.CastCount, typ, seq, casts)
		}
	}
	quiet := func(what string) {
		t.Helper()
		select {
		case ev := <-sub.C:
			t.Fatalf("%s published %s:%s", what, ev.Name(), ev.Type())
		default:
		}
		if v := f.view(t); v.Version.Seq != seq {
			t.Fatalf("%s: version %+v, want seq %d", what, v.Version, seq)
		}
	}

	f.state.PublishTeam(f.teamID)
	want("publish team", teamstate.GameStarted, 0, 0)
	f.selectAt(t, 0, 0, "quadrilateral")
	want("select", teamstate.ShapeSelected, 1, 0)
	for i, w := range quad[:3] {
		f.scanAt(t, f.tick(float64(i+1)), i, w)
		want("scan "+w, teamstate.ScanAdded, 1, 0)
	}
	if err := f.scanErrAt(f.tick(4), 0, quad[0]); !errors.Is(err, teamstate.ErrWaypointAlreadyScanned) {
		t.Fatalf("rescan: %v", err)
	}
	quiet("rejected rescan")
	f.scanAt(t, f.tick(5), 3, quad[3])
	want("completing scan", teamstate.SpellCast, 1, 1)
	if _, err := f.state.ClearSelection(ctx, f.users[0]); err != nil {
		t.Fatal(err)
	}
	quiet("clear with nothing selected")
	if _, err := f.state.Select(ctx, f.users[0], "quadrilateral"); !errors.Is(err, teamstate.ErrShapeOnCooldown) {
		t.Fatalf("reselecting the quadrilateral right after its cast: %v, want shape_on_cooldown", err)
	}
	quiet("quadrilateral on cooldown")
	f.selectAt(t, f.tick(6), 0, "triangle")
	want("select another class", teamstate.ShapeSelected, 1, 1)
	f.scanAt(t, f.tick(7), 0, "2001")
	want("scan", teamstate.ScanAdded, 1, 1)
	f.scanAt(t, f.tick(7), 1, "2003")
	want("scan", teamstate.ScanAdded, 1, 1)
	f.scanAt(t, f.tick(8), 2, nowhere) // in no pattern: wrong
	want("wrong scan", teamstate.WrongScan, 1, 1)
	if _, err := f.state.ClearSelection(ctx, f.users[0]); err != nil {
		t.Fatal(err)
	}
	want("clear", teamstate.SelectionCleared, 1, 1)
	if got, _ := f.state.Snapshot(f.users[0]); got.Type() != live.Snapshot {
		t.Fatalf("snapshot type %q", got.Type())
	}
}

// TestBigPatternsUnlock uses a seed pentagon, and adds a shape mixing a
// 4-point and a 5-point pattern (through nowhere and a second test-only
// waypoint, 2098, so neither repeats a seed pattern's set; 2098 is only in
// the 5-point one), and checks the unlock: before the team has defeated
// unlock_after_bosses bosses, unlocked_points is locked_pattern_points − 1,
// the pentagon is in locked_shapes and can't be selected (shape_locked), and
// in the mixed shape a scan that only fits the 5-point pattern is wrong;
// after that many defeats (every boss at 1 HP) unlocked_points is
// max_pattern_points and both cast.
func TestBigPatternsUnlock(t *testing.T) {
	pent := []string{"2003", "2004", "2005", "2006", "2009"}
	f := started(t, fixtureOptions{sql: `
		UPDATE boss SET hp = 1;
		INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('2098', 'Elsewhere', 0, 0);
		INSERT INTO shape (id, name) VALUES ('mixed', 'Mixed');
		INSERT INTO pattern (id, shape_id, damage, spell_class_id) VALUES (1004, 'mixed', 100, 'class_4'), (1005, 'mixed', 150, 'class_5');
		INSERT INTO pattern_waypoint (pattern_id, waypoint_id) VALUES
			(1004, '2001'), (1004, '2002'), (1004, '2008'), (1004, '2099'),
			(1005, '2003'), (1005, '2004'), (1005, '2005'), (1005, '2098'), (1005, '2099');`})
	if f.rules.LockedPatternPoints != 5 || f.rules.MaxPatternPoints < 5 || f.rules.UnlockAfterBosses < 1 {
		t.Skipf("test assumes locked_pattern_points = 5 and something to unlock (unlock_after_bosses %d)", f.rules.UnlockAfterBosses)
	}
	v := f.view(t)
	if v.UnlockedPoints != 4 || !slices.Contains(v.LockedShapes, "pentagon") || slices.Contains(v.LockedShapes, "mixed") {
		t.Fatalf("before the unlock: unlocked_points %d, locked %v", v.UnlockedPoints, v.LockedShapes)
	}
	if _, err := f.state.Select(context.Background(), f.users[0], "pentagon"); !errors.Is(err, teamstate.ErrShapeLocked) {
		t.Fatalf("selecting a locked shape: %v", err)
	}
	f.selectAt(t, 0, 0, "mixed")
	if res := f.scanAt(t, f.tick(1), 0, "2098"); res.Outcome != teamstate.OutcomeWrongScan {
		t.Fatalf("a point only in the locked 5-point pattern: %s", res.Outcome)
	}

	for round := range f.rules.UnlockAfterBosses {
		f.castAlternating(t, round+1) // each defeats a 1-HP boss
	}
	v = f.view(t)
	if v.BossesDefeated != f.rules.UnlockAfterBosses || v.UnlockedPoints != f.rules.MaxPatternPoints || len(v.LockedShapes) != 0 {
		t.Fatalf("after %d defeats: unlocked_points %d, locked %v", v.BossesDefeated, v.UnlockedPoints, v.LockedShapes)
	}
	t0 := f.tick(1000)
	f.selectAt(t, t0, 0, "pentagon")
	var res teamstate.ScanResult
	for i, w := range pent {
		res = f.scanAt(t, t0+f.tick(float64(i+1)), i, w)
	}
	expectOutcome(t, "the pentagon after the unlock", res, teamstate.OutcomePatternCompleted)
}

// TestOnlyLeaderSelects checks that selecting and clearing are the leader's:
// any other member gets not_leader for both and changes nothing, and still
// scans.
func TestOnlyLeaderSelects(t *testing.T) {
	f := started(t)
	ctx := context.Background()
	if _, err := f.state.Select(ctx, f.users[1], "quadrilateral"); !errors.Is(err, teamstate.ErrNotLeader) {
		t.Fatalf("a member selecting: %v", err)
	}
	f.selectAt(t, 0, 0, "quadrilateral")
	f.scanAt(t, f.tick(1), 3, "2001")
	if _, err := f.state.ClearSelection(ctx, f.users[2]); !errors.Is(err, teamstate.ErrNotLeader) {
		t.Fatalf("a member clearing: %v", err)
	}
	if v := f.view(t); v.Selection == nil || len(v.Stack) != 1 {
		t.Fatalf("a refused clear changed the state: %+v", v)
	}
}
