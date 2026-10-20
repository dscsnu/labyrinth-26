package tests

// The game-loop simulation: simTeams teams of every size play the whole loop
// on the fake clock (selecting, scanning, wrong scans, retries, races,
// casts, every boss, boss attacks, knockouts, revives, an admin removing
// members, damage multipliers), each with its own configuration. After every
// step every team's state is checked against model, an independent
// re-implementation of the rules, and at the end against the last team_state
// its watcher received. TestGameLoopRestart plays the same league with the
// backend going down part way, for several lengths of downtime.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

const (
	// simTeams is how many teams play at once.
	simTeams = 100
	// simStep is the game time between steps. Shorter than
	// simAttackInterval, so no step passes two attacks of one boss.
	simStep = 30 * time.Second
	// simAttackInterval is how often the league's bosses attack: shorter
	// than config.toml's game.boss_attack.interval, so the simulation's 90
	// minutes knock teams out (boss 1 alone needs 10 attacks at 1 damage:
	// 50 min) and the restart test's downtimes span several attacks, yet
	// long enough that the fastest teams get through every boss.
	simAttackInterval = 5 * time.Minute
	// simSteps is how long the game runs: 180 × 30 s = 90 min, long enough
	// for an idle team to be knocked out by boss 1 (10 attacks × 5 min).
	simSteps = 180
	// simDownAt is the step after which TestGameLoopRestart takes the
	// backend down; it plays as many steps again after the restart.
	simDownAt = 45
	// simRemoveAt is the step at which shrinking teams lose a member.
	simRemoveAt = 40
	// simReviveIdleAt is the step at which idle teams are revived, if
	// knocked out by then.
	simReviveIdleAt = 110
	// simReviveDelay is how many steps a knocked-out slowpoke waits for its
	// revive.
	simReviveDelay = 3
)

// simStrategies are the ways a team plays; team i plays simStrategies[i %
// len] with size 1 + (i / len) % 6, so each strategy runs at several sizes.
var simStrategies = []string{
	"steady",     // casts a random pattern every 2 steps, members taking turns
	"rush",       // casts every step: defeats every boss, then casts for nothing
	"solo",       // one member scans every point, every 3 steps
	"idle",       // never plays: knocked out by boss 1, revived, attacked again
	"sloppy",     // a wrong scan poisons the attempt; a rescan is refused; reselects and casts
	"retrier",    // sends every scan twice: the resend is refused, changing nothing
	"quitter",    // half its attempts are given up (cleared) after one scan
	"repeater",   // casts the same pattern every time: already_completed after the first
	"slowpoke",   // casts every 8 steps: attacked mid-fight, knocked out, revived
	"racer",      // two members scan the same point at once: exactly one is accepted
	"shrinking",  // an admin removes a member (the leader for even teams), or is refused the last one
	"multiplier", // a stored damage multiplier (×2, ×1.5, ×0.5, ×3) scales every cast
}

// simMultipliers are the multipliers multiplier teams get, by team index.
var simMultipliers = []float64{2, 1.5, 0.5, 3}

// league is a seeded database with simTeams started teams, their stores and
// a fake clock: the game-loop simulation's fixture.
type league struct {
	atlas  *atlas.Atlas
	bosses *bosses.Bosses
	db     *postgres.DB
	dbURL  string
	teams  *teams.Store
	state  *teamstate.Store
	hub    *live.Hub
	rules  teamstate.Rules
	clock  *clock
	sims   []*simTeam
	// elapsed is how far the fake clock is past start: each step adds
	// simStep, and downtime adds its length.
	elapsed time.Duration
	// big are the patterns of at least game.locked_pattern_points
	// waypoints (the league adds a pentagon and a hexagon: the seed has
	// none), locked until a team has defeated game.unlock_after_bosses.
	big []int
}

// simBigPatterns adds a 5-point pentagon and a 6-point hexagon, each a shape
// of its own: the locked patterns the simulation unlocks.
const simBigPatterns = `
	INSERT INTO shape (id, name) VALUES ('pentagon', 'Pentagon'), ('hexagon', 'Hexagon');
	INSERT INTO pattern (id, shape_id, damage, spell_class_id) VALUES (2001, 'pentagon', 150, 'class_5'), (2002, 'hexagon', 170, 'class_6');
	INSERT INTO pattern_waypoint (pattern_id, waypoint_id) VALUES
		(2001, '1003'), (2001, '1004'), (2001, '1005'), (2001, '1006'), (2001, '1009'),
		(2002, '1003'), (2002, '1004'), (2002, '1005'), (2002, '1006'), (2002, '1009'), (2002, '1010');`

// simTeam is one team in the simulation: its configuration, members, model
// and the last team_state its watcher received.
type simTeam struct {
	i        int
	id       string
	strategy string
	members  []uuid.UUID
	removed  int  // index of the member an admin removes, or -1
	gone     bool // the removal has happened
	watcher  int  // index of a member never removed, whose events are kept
	rng      *rand.Rand
	model    model
	koStep   int // the step the team was seen knocked out, or -1

	mu   sync.Mutex
	last []byte // the newest team_state message the watcher received
}

// simCounts counts what the simulation exercised, so a configuration that
// silently stopped happening fails the test.
type simCounts struct {
	casts, defeats, allDefeated, knockouts, revives, wrongScans, poisoned, retries,
	cleared, already, races, removals, lastMember, refusedRemoved, knockedRefused,
	lockedRefused, bigCasts atomic.Int64
}

// newLeague seeds the dev content and starts simTeams teams of every size,
// with boss attacks as seeded. Multiplier teams get their multiplier stored
// before the store loads.
func newLeague(t testing.TB, attacks bool) *league {
	t.Helper()
	ctx := context.Background()
	set := configtest.Settings(t)
	if set.Game.MaxPatternPoints < seedMaxPoints || set.Game.Bosses != seedBosses || set.Teams.Size < minTestTeam {
		t.Skipf("the simulation needs the 2025 seed's limits (%d points, %d bosses) and teams.size >= %d",
			seedMaxPoints, seedBosses, minTestTeam)
	}
	dbURL := pgtest.URL(t, "teamstate_sim")
	db := pgtest.Open(t, dbURL)
	if err := db.SeedDev2025(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, simBigPatterns); err != nil {
		t.Fatal(err)
	}
	l := &league{db: db, dbURL: dbURL, hub: live.NewHub(set.Live.HubShards, set.Live.SubscriberBuffer), clock: &clock{t: start}}
	teamRules := teams.Rules{
		Size: set.Teams.Size, NameMaxLength: set.Teams.NameMaxLength,
		WriteTimeout: set.Storage.WriteTimeout.Duration, CodeAttempts: set.Teams.CodeAttempts,
		OnRosterChange: func(tm teams.Team) { l.state.RosterChanged(tm.ID) },
	}
	ts := pgtest.Must(teams.Load(ctx, db, l.hub, pgtest.Quiet, teamRules))
	users := user.New(db)
	for i := range simTeams {
		st := &simTeam{i: i, strategy: simStrategies[i%len(simStrategies)], removed: -1, koStep: -1,
			rng: rand.New(rand.NewPCG(uint64(i), 2026))}
		size := 1 + (i/len(simStrategies))%set.Teams.Size
		for m := range size {
			u := pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{
				Subject: fmt.Sprintf("sim-%d-%d", i, m), Email: fmt.Sprintf("sim%d.%d@snu.edu.in", i, m), Name: fmt.Sprintf("T%dM%d", i, m),
			}))
			st.members = append(st.members, u.ID)
		}
		team := pgtest.Must(ts.Create(ctx, st.members[0], fmt.Sprintf("Sim %d", i)))
		for _, u := range st.members[1:] {
			pgtest.Must(ts.Join(ctx, u, team.ID))
			pgtest.Must(ts.SetReady(ctx, u, true))
		}
		pgtest.Must(ts.Start(ctx, st.members[0]))
		st.id = team.ID
		st.model = model{phaseStart: start, phaseHealth: set.Game.Health, mult: 1, patterns: map[int]bool{}}
		if st.strategy == "multiplier" {
			st.model.mult = simMultipliers[(i/len(simStrategies))%len(simMultipliers)]
			if _, err := db.Pool().Exec(ctx, "INSERT INTO team_boss (team_id, multiplier) VALUES ($1, $2)", team.ID, st.model.mult); err != nil {
				t.Fatal(err)
			}
		}
		l.sims = append(l.sims, st)
	}
	// Every team starts at the fake clock's start (see started); teams reads
	// started_at at load.
	if _, err := db.Pool().Exec(ctx, "UPDATE team SET started_at = $1 WHERE status = 'IN_GAME'", start); err != nil {
		t.Fatal(err)
	}
	l.teams = pgtest.Must(teams.Load(ctx, db, l.hub, pgtest.Quiet, teamRules))
	l.rules = teamstate.Rules{
		MaxPatternPoints: set.Game.MaxPatternPoints, Health: set.Game.Health,
		LockedPatternPoints: set.Game.LockedPatternPoints, UnlockAfterBosses: set.Game.UnlockAfterBosses,
		WriteTimeout: set.Storage.WriteTimeout.Duration, Epoch: 1, Clock: l.clock,
	}
	l.atlas = pgtest.Must(atlas.Load(ctx, db, pgtest.Quiet, atlas.Rules{MaxPatternPoints: l.rules.MaxPatternPoints}))
	for p := range l.atlas.PatternCount() {
		if l.atlas.Pattern(p).Waypoints.Len() >= l.rules.LockedPatternPoints {
			l.big = append(l.big, p)
		}
	}
	// The league's bosses attack every simAttackInterval with config.toml's
	// damages, so 90 minutes see many attacks of every boss.
	br := bossRules(set, attacks)
	if attacks {
		br.AttackInterval = simAttackInterval
	}
	l.bosses = pgtest.Must(bosses.Load(ctx, db, pgtest.Quiet, br))
	l.state = pgtest.Must(teamstate.Load(ctx, db, l.atlas, l.bosses, l.teams, passQR{}, l.hub, pgtest.Quiet, l.rules))
	return l
}

// model is an independent re-implementation of a team's boss and health
// rules, from the README rather than the code: what the team's state must be.
type model struct {
	defeated, damage int
	phaseStart       time.Time
	phaseHealth      int
	mult             float64
	casts            int
	patterns         map[int]bool
}

// health returns the team's health and the current boss's attacks this
// phase at now: one attack every interval from the phase's start, each
// taking the boss's attack damage, stopping at 0 (knocked out) or once
// every boss is defeated.
func (m *model) health(now time.Time, b *bosses.Bosses) (health, attacks int) {
	if m.defeated >= b.Count() {
		return m.phaseHealth, 0
	}
	boss := b.At(m.defeated)
	k := 0
	if elapsed := now.Sub(m.phaseStart); elapsed > 0 {
		k = int(elapsed / boss.AttackInterval())
	}
	if boss.AttackDamage > 0 {
		k = min(k, (m.phaseHealth+boss.AttackDamage-1)/boss.AttackDamage) // no attacks after the knockout
	}
	return max(0, m.phaseHealth-k*boss.AttackDamage), k
}

// cast applies a cast of the pattern's damage at now: the damage dealt
// (× the multiplier, rounded), the boss hit (0 for none) and whether it fell.
// A defeat starts the next boss's phase at now with the team's health then.
func (m *model) cast(now time.Time, damage int, b *bosses.Bosses) (dealt, boss int, defeated bool) {
	m.casts++
	if m.defeated >= b.Count() {
		return 0, 0, false
	}
	dealt = int(math.Round(float64(damage) * m.mult))
	boss = m.defeated + 1
	m.damage += dealt
	if m.damage >= b.At(m.defeated).HP {
		h, _ := m.health(now, b)
		m.defeated, m.damage, m.phaseStart, m.phaseHealth = m.defeated+1, 0, now, h
		defeated = true
	}
	return dealt, boss, defeated
}

// unlocked returns the most waypoints a pattern the team can cast may have:
// locked − 1 until it has defeated after bosses, then max.
func (m *model) unlocked(locked, after, maxPoints int) int {
	if m.defeated >= after {
		return maxPoints
	}
	return locked - 1
}

// revive restarts the current phase at now with full health.
func (m *model) revive(now time.Time, full int) { m.phaseStart, m.phaseHealth = now, full }

// TestGameLoopSimulation plays simSteps steps of simStep: each step the clock
// moves (firing due boss attacks), then every team acts at once, each by
// its strategy; then every team's state must equal its model. At the end
// each watcher's last event must be the team's state, and every
// configuration must have happened.
func TestGameLoopSimulation(t *testing.T) {
	l := newLeague(t, true)
	var counts simCounts
	l.watchAll(t)
	l.steps(t, 1, simSteps, &counts)
	l.converge(t)

	for name, c := range map[string]*atomic.Int64{
		"casts": &counts.casts, "boss defeats": &counts.defeats, "teams through every boss": &counts.allDefeated,
		"knockouts": &counts.knockouts, "revives": &counts.revives, "wrong scans": &counts.wrongScans,
		"poisoned refusals": &counts.poisoned, "refused resends": &counts.retries, "cleared attempts": &counts.cleared,
		"repeat casts": &counts.already, "races": &counts.races, "removals": &counts.removals,
		"last-member refusals": &counts.lastMember, "removed members refused": &counts.refusedRemoved,
		"knocked-out refusals":  &counts.knockedRefused,
		"locked shapes refused": &counts.lockedRefused, "big patterns cast after the unlock": &counts.bigCasts,
	} {
		if c.Load() == 0 {
			t.Errorf("the simulation never exercised %s", name)
		}
		t.Logf("%s: %d", name, c.Load())
	}
}

// TestGameLoopRestart plays the league for simDownAt steps, takes the
// backend down (the store closed: no timers, no requests) while the game
// clock moves on by the downtime, starts it again in a new epoch, and plays
// as many steps more. For each downtime:
//
//   - 0: the state must survive the restart exactly (JSON, version aside)
//   - 90 s, shorter than the league's attack interval (simAttackInterval)
//   - 15 min, several attacks of every boss
//   - 60 min, long enough that teams are knocked out while it's down
//
// After the restart every team must equal its model at the new time (the
// attacks that fell due while down applied at load, knockouts included),
// play must go on, and every phone, handed its team's snapshot as on
// reconnect, must end on its team's state.
func TestGameLoopRestart(t *testing.T) {
	for _, down := range []time.Duration{0, 90 * time.Second, 15 * time.Minute, time.Hour} {
		t.Run(down.String(), func(t *testing.T) {
			l := newLeague(t, true)
			var counts simCounts
			l.watchAll(t)
			l.steps(t, 1, simDownAt, &counts)
			knockedBefore := counts.knockouts.Load()
			l.downAndRestart(t, down, simDownAt, &counts)
			knockedWhileDown := counts.knockouts.Load() - knockedBefore
			l.steps(t, simDownAt+1, 2*simDownAt, &counts)
			l.converge(t)
			t.Logf("knocked out while down: %d; casts: %d", knockedWhileDown, counts.casts.Load())
			if down >= time.Hour && knockedWhileDown == 0 {
				t.Errorf("an hour down should knock some teams out")
			}
		})
	}
}

// watchAll picks each team's watcher (a member never removed) and starts
// watching its events.
func (l *league) watchAll(t *testing.T) {
	for _, st := range l.sims {
		st.watcher = 0
		if st.strategy == "shrinking" && len(st.members) > 1 {
			st.removed = len(st.members) - 1
			if st.i%2 == 0 {
				st.removed = 0 // the leader
			}
			st.watcher = (st.removed + 1) % len(st.members)
		}
		l.watch(t, st)
	}
}

// steps plays steps from to to (inclusive): each moves the clock by simStep
// (firing due attacks), has every team act at once by its strategy, then
// checks every team against its model.
func (l *league) steps(t *testing.T, from, to int, counts *simCounts) {
	t.Helper()
	for step := from; step <= to; step++ {
		l.elapsed += simStep
		l.clock.at(l.elapsed)
		var wg sync.WaitGroup
		for _, st := range l.sims {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.play(t, st, step, counts)
			}()
		}
		wg.Wait()
		if t.Failed() {
			t.Fatalf("step %d failed", step)
		}
		for _, st := range l.sims {
			l.check(t, st, step, counts)
		}
	}
}

// watch subscribes st's watcher and keeps the newest team_state message it
// receives, until the subscription ends.
func (l *league) watch(t *testing.T, st *simTeam) {
	sub := l.hub.Subscribe(st.members[st.watcher])
	t.Cleanup(sub.Close)
	go func() {
		for ev := range sub.C {
			if ev.Name() != "team_state" {
				continue
			}
			var env struct {
				Message json.RawMessage `json:"message"`
			}
			if err := json.Unmarshal(ev.Data(), &env); err != nil {
				continue
			}
			st.mu.Lock()
			st.last = env.Message
			st.mu.Unlock()
		}
	}()
}

// view returns st's state as its watcher sees it.
func (l *league) view(t *testing.T, st *simTeam) teamstate.View {
	return pgtest.Must(l.state.ForUser(st.members[st.watcher]))
}

// converge waits until every watcher's newest team_state is its team's
// state, version included: every change was published, in order.
func (l *league) converge(t *testing.T) {
	t.Helper()
	for _, st := range l.sims {
		want := pgtest.Must(json.Marshal(l.view(t, st)))
		deadline := time.Now().Add(2 * time.Second)
		for {
			st.mu.Lock()
			got := st.last
			st.mu.Unlock()
			if string(got) == string(want) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("team %d (%s): the watcher's last team_state isn't the team's state\ngot:  %s\nwant: %s", st.i, st.strategy, got, want)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
}

// downAndRestart takes the backend down for down: it waits for every event
// to arrive, closes the store (so no timer fires), moves the clock on by
// down, and loads a new store from the same database in a new epoch, as a
// backend restart would. With no downtime every team's state must survive
// exactly (JSON, version aside); after any downtime it must equal its model
// at the new time, the attacks that fell due while down included. Each
// watcher then gets its team's snapshot, as a phone reconnecting to the new
// process would.
func (l *league) downAndRestart(t *testing.T, down time.Duration, step int, counts *simCounts) {
	t.Helper()
	l.converge(t) // nothing from the old store still in flight
	before := map[string]string{}
	for _, st := range l.sims {
		before[st.id] = withoutVersionJSON(t, l.view(t, st))
	}
	l.state.Close()
	l.elapsed += down
	l.clock.at(l.elapsed) // fires nothing: the store is closed
	l.rules.Epoch++
	l.state = pgtest.Must(teamstate.Load(context.Background(), pgtest.Open(t, l.dbURL), l.atlas, l.bosses, l.teams, passQR{}, l.hub, pgtest.Quiet, l.rules))
	for _, st := range l.sims {
		if after := withoutVersionJSON(t, l.view(t, st)); down == 0 && after != before[st.id] {
			t.Fatalf("team %d (%s) across the restart\nbefore: %s\nafter:  %s", st.i, st.strategy, before[st.id], after)
		}
		l.check(t, st, step, counts)
		ev, ok := l.state.Snapshot(st.members[st.watcher])
		if !ok {
			t.Fatalf("team %d: no snapshot after the restart", st.i)
		}
		var env struct {
			Message json.RawMessage `json:"message"`
		}
		pgtest.Must(0, json.Unmarshal(ev.Data(), &env))
		st.mu.Lock()
		st.last = env.Message
		st.mu.Unlock()
	}
}

// withoutVersionJSON is v as JSON without its version, which starts again
// after a restart.
func withoutVersionJSON(t *testing.T, v teamstate.View) string {
	var m map[string]any
	pgtest.Must(0, json.Unmarshal(pgtest.Must(json.Marshal(v)), &m))
	delete(m, "version")
	return string(pgtest.Must(json.Marshal(m)))
}

// check compares st's state with its model after a step: bosses, damage,
// health, knockout, attacks, the next attack and the cast count. It also
// notes a new knockout.
func (l *league) check(t *testing.T, st *simTeam, step int, counts *simCounts) {
	t.Helper()
	now := l.clock.Now()
	v := l.view(t, st)
	m := &st.model
	health, attacks := m.health(now, l.bosses)
	fail := func(what string, got, want any) {
		t.Fatalf("step %d, team %d (%s, %d members): %s is %v, want %v\nstate: %+v", step, st.i, st.strategy, len(st.members), what, got, want, v)
	}
	if v.BossesDefeated != m.defeated {
		fail("bosses_defeated", v.BossesDefeated, m.defeated)
	}
	if v.Health != health {
		fail("health", v.Health, health)
	}
	if v.KnockedOut != (health == 0) {
		fail("knocked_out", v.KnockedOut, health == 0)
	}
	if v.CastCount != m.casts {
		fail("cast_count", v.CastCount, m.casts)
	}
	if v.DamageMultiplier != m.mult {
		fail("damage_multiplier", v.DamageMultiplier, m.mult)
	}
	unlocked := m.unlocked(l.rules.LockedPatternPoints, l.rules.UnlockAfterBosses, l.rules.MaxPatternPoints)
	if v.UnlockedPoints != unlocked || slices.Contains(v.LockedShapes, "pentagon") != (unlocked < 5) ||
		slices.Contains(v.LockedShapes, "hexagon") != (unlocked < 6) {
		fail("unlocked_points / locked_shapes", fmt.Sprint(v.UnlockedPoints, v.LockedShapes), unlocked)
	}
	if m.defeated >= l.bosses.Count() {
		if v.Boss != nil {
			fail("boss after every boss", v.Boss, nil)
		}
		return
	}
	if v.Boss == nil || v.Boss.Serial != m.defeated+1 || v.Boss.Damage != m.damage || v.Boss.Attacks != attacks ||
		!v.Boss.PhaseStartedAt.Equal(m.phaseStart) {
		fail("boss", v.Boss, fmt.Sprintf("serial %d, damage %d, %d attacks since %v", m.defeated+1, m.damage, attacks, m.phaseStart))
	}
	if health == 0 {
		if v.Boss.NextAttackAt != nil {
			fail("next_attack_at when knocked out", *v.Boss.NextAttackAt, nil)
		}
		if st.koStep < 0 {
			st.koStep = step
			counts.knockouts.Add(1)
		}
		return
	}
	st.koStep = -1
	next := m.phaseStart.Add(time.Duration(attacks+1) * l.bosses.At(m.defeated).AttackInterval())
	if v.Boss.NextAttackAt == nil || !v.Boss.NextAttackAt.Equal(next) {
		fail("next_attack_at", v.Boss.NextAttackAt, next)
	}
}

// play is one step of st's strategy. Errors go through t.Errorf: it runs in
// its own goroutine.
func (l *league) play(t *testing.T, st *simTeam, step int, counts *simCounts) {
	now := l.clock.Now()
	health, _ := st.model.health(now, l.bosses)
	knocked := health == 0
	every := func(n int) bool { return (step+st.i)%n == 0 }

	switch st.strategy {
	case "steady", "multiplier":
		if every(2) {
			l.castRandom(t, st, knocked, counts, rotate(st))
		}
	case "rush":
		l.castRandom(t, st, knocked, counts, rotate(st))
	case "solo":
		if every(3) {
			l.castRandom(t, st, knocked, counts, func(int) int { return 0 })
		}
	case "idle":
		if step == simReviveIdleAt && knocked {
			l.revive(t, st, counts)
		}
	case "sloppy":
		if every(3) {
			l.sloppy(t, st, knocked, counts)
		}
	case "retrier":
		if every(2) {
			l.castPattern(t, st, l.randomPattern(st, 1), knocked, counts, rotate(st), true)
		}
	case "quitter":
		if every(2) {
			if st.rng.IntN(2) == 0 {
				l.quit(t, st, knocked, counts)
			} else {
				l.castRandom(t, st, knocked, counts, rotate(st))
			}
		}
	case "repeater":
		if every(2) {
			l.castPattern(t, st, l.atlas.ShapePatterns(0)[0], knocked, counts, rotate(st), false)
		}
	case "slowpoke":
		if knocked && st.koStep >= 0 && step >= st.koStep+simReviveDelay {
			l.revive(t, st, counts)
		} else if every(8) {
			l.castRandom(t, st, knocked, counts, rotate(st))
		}
	case "racer":
		if every(2) {
			l.race(t, st, knocked, counts)
		}
	case "shrinking":
		if step == simRemoveAt {
			l.remove(t, st, counts)
		}
		if st.gone && every(5) {
			if _, err := l.state.Scan(context.Background(), st.members[st.removed], "1001"); !errors.Is(err, teamstate.ErrNotInTeam) {
				t.Errorf("team %d: the removed member's scan: %v, want not_in_team", st.i, err)
			}
			counts.refusedRemoved.Add(1)
		}
		if every(2) {
			l.castRandom(t, st, knocked, counts, rotate(st))
		}
	}
}

// rotate returns a scanner choice that takes turns among st's current
// members (skipping a removed one).
func rotate(st *simTeam) func(k int) int {
	return func(k int) int {
		active := []int{}
		for m := range st.members {
			if m != st.removed || !st.gone {
				active = append(active, m)
			}
		}
		return active[k%len(active)]
	}
}

// randomPattern picks a random pattern of at least minPoints waypoints:
// one time in five a big (locked until the unlock) one, so every team tries
// them before and after.
func (l *league) randomPattern(st *simTeam, minPoints int) int {
	if len(l.big) > 0 && st.rng.IntN(5) == 0 {
		return l.big[st.rng.IntN(len(l.big))]
	}
	for {
		p := st.rng.IntN(l.atlas.PatternCount())
		if l.atlas.Pattern(p).Waypoints.Len() >= minPoints {
			return p
		}
	}
}

// castRandom casts a random pattern, scanned by who(k) for its k-th point.
func (l *league) castRandom(t *testing.T, st *simTeam, knocked bool, counts *simCounts, who func(int) int) {
	l.castPattern(t, st, l.randomPattern(st, 1), knocked, counts, who, false)
}

// castPattern selects pattern p's shape and scans its points (each twice if
// resend: the resend must be refused), then checks the cast against the
// model. A knocked-out team's select must be refused instead.
func (l *league) castPattern(t *testing.T, st *simTeam, p int, knocked bool, counts *simCounts, who func(int) int, resend bool) {
	ctx := context.Background()
	pat := l.atlas.Pattern(p)
	shape := l.atlas.Shape(pat.Shape).ID
	if l.lockedFor(t, st, pat, shape, knocked, counts) {
		return
	}
	if !l.selectOK(t, st, shape, knocked, counts) {
		return
	}
	var res teamstate.ScanResult
	ids := l.atlas.WaypointIDs(pat.Waypoints)
	for k, w := range ids {
		var err error
		res, err = l.state.Scan(ctx, st.members[who(k)], w)
		if err != nil {
			t.Errorf("team %d (%s): scan %s: %v", st.i, st.strategy, w, err)
			return
		}
		if resend {
			_, err := l.state.Scan(ctx, st.members[who(k+1)], w)
			want := teamstate.ErrWaypointAlreadyScanned
			if k == len(ids)-1 {
				want = teamstate.ErrNoShapeSelected // the cast purged the selection
			}
			if !errors.Is(err, want) {
				t.Errorf("team %d: resent scan %s: %v, want %v", st.i, w, err, want)
			}
			counts.retries.Add(1)
		}
	}
	l.expectCast(t, st, p, res, counts)
}

// lockedFor reports whether pat is locked for st (bigger than the model's
// unlocked size; the big patterns' shapes hold nothing smaller), in which
// case selecting its shape must be refused with shape_locked (or
// knocked_out first, the check before it) and the caller stops.
func (l *league) lockedFor(t *testing.T, st *simTeam, pat atlas.Pattern, shape string, knocked bool, counts *simCounts) bool {
	if pat.Waypoints.Len() <= st.model.unlocked(l.rules.LockedPatternPoints, l.rules.UnlockAfterBosses, l.rules.MaxPatternPoints) {
		return false
	}
	_, err := l.state.Select(context.Background(), st.members[rotate(st)(0)], shape)
	want := teamstate.ErrShapeLocked
	if knocked {
		want = teamstate.ErrKnockedOut
	}
	if !errors.Is(err, want) {
		t.Errorf("team %d (%s): selecting locked %s: %v, want %v", st.i, st.strategy, shape, err, want)
	}
	counts.lockedRefused.Add(1)
	return true
}

// selectOK selects shape for st, expecting knocked_out if the team is
// knocked out (and counting it); reports whether the team may go on.
func (l *league) selectOK(t *testing.T, st *simTeam, shape string, knocked bool, counts *simCounts) bool {
	m := rotate(st)(0) // the leader: the first active member, member 1 once an admin removed member 0
	_, err := l.state.Select(context.Background(), st.members[m], shape)
	if knocked {
		if !errors.Is(err, teamstate.ErrKnockedOut) {
			t.Errorf("team %d (%s): select while knocked out: %v", st.i, st.strategy, err)
		}
		counts.knockedRefused.Add(1)
		return false
	}
	if err != nil {
		t.Errorf("team %d (%s): select %s: %v", st.i, st.strategy, shape, err)
		return false
	}
	return true
}

// expectCast checks a completing scan's result against the model's cast.
func (l *league) expectCast(t *testing.T, st *simTeam, p int, res teamstate.ScanResult, counts *simCounts) {
	m := &st.model
	before := m.defeated
	already := m.patterns[p]
	m.patterns[p] = true
	dealt, boss, defeated := m.cast(l.clock.Now(), l.atlas.Pattern(p).Spell.Damage, l.bosses)
	lc := res.State.LastCast
	if res.Outcome != teamstate.OutcomePatternCompleted || lc == nil || lc.Dealt != dealt || lc.DefeatedBoss != defeated ||
		res.AlreadyCompleted != already || (boss == 0) != (lc.Boss == nil) || (lc.Boss != nil && *lc.Boss != boss) {
		t.Errorf("team %d (%s): cast %+v (outcome %s, already %v), want dealt %d to boss %d, defeated %v, already %v",
			st.i, st.strategy, lc, res.Outcome, res.AlreadyCompleted, dealt, boss, defeated, already)
		return
	}
	counts.casts.Add(1)
	if l.atlas.Pattern(p).Waypoints.Len() >= l.rules.LockedPatternPoints {
		counts.bigCasts.Add(1)
	}
	if already {
		counts.already.Add(1)
	}
	if defeated {
		counts.defeats.Add(1)
		if before+1 == l.bosses.Count() {
			counts.allDefeated.Add(1)
		}
	}
}

// sloppy scans a pattern's first point, then a waypoint that can't fit the
// shape (wrong_scan, poisoned), has a rescan refused, then reselects and
// casts the pattern.
func (l *league) sloppy(t *testing.T, st *simTeam, knocked bool, counts *simCounts) {
	ctx := context.Background()
	p := l.randomPattern(st, 2)
	pat := l.atlas.Pattern(p)
	ids := l.atlas.WaypointIDs(pat.Waypoints)
	first := pgtest.Must(l.atlas.SetOf(ids[:1]))
	wrong := ""
	for w := range l.atlas.WaypointCount() {
		if !first.Has(w) && !l.atlas.FitsShape(pat.Shape, first.With(w)) {
			wrong = l.atlas.Waypoint(w).ID
			break
		}
	}
	if wrong == "" { // every waypoint fits with this one: just cast
		l.castPattern(t, st, p, knocked, counts, rotate(st), false)
		return
	}
	if l.lockedFor(t, st, pat, l.atlas.Shape(pat.Shape).ID, knocked, counts) {
		return
	}
	if !l.selectOK(t, st, l.atlas.Shape(pat.Shape).ID, knocked, counts) {
		return
	}
	who := rotate(st)
	if _, err := l.state.Scan(ctx, st.members[who(0)], ids[0]); err != nil {
		t.Errorf("team %d: first scan: %v", st.i, err)
		return
	}
	res, err := l.state.Scan(ctx, st.members[who(1)], wrong)
	if err != nil || res.Outcome != teamstate.OutcomeWrongScan || !res.State.Poisoned {
		t.Errorf("team %d: wrong scan %s: %v %+v", st.i, wrong, err, res)
		return
	}
	counts.wrongScans.Add(1)
	if _, err := l.state.Scan(ctx, st.members[who(2)], ids[1]); !errors.Is(err, teamstate.ErrSelectionPoisoned) {
		t.Errorf("team %d: scan on a poisoned attempt: %v", st.i, err)
	}
	counts.poisoned.Add(1)
	l.castPattern(t, st, p, false, counts, who, false) // reselecting purges it
}

// quit selects, scans one point, and gives up: the selection and stack must
// be empty after.
func (l *league) quit(t *testing.T, st *simTeam, knocked bool, counts *simCounts) {
	ctx := context.Background()
	p := l.randomPattern(st, 2)
	pat := l.atlas.Pattern(p)
	if l.lockedFor(t, st, pat, l.atlas.Shape(pat.Shape).ID, knocked, counts) {
		return
	}
	if !l.selectOK(t, st, l.atlas.Shape(pat.Shape).ID, knocked, counts) {
		return
	}
	if _, err := l.state.Scan(ctx, st.members[rotate(st)(0)], l.atlas.WaypointIDs(pat.Waypoints)[0]); err != nil {
		t.Errorf("team %d: scan before giving up: %v", st.i, err)
		return
	}
	v, err := l.state.ClearSelection(ctx, st.members[rotate(st)(0)])
	if err != nil || v.Selection != nil || len(v.Stack) != 0 {
		t.Errorf("team %d: clear: %v %+v", st.i, err, v)
		return
	}
	counts.cleared.Add(1)
}

// race has two members (or one member twice, for a team of one) scan a
// pattern's first point at the same moment: exactly one is accepted, the
// other refused as already scanned; then the rest is scanned and cast.
func (l *league) race(t *testing.T, st *simTeam, knocked bool, counts *simCounts) {
	ctx := context.Background()
	p := l.randomPattern(st, 2)
	pat := l.atlas.Pattern(p)
	if l.lockedFor(t, st, pat, l.atlas.Shape(pat.Shape).ID, knocked, counts) {
		return
	}
	if !l.selectOK(t, st, l.atlas.Shape(pat.Shape).ID, knocked, counts) {
		return
	}
	ids := l.atlas.WaypointIDs(pat.Waypoints)
	who := rotate(st)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for r := range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[r] = l.state.Scan(ctx, st.members[who(r)], ids[0])
		}()
	}
	wg.Wait()
	if (errs[0] == nil) == (errs[1] == nil) || !errors.Is(errors.Join(errs...), teamstate.ErrWaypointAlreadyScanned) {
		t.Errorf("team %d: race on %s: %v", st.i, ids[0], errs)
		return
	}
	counts.races.Add(1)
	var res teamstate.ScanResult
	for k, w := range ids[1:] {
		var err error
		if res, err = l.state.Scan(ctx, st.members[who(k+1)], w); err != nil {
			t.Errorf("team %d: scan %s after the race: %v", st.i, w, err)
			return
		}
	}
	l.expectCast(t, st, p, res, counts)
}

// remove has an admin remove st.removed (naming member 1 the new leader when
// removing the leader), or, for a team of one, be refused the last member.
func (l *league) remove(t *testing.T, st *simTeam, counts *simCounts) {
	ctx := context.Background()
	if len(st.members) == 1 {
		if _, err := l.teams.RemoveMember(ctx, st.id, st.members[0], nil); !errors.Is(err, teams.ErrLastMember) {
			t.Errorf("team %d: removing the only member: %v", st.i, err)
		}
		counts.lastMember.Add(1)
		return
	}
	var newLeader *uuid.UUID
	if st.removed == 0 {
		newLeader = &st.members[1]
	}
	if _, err := l.teams.RemoveMember(ctx, st.id, st.members[st.removed], newLeader); err != nil {
		t.Errorf("team %d: remove member %d: %v", st.i, st.removed, err)
		return
	}
	st.gone = true
	counts.removals.Add(1)
}

// revive has an admin revive st, and the model follow.
func (l *league) revive(t *testing.T, st *simTeam, counts *simCounts) {
	v, err := l.state.Revive(context.Background(), st.id)
	if err != nil || v.Health != l.rules.Health || v.KnockedOut {
		t.Errorf("team %d (%s): revive: %v %+v", st.i, st.strategy, err, v)
		return
	}
	st.model.revive(l.clock.Now(), l.rules.Health)
	counts.revives.Add(1)
}

// BenchmarkGameLoopCast measures casts per second with simTeams teams
// playing at once: each goroutine owns a team and casts random patterns
// (select plus every scan), boss attacks off. Reported per cast.
func BenchmarkGameLoopCast(b *testing.B) {
	l := newLeague(b, false)
	var next atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := int(next.Add(1)) - 1
		if i >= simTeams {
			b.Error("more goroutines than teams; lower -cpu")
			return
		}
		st := l.sims[i]
		ctx := context.Background()
		for pb.Next() {
			pat := l.atlas.Pattern(st.rng.IntN(l.atlas.PatternCount()))
			if _, err := l.state.Select(ctx, st.members[0], l.atlas.Shape(pat.Shape).ID); err != nil {
				b.Error(err)
				return
			}
			for k, w := range l.atlas.WaypointIDs(pat.Waypoints) {
				if _, err := l.state.Scan(ctx, st.members[k%len(st.members)], w); err != nil {
					b.Error(err)
					return
				}
			}
		}
	})
}
