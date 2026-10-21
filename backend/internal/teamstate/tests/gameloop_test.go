package tests

// The game-loop simulation: simTeams teams of every size play the whole loop
// on the fake clock (selecting, scanning, wrong scans, retries, races,
// casts, every boss, boss attacks, knockouts, revives, an admin removing
// members, damage multipliers, powerups, quizzes), each with its own
// configuration. After every
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
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/bosses"
	"labyrinth/internal/leaderboard"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/qte"
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
	// simQRCooldown is the league's game.qr_cooldown: shorter than
	// config.toml's, because the seed has only 11 waypoints, so its 5 minutes
	// at 3–4 waypoints a cast would leave a rushing team about one cast in
	// three steps, and nobody would reach every boss or free the 5 waypoints
	// a big pattern needs. Several steps long, so cooling QR codes still
	// refuse scans and block whole steps.
	simQRCooldown = 2 * time.Minute
	// simShortQRCooldown is the league's powerups.short_qr_cooldown: shorter
	// than simQRCooldown, so a cast during a short_qr_cooldown powerup cools
	// for a different time than one after it.
	simShortQRCooldown = time.Minute
	// simShortQRWindow is the league's powerups.short_qr_window: a few steps,
	// so windows start and end often, before and after simDownAt.
	simShortQRWindow = 3 * time.Minute
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
	// simQTEEvery is how often, in steps, the league starts a quiz, each open
	// for one step (simStep): closed when the next step moves the clock. A
	// divisor of simDownAt, so one is open when TestGameLoopRestart goes down.
	simQTEEvery = 5
)

// simStrategies are the ways a team plays; team i plays simStrategies[i %
// len] with size 1 + (i / len) % 6, so each strategy runs at several sizes.
var simStrategies = []string{
	"steady",     // casts a random pattern every 2 steps, members taking turns; every 7 tries its cooldown class (refused)
	"rush",       // casts every step: defeats every boss, then casts for nothing
	"solo",       // one member scans every point, every 3 steps
	"idle",       // never plays: knocked out by boss 1, revived, attacked again
	"sloppy",     // a wrong scan poisons the attempt; a rescan is refused; reselects and casts
	"retrier",    // sends every scan twice: the resend is refused, changing nothing
	"quitter",    // half its attempts are given up (cleared) after one scan
	"repeater",   // casts the first castable pattern in atlas order (the spell and QR cooldowns move it on): already_completed once it cycles back
	"slowpoke",   // casts every 8 steps: attacked mid-fight, knocked out, revived
	"racer",      // two members scan the same point at once: exactly one is accepted
	"shrinking",  // an admin removes a member (the leader for even teams), or is refused the last one
	"multiplier", // a stored damage multiplier (×2, ×1.5, ×0.5, ×3) scales every cast
	"pampered",   // casts every 2 steps; every 4 an admin resets its cooldowns, and it casts its last spell again at once
	"doomed",     // casts every 2 steps until an admin deletes the team (simDeleteAt); then its scans are not_in_team
	"powered",    // an admin grants it powerups; its leader uses them (multiplier, heart, short QR cooldown, instant cast) and casts
	"quizzer",    // never casts; answers each quiz right or wrong, two members at once or with a resend, or a step late (qte_closed)
}

// simDeleteAt is the step at which an admin deletes the doomed teams.
const simDeleteAt = 30

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
	qtes   *qte.Store
	qteSet qte.Rules
	hub    *live.Hub
	rules  teamstate.Rules
	clock  *clock
	sims   []*simTeam
	// quiz is the latest quiz the league started, and quizStep the step it
	// started at (0 before the first).
	quiz     qte.QTE
	quizStep int
	// elapsed is how far the fake clock is past start: each step adds
	// simStep, and downtime adds its length.
	elapsed time.Duration
	// big are the patterns of at least game.locked_pattern_points
	// waypoints (the 2026 seed's pentagons and hexagons), locked until a
	// team has defeated game.unlock_after_bosses.
	big []int
	// board is the leaderboard over the league, checked against the models.
	board *leaderboard.Leaderboard
	// hookErr holds an error from OnFinished (teams' Finish), which runs
	// without a test to report to; steps fail on it.
	hookErr atomic.Value
}

// simNowhere adds a waypoint in no pattern: in the 2026 content every
// combination of the 10 checkpoints is a pattern, so every scan fits its
// shape; scanning this one is the sloppy teams' wrong scan.
const simNowhere = `INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('2099', 'Nowhere', 0, 0);`

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
	lockedRefused, bigCasts, cooldownRefused, qrRefused, qrBlocked,
	finishedRefused, resets, deletions, deletedRefused,
	grants, wasted, boostedCasts, hearts, fullHealthRefused, shortActivations, shortCasts, shortEnded,
	instantCasts, instantBypassed, patternLockedRefused, powerupActiveRefused, notHeldRefused, notLeaderRefused,
	quizzes, qteRight, qteWrong, qteGranted, qteWasted, qteRaces, qteResent, qteClosed, qteKnocked atomic.Int64
}

// newLeague seeds the dev content and starts simTeams teams of every size,
// with boss attacks as seeded. Multiplier teams get their multiplier stored
// before the store loads.
func newLeague(t testing.TB, attacks bool) *league {
	t.Helper()
	ctx := context.Background()
	set := configtest.Settings(t)
	if set.Game.MaxPatternPoints < seedMaxPoints || set.Game.Bosses != seedBosses || set.Teams.Size < minTestTeam {
		t.Skipf("the simulation needs the 2026 seed's limits (%d points, %d bosses) and teams.size >= %d",
			seedMaxPoints, seedBosses, minTestTeam)
	}
	dbURL := pgtest.URL(t, "teamstate_sim")
	db := pgtest.Open(t, dbURL)
	if err := db.Seed2026(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, simNowhere); err != nil {
		t.Fatal(err)
	}
	l := &league{db: db, dbURL: dbURL, hub: live.NewHub(set.Live.HubShards, set.Live.SubscriberBuffer), clock: &clock{t: start}}
	teamRules := teams.Rules{
		Size: set.Teams.Size, NameMaxLength: set.Teams.NameMaxLength,
		WriteTimeout: set.Storage.WriteTimeout.Duration, CodeAttempts: set.Teams.CodeAttempts,
		OnRosterChange: func(tm teams.Team) { l.state.RosterChanged(tm.ID) },
		OnDelete:       func(id string) { l.state.TeamDeleted(id) },
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
		st.model = model{phaseStart: start, phaseHealth: set.Game.Health, mult: 1, patterns: map[int]bool{}, qr: map[int]time.Time{}}
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
		QRCooldown:        simQRCooldown,
		PowerupMultiplier: set.Powerups.DamageMultiplier, PowerupHeart: set.Powerups.Heart,
		ShortQRCooldown: simShortQRCooldown, ShortQRWindow: simShortQRWindow,
		// A team through its last boss is FINISHED, as in server.Build.
		OnFinished: func(id string) {
			if err := l.teams.Finish(context.Background(), id); err != nil {
				l.hookErr.Store(err)
			}
		},
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
	l.qteSet = qteRules(set, l.clock)
	l.qtes = pgtest.Must(qte.Load(ctx, db, l.hub, pgtest.Quiet, l.qteSet))
	t.Cleanup(func() { l.qtes.Close() })
	l.state = pgtest.Must(teamstate.Load(ctx, db, l.atlas, l.bosses, l.teams, passQR{}, l.qtes, l.hub, pgtest.Quiet, l.rules))
	l.board = leaderboard.New(l.state, l.teams, l.hub, pgtest.Quiet, leaderboard.Rules{Size: 5, PublishEvery: time.Second})
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
	// cooldown is the spell class the team can't cast: its last cast's, ""
	// before its first.
	cooldown string
	// qr is when each waypoint (by index) the team has cast opens again: its
	// latest cast's time + game.qr_cooldown. A waypoint is cooling while now
	// is before it.
	qr map[int]time.Time
	// lastDefeat is when the team's latest boss fell (the leaderboard's
	// second key); zero if none has.
	lastDefeat time.Time
	// deleted is set once an admin deleted the team.
	deleted bool
	// last is the pattern of the team's latest cast; hasLast reports one.
	last    int
	hasLast bool
	// held are the powerup kinds the team holds, by kind.
	held [4]bool
	// armed: a damage_multiplier is active; the next cast deals × it.
	armed bool
	// shortUntil is when the team's short_qr_cooldown window ends; it's
	// active while now is before it.
	shortUntil time.Time
	// answer is the team's latest quiz answer (team_state.qte), nil before
	// its first.
	answer *teamstate.QTEAnswerView
}

// qrFor returns how long a cast at now cools its waypoints: the short
// cooldown while a short_qr_cooldown window runs, else game.qr_cooldown.
func (m *model) qrFor(now time.Time, r teamstate.Rules) time.Duration {
	if now.Before(m.shortUntil) {
		return r.ShortQRCooldown
	}
	return r.QRCooldown
}

// coolFrom puts waypoint w on cooldown until end, unless an earlier cast
// already keeps it cooling longer: a waypoint cools until its latest end.
func (m *model) coolFrom(w int, end time.Time) {
	if end.After(m.qr[w]) {
		m.qr[w] = end
	}
}

// heal is a heart: health up by amount, never above full, from the same
// phase start (so the attacks so far still count).
func (m *model) heal(now time.Time, amount, full int, b *bosses.Bosses) {
	h, _ := m.health(now, b)
	m.phaseHealth += min(amount, full-h)
}

// finished reports whether the team has defeated every boss: it's FINISHED,
// and its game writes are not_in_game.
func (m *model) finished(b *bosses.Bosses) bool { return b.Count() > 0 && m.defeated >= b.Count() }

// reset is an admin's cooldown reset: nothing is on cooldown.
func (m *model) reset() { m.cooldown, m.qr = "", map[int]time.Time{} }

// cooling reports whether waypoint w is on the team's QR cooldown at now.
func (m *model) cooling(w int, now time.Time) bool {
	until, ok := m.qr[w]
	return ok && now.Before(until)
}

// qrCooldowns returns the waypoints cooling at now with their until, sorted
// by until then waypoint index: what team_state.qr_cooldowns must hold.
func (m *model) qrCooldowns(now time.Time) []teamstate.QRCooldown {
	var out []teamstate.QRCooldown
	for w, until := range m.qr {
		if now.Before(until) {
			out = append(out, teamstate.QRCooldown{Waypoint: w, Until: until})
		}
	}
	slices.SortFunc(out, func(a, b teamstate.QRCooldown) int {
		if c := a.Until.Compare(b.Until); c != 0 {
			return c
		}
		return a.Waypoint - b.Waypoint
	})
	return out
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
// (× the multiplier × factor, rounded, multiplied in that order), the boss
// hit (0 for none) and whether it fell. A defeat starts the next boss's phase
// at now with the team's health then.
func (m *model) cast(now time.Time, damage int, factor float64, b *bosses.Bosses) (dealt, boss int, defeated bool) {
	m.casts++
	if m.defeated >= b.Count() {
		return 0, 0, false
	}
	dealt = int(math.Round(float64(damage) * m.mult * factor))
	boss = m.defeated + 1
	m.damage += dealt
	if m.damage >= b.At(m.defeated).HP {
		h, _ := m.health(now, b)
		m.defeated, m.damage, m.phaseStart, m.phaseHealth = m.defeated+1, 0, now, h
		m.lastDefeat = now
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
	l.checkLeaderboard(t)

	for name, c := range map[string]*atomic.Int64{
		"casts": &counts.casts, "boss defeats": &counts.defeats, "teams through every boss": &counts.allDefeated,
		"knockouts": &counts.knockouts, "revives": &counts.revives, "wrong scans": &counts.wrongScans,
		"poisoned refusals": &counts.poisoned, "refused resends": &counts.retries, "cleared attempts": &counts.cleared,
		"repeat casts": &counts.already, "races": &counts.races, "removals": &counts.removals,
		"last-member refusals": &counts.lastMember, "removed members refused": &counts.refusedRemoved,
		"knocked-out refusals":  &counts.knockedRefused,
		"locked shapes refused": &counts.lockedRefused, "big patterns cast after the unlock": &counts.bigCasts,
		"cooldown refusals": &counts.cooldownRefused, "QR cooldown refusals": &counts.qrRefused,
		"steps with every pattern's QR codes cooling": &counts.qrBlocked,
		"finished teams refused":                      &counts.finishedRefused, "cooldown resets": &counts.resets,
		"deleted teams": &counts.deletions, "deleted teams refused": &counts.deletedRefused,
		"powerups granted": &counts.grants, "powerups wasted": &counts.wasted, "boosted casts": &counts.boostedCasts,
		"hearts used": &counts.hearts, "hearts refused at full health": &counts.fullHealthRefused,
		"short QR cooldowns activated": &counts.shortActivations, "casts in a short QR window": &counts.shortCasts,
		"short QR windows ended": &counts.shortEnded, "instant casts": &counts.instantCasts,
		"instant casts past a cooldown": &counts.instantBypassed, "locked instant casts refused": &counts.patternLockedRefused,
		"powerups refused while active": &counts.powerupActiveRefused, "powerups refused not held": &counts.notHeldRefused,
		"powerups refused for a member": &counts.notLeaderRefused,
		"quizzes":                       &counts.quizzes, "right answers": &counts.qteRight, "wrong answers": &counts.qteWrong,
		"powerups earned by a quiz": &counts.qteGranted, "quiz powerups wasted": &counts.qteWasted,
		"members answering at once": &counts.qteRaces, "refused quiz resends": &counts.qteResent,
		"late answers refused": &counts.qteClosed, "knocked-out answers refused": &counts.qteKnocked,
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
			l.checkLeaderboard(t)
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
		if step%simQTEEvery == 0 {
			l.startQuiz(t, step, counts)
		}
		var wg sync.WaitGroup
		for _, st := range l.sims {
			wg.Add(1)
			go func() {
				defer wg.Done()
				l.play(t, st, step, counts)
			}()
		}
		wg.Wait()
		if err, _ := l.hookErr.Load().(error); err != nil {
			t.Fatalf("step %d: marking a team finished: %v", step, err)
		}
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
		if st.model.deleted {
			continue // no state to converge on
		}
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
		if !st.model.deleted {
			before[st.id] = withoutVersionJSON(t, l.view(t, st))
		}
	}
	l.state.Close()
	l.qtes.Close()
	l.elapsed += down
	l.clock.at(l.elapsed) // fires nothing: the stores are closed
	l.rules.Epoch++
	pg := pgtest.Open(t, l.dbURL)
	l.qtes = pgtest.Must(qte.Load(context.Background(), pg, l.hub, pgtest.Quiet, l.qteSet))
	l.state = pgtest.Must(teamstate.Load(context.Background(), pg, l.atlas, l.bosses, l.teams, passQR{}, l.qtes, l.hub, pgtest.Quiet, l.rules))
	// A quiz started at the last step is still open after no downtime and
	// gone after any.
	if open := l.qtes.Public() != nil; open != (down == 0 && l.quizStep == step) {
		t.Fatalf("after %v down, the quiz from step %d open: %v", down, l.quizStep, open)
	}
	l.board = leaderboard.New(l.state, l.teams, l.hub, pgtest.Quiet, leaderboard.Rules{Size: 5, PublishEvery: time.Second})
	for _, st := range l.sims {
		if st.model.deleted {
			l.check(t, st, step, counts)
			continue
		}
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
// health, knockout, attacks, the next attack, the cast count and the QR
// cooldowns. It also notes a new knockout.
func (l *league) check(t *testing.T, st *simTeam, step int, counts *simCounts) {
	t.Helper()
	now := l.clock.Now()
	if st.model.deleted { // no team, no state
		if _, err := l.state.ForUser(st.members[st.watcher]); !errors.Is(err, teamstate.ErrNotInTeam) {
			t.Fatalf("step %d, team %d: a deleted team's state: %v", step, st.i, err)
		}
		return
	}
	v := l.view(t, st)
	m := &st.model
	if !m.shortUntil.IsZero() && !now.Before(m.shortUntil) {
		m.shortUntil = time.Time{} // the window ended by itself (powerup_ended)
		counts.shortEnded.Add(1)
	}
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
	if got := v.CooldownClass; (got == nil) != (m.cooldown == "") || (got != nil && *got != m.cooldown) {
		fail("cooldown_class", got, m.cooldown)
	}
	var qr []teamstate.QRCooldown
	for _, c := range v.QRCooldowns {
		w, _ := l.atlas.WaypointIndex(c.WaypointID)
		qr = append(qr, teamstate.QRCooldown{Waypoint: w, Until: c.Until})
	}
	if want := m.qrCooldowns(now); !slices.EqualFunc(qr, want, func(a, b teamstate.QRCooldown) bool {
		return a.Waypoint == b.Waypoint && a.Until.Equal(b.Until)
	}) {
		fail("qr_cooldowns", qr, want)
	}
	var held, active []string
	for k := range 4 {
		if m.held[k] {
			held = append(held, teamstate.Powerup(k).ID())
		}
	}
	if m.armed {
		active = append(active, "damage_multiplier")
	}
	if !m.shortUntil.IsZero() {
		active = append(active, "short_qr_cooldown@"+m.shortUntil.UTC().Format(time.RFC3339Nano))
	}
	var gotActive []string
	for _, a := range v.Powerups.Active {
		if a.Until != nil {
			gotActive = append(gotActive, a.Kind+"@"+a.Until.UTC().Format(time.RFC3339Nano))
		} else {
			gotActive = append(gotActive, a.Kind)
		}
	}
	if !slices.Equal(v.Powerups.Held, held) && !(len(held) == 0 && len(v.Powerups.Held) == 0) {
		fail("powerups.held", v.Powerups.Held, held)
	}
	if !slices.Equal(gotActive, active) {
		fail("powerups.active", gotActive, active)
	}
	if (v.QTE == nil) != (m.answer == nil) || (v.QTE != nil && !sameAnswer(*v.QTE, *m.answer)) {
		fail("qte", v.QTE, m.answer)
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

	// A deleted team is gone: its members have no team.
	if st.model.deleted {
		if every(3) {
			if _, err := l.state.Select(context.Background(), st.members[0], "quadrilateral"); !errors.Is(err, teamstate.ErrNotInTeam) {
				t.Errorf("team %d: a deleted team's select: %v, want not_in_team", st.i, err)
			}
			counts.deletedRefused.Add(1)
		}
		return
	}
	// A finished team plays no more: every game write is not_in_game.
	if st.model.finished(l.bosses) {
		if every(3) {
			if _, err := l.state.Select(context.Background(), st.members[rotate(st)(0)], "quadrilateral"); !errors.Is(err, teamstate.ErrNotInGame) {
				t.Errorf("team %d (%s): a finished team's select: %v, want not_in_game", st.i, st.strategy, err)
			}
			counts.finishedRefused.Add(1)
		}
		return
	}

	switch st.strategy {
	case "quizzer":
		l.quizzer(t, st, step, knocked, counts)
	case "powered":
		l.powered(t, st, step, knocked, counts)
	case "pampered":
		if every(4) && (st.model.cooldown != "" || len(st.model.qrCooldowns(now)) > 0) {
			v, err := l.state.ResetCooldowns(context.Background(), st.id)
			if err != nil || v.CooldownClass != nil || len(v.QRCooldowns) != 0 {
				t.Errorf("team %d: reset: %v %+v", st.i, err, v)
				return
			}
			st.model.reset()
			counts.resets.Add(1)
			if lc, ok := st.lastPattern(); ok { // its last spell, cooling a moment ago, casts at once
				l.castPattern(t, st, lc, knocked, counts, rotate(st), false)
			}
		} else if every(2) {
			l.castRandom(t, st, knocked, counts, rotate(st))
		}
	case "doomed":
		if step == simDeleteAt {
			if err := l.teams.Delete(context.Background(), st.id); err != nil {
				t.Errorf("team %d: delete: %v", st.i, err)
				return
			}
			st.model.deleted = true
			counts.deletions.Add(1)
		} else if every(2) {
			l.castRandom(t, st, knocked, counts, rotate(st))
		}
	case "steady", "multiplier":
		if st.strategy == "steady" && every(7) && st.model.cooldown != "" {
			l.tryCooldown(t, st, knocked, counts)
		} else if every(2) {
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
			if p, ok := l.randomPattern(st, 1, counts); ok {
				l.castPattern(t, st, p, knocked, counts, rotate(st), true)
			}
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
			if p, ok := l.repeated(st); ok {
				l.castPattern(t, st, p, knocked, counts, rotate(st), false)
			}
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
			if _, err := l.state.Scan(context.Background(), st.members[st.removed], "2001"); !errors.Is(err, teamstate.ErrNotInTeam) {
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

// castable reports whether the model lets st cast pattern p now, the unlock
// aside (lockedFor checks that): not of its cooldown class (tryCooldown
// tries those), and none of its waypoints on the QR cooldown.
func (l *league) castable(st *simTeam, p int) bool {
	pat := l.atlas.Pattern(p)
	if pat.Spell.Class == st.model.cooldown {
		return false
	}
	now := l.clock.Now()
	for _, w := range pat.Waypoints.Indexes() {
		if st.model.cooling(w, now) {
			return false
		}
	}
	return true
}

// randomPattern picks a random castable pattern of at least minPoints
// waypoints: one time in five a big (locked until the unlock) one, so every
// team tries them before and after. False, counted in qrBlocked, when the QR
// cooldown leaves none, or holds the waypoints of the big pattern an
// unlocked team picked: the team waits for a later step (casting something
// small instead would keep the big pattern's waypoints cooling for ever).
func (l *league) randomPattern(st *simTeam, minPoints int, counts *simCounts) (int, bool) {
	if len(l.big) > 0 && st.rng.IntN(5) == 0 {
		p := l.big[st.rng.IntN(len(l.big))]
		pat := l.atlas.Pattern(p)
		if l.castable(st, p) {
			return p, true
		}
		unlocked := st.model.unlocked(l.rules.LockedPatternPoints, l.rules.UnlockAfterBosses, l.rules.MaxPatternPoints)
		if pat.Spell.Class != st.model.cooldown && pat.Waypoints.Len() <= unlocked {
			counts.qrBlocked.Add(1)
			return 0, false
		}
	}
	var candidates []int
	for p := range l.atlas.PatternCount() {
		if l.atlas.Pattern(p).Waypoints.Len() >= minPoints && l.castable(st, p) {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		counts.qrBlocked.Add(1)
		return 0, false
	}
	return candidates[st.rng.IntN(len(candidates))], true
}

// repeated returns the repeater's next pattern: the first castable pattern
// smaller than the locked size, in atlas order. The spell and QR cooldowns
// move it along the first few patterns, and back to the first once its
// waypoints open again, so it casts the same patterns over and over. False
// when none is castable.
func (l *league) repeated(st *simTeam) (int, bool) {
	for p := range l.atlas.PatternCount() {
		if l.atlas.Pattern(p).Waypoints.Len() < l.rules.LockedPatternPoints && l.castable(st, p) {
			return p, true
		}
	}
	return 0, false
}

// tryCooldown tries a pattern of the team's cooldown class (one it has
// unlocked): if every unlocked pattern of its shape is of that class,
// selecting the shape must be refused (shape_on_cooldown, or knocked_out
// first); otherwise the shape selects, and scanning the pattern's points in
// order must hit a wrong scan before it could complete, after which the
// leader clears the attempt.
func (l *league) tryCooldown(t *testing.T, st *simTeam, knocked bool, counts *simCounts) {
	ctx := context.Background()
	unlocked := st.model.unlocked(l.rules.LockedPatternPoints, l.rules.UnlockAfterBosses, l.rules.MaxPatternPoints)
	var candidates []int
	for p := range l.atlas.PatternCount() {
		if pat := l.atlas.Pattern(p); pat.Spell.Class == st.model.cooldown && pat.Waypoints.Len() <= unlocked {
			candidates = append(candidates, p)
		}
	}
	if len(candidates) == 0 {
		return
	}
	pat := l.atlas.Pattern(candidates[st.rng.IntN(len(candidates))])
	shape := l.atlas.Shape(pat.Shape).ID
	onCooldown := true // every unlocked pattern of the shape is of the cooldown class
	for _, p := range l.atlas.ShapePatterns(pat.Shape) {
		if q := l.atlas.Pattern(p); q.Waypoints.Len() <= unlocked && q.Spell.Class != st.model.cooldown {
			onCooldown = false
		}
	}
	leader := st.members[rotate(st)(0)]
	_, err := l.state.Select(ctx, leader, shape)
	switch {
	case knocked:
		if !errors.Is(err, teamstate.ErrKnockedOut) {
			t.Errorf("team %d: select %s while knocked out: %v", st.i, shape, err)
		}
		counts.knockedRefused.Add(1)
		return
	case onCooldown:
		if !errors.Is(err, teamstate.ErrShapeOnCooldown) {
			t.Errorf("team %d: select %s (all %s): %v, want shape_on_cooldown", st.i, shape, st.model.cooldown, err)
		}
		counts.cooldownRefused.Add(1)
		return
	case err != nil:
		t.Errorf("team %d: select %s (not all %s): %v", st.i, shape, st.model.cooldown, err)
		return
	}
	for k, w := range l.atlas.WaypointIDs(pat.Waypoints) {
		res, err := l.state.Scan(ctx, st.members[rotate(st)(k)], w)
		if l.coolingID(st, w) { // the QR cooldown refuses it before the wrong-scan check
			if !errors.Is(err, teamstate.ErrWaypointOnCooldown) {
				t.Errorf("team %d: scanning cooling %s: %v, want waypoint_on_cooldown", st.i, w, err)
			}
			counts.qrRefused.Add(1)
			break
		}
		if err != nil || res.Outcome == teamstate.OutcomePatternCompleted {
			t.Errorf("team %d: scanning a %s pattern on cooldown: %v %s", st.i, st.model.cooldown, err, res.Outcome)
			return
		}
		if res.Outcome == teamstate.OutcomeWrongScan {
			counts.cooldownRefused.Add(1)
			break
		}
	}
	if _, err := l.state.ClearSelection(ctx, leader); err != nil {
		t.Errorf("team %d: clear after the cooldown try: %v", st.i, err)
	}
}

// coolingID reports whether the waypoint with ID id is on st's QR cooldown
// now, by the model.
func (l *league) coolingID(st *simTeam, id string) bool {
	w, _ := l.atlas.WaypointIndex(id)
	return st.model.cooling(w, l.clock.Now())
}

// castRandom casts a random pattern, scanned by who(k) for its k-th point,
// or does nothing if the QR cooldown leaves none.
func (l *league) castRandom(t *testing.T, st *simTeam, knocked bool, counts *simCounts, who func(int) int) {
	if p, ok := l.randomPattern(st, 1, counts); ok {
		l.castPattern(t, st, p, knocked, counts, who, false)
	}
}

// probeQR has the leader scan a waypoint on the team's QR cooldown, if any,
// one time in three, right after selecting (nothing scanned yet, so it can't
// be already_scanned): it must be refused with waypoint_on_cooldown,
// recording nothing.
func (l *league) probeQR(t *testing.T, st *simTeam, counts *simCounts) {
	cooling := st.model.qrCooldowns(l.clock.Now())
	if len(cooling) == 0 || st.rng.IntN(3) != 0 {
		return
	}
	w := l.atlas.Waypoint(cooling[st.rng.IntN(len(cooling))].Waypoint).ID
	if _, err := l.state.Scan(context.Background(), st.members[rotate(st)(0)], w); !errors.Is(err, teamstate.ErrWaypointOnCooldown) {
		t.Errorf("team %d (%s): scanning cooling %s: %v, want waypoint_on_cooldown", st.i, st.strategy, w, err)
	}
	counts.qrRefused.Add(1)
}

// castPattern selects pattern p's shape, sometimes probes a cooling QR code
// (probeQR), and scans its points (each twice if resend: the resend must be
// refused), then checks the cast against the model. A knocked-out team's
// select must be refused instead.
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
	l.probeQR(t, st, counts)
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
	l.expectCast(t, st, p, res, counts, false)
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

// expectCast checks a completing scan's result (or an instant cast's,
// instant) against the model's cast.
func (l *league) expectCast(t *testing.T, st *simTeam, p int, res teamstate.ScanResult, counts *simCounts, instant bool) {
	m := &st.model
	before := m.defeated
	already := m.patterns[p]
	m.patterns[p] = true
	m.cooldown = l.atlas.Pattern(p).Spell.Class
	m.last, m.hasLast = p, true
	now := l.clock.Now()
	short := now.Before(m.shortUntil)
	for _, w := range l.atlas.Pattern(p).Waypoints.Indexes() {
		m.coolFrom(w, now.Add(m.qrFor(now, l.rules)))
	}
	factor, boosted := 1.0, m.armed
	if boosted {
		factor, m.armed = l.rules.PowerupMultiplier, false
	}
	dealt, boss, defeated := m.cast(now, l.atlas.Pattern(p).Spell.Damage, factor, l.bosses)
	lc := res.State.LastCast
	if res.Outcome != teamstate.OutcomePatternCompleted || lc == nil || lc.Dealt != dealt || lc.DefeatedBoss != defeated ||
		res.AlreadyCompleted != already || (boss == 0) != (lc.Boss == nil) || (lc.Boss != nil && *lc.Boss != boss) ||
		lc.Boosted != boosted || lc.ShortQRCooldown != short || lc.Instant != instant {
		t.Errorf("team %d (%s): cast %+v (outcome %s, already %v), want dealt %d to boss %d, defeated %v, already %v",
			st.i, st.strategy, lc, res.Outcome, res.AlreadyCompleted, dealt, boss, defeated, already)
		return
	}
	counts.casts.Add(1)
	if boosted {
		counts.boostedCasts.Add(1)
	}
	if short {
		counts.shortCasts.Add(1)
	}
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
	p, ok := l.randomPattern(st, 2, counts)
	if !ok {
		return
	}
	pat := l.atlas.Pattern(p)
	ids := l.atlas.WaypointIDs(pat.Waypoints)
	first := pgtest.Must(l.atlas.SetOf(ids[:1]))
	wrong := ""
	for w := range l.atlas.WaypointCount() {
		if !first.Has(w) && !l.atlas.FitsShapeCastable(pat.Shape, first.With(w), l.rules.MaxPatternPoints, "") && !st.model.cooling(w, l.clock.Now()) {
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
	p, ok := l.randomPattern(st, 2, counts)
	if !ok {
		return
	}
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
	p, ok := l.randomPattern(st, 2, counts)
	if !ok {
		return
	}
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
	l.expectCast(t, st, p, res, counts, false)
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

// lastPattern returns the pattern of st's latest cast, if any.
func (st *simTeam) lastPattern() (int, bool) { return st.model.last, st.model.hasLast }

// checkLeaderboard ranks the models the way the README says (bosses
// defeated, then the latest defeat's time, earlier first and none last,
// then damage on the current boss, then team ID; deleted teams left out)
// and checks every team's leaderboard entry against it: rank, bosses,
// damage, finished.
func (l *league) checkLeaderboard(t *testing.T) {
	t.Helper()
	var ranked []*simTeam
	for _, st := range l.sims {
		if !st.model.deleted {
			ranked = append(ranked, st)
		}
	}
	slices.SortFunc(ranked, func(a, b *simTeam) int {
		ma, mb := a.model, b.model
		if ma.defeated != mb.defeated {
			return mb.defeated - ma.defeated
		}
		switch {
		case !ma.lastDefeat.IsZero() && mb.lastDefeat.IsZero():
			return -1
		case ma.lastDefeat.IsZero() && !mb.lastDefeat.IsZero():
			return 1
		case !ma.lastDefeat.Equal(mb.lastDefeat):
			return ma.lastDefeat.Compare(mb.lastDefeat)
		}
		if ma.damage != mb.damage {
			return mb.damage - ma.damage
		}
		return strings.Compare(a.id, b.id)
	})
	for i, st := range ranked {
		got := l.board.ForUser(st.members[st.watcher])
		if got.Teams != len(ranked) {
			t.Fatalf("the leaderboard ranks %d teams, want %d", got.Teams, len(ranked))
		}
		me := got.Me
		if me == nil || me.Rank != i+1 || me.BossesDefeated != st.model.defeated || me.Damage != st.model.damage ||
			me.Finished != st.model.finished(l.bosses) {
			t.Fatalf("team %d (%s): leaderboard entry %+v, want rank %d with %d bosses, %d damage, finished %v",
				st.i, st.strategy, me, i+1, st.model.defeated, st.model.damage, st.model.finished(l.bosses))
		}
	}
}

// powered is one step of a powered team. One step in four an admin grants
// it a random powerup (wasted when it already holds one). Otherwise its
// leader uses one held kind, picked at random: a damage multiplier arms the
// next cast (powerup_active if one is armed), a heart heals (full_health at
// full health), a short QR cooldown starts its window (powerup_active while
// one runs), an instant cast casts at once (instantCast). Knocked out, any
// use is knocked_out. Now and then a member tries to use one (not_leader)
// and the leader one not held (powerup_not_held). It casts every other step.
func (l *league) powered(t *testing.T, st *simTeam, step int, knocked bool, counts *simCounts) {
	ctx := context.Background()
	m := &st.model
	now := l.clock.Now()
	leader := st.members[rotate(st)(0)]
	if (step+st.i)%4 == 0 {
		k := teamstate.Powerup(st.rng.IntN(len(m.held)))
		res, err := l.state.GrantPowerup(ctx, st.id, k)
		if err != nil || res.Granted == m.held[k] {
			t.Errorf("team %d: grant %s: granted %v while holding %v: %v", st.i, k.ID(), res.Granted, m.held[k], err)
			return
		}
		if res.Granted {
			m.held[k] = true
			counts.grants.Add(1)
		} else {
			counts.wasted.Add(1)
		}
		return
	}
	// use has the leader use kind k, wanting err; it reports success.
	use := func(k teamstate.Powerup, want error) bool {
		_, err := l.state.Activate(ctx, leader, k)
		if !errors.Is(err, want) {
			t.Errorf("team %d: use %s: %v, want %v", st.i, k.ID(), err, want)
			return false
		}
		return err == nil
	}
	var held []teamstate.Powerup
	for k := range len(m.held) {
		if m.held[k] {
			held = append(held, teamstate.Powerup(k))
		}
	}
	if knocked {
		for _, k := range held {
			if k != teamstate.PowerupInstantCast {
				use(k, teamstate.ErrKnockedOut)
				counts.knockedRefused.Add(1)
				break
			}
		}
		return
	}
	if other := st.members[rotate(st)(1)]; other != leader && st.rng.IntN(4) == 0 {
		if _, err := l.state.Activate(ctx, other, teamstate.PowerupHeart); !errors.Is(err, teamstate.ErrNotLeader) {
			t.Errorf("team %d: a member using a powerup: %v, want not_leader", st.i, err)
		}
		counts.notLeaderRefused.Add(1)
	}
	if k := teamstate.Powerup(st.rng.IntN(3)); !m.held[k] && st.rng.IntN(3) == 0 {
		use(k, teamstate.ErrPowerupNotHeld)
		counts.notHeldRefused.Add(1)
	}
	if len(held) > 0 {
		switch k := held[st.rng.IntN(len(held))]; k {
		case teamstate.PowerupDamageMultiplier:
			if m.armed {
				use(k, teamstate.ErrPowerupActive)
				counts.powerupActiveRefused.Add(1)
			} else if use(k, nil) {
				m.held[k], m.armed = false, true
			}
		case teamstate.PowerupHeart:
			if h, _ := m.health(now, l.bosses); h >= l.rules.Health {
				use(k, teamstate.ErrFullHealth)
				counts.fullHealthRefused.Add(1)
			} else if use(k, nil) {
				m.held[k] = false
				m.heal(now, l.rules.PowerupHeart, l.rules.Health, l.bosses)
				counts.hearts.Add(1)
			}
		case teamstate.PowerupShortQRCooldown:
			if now.Before(m.shortUntil) {
				use(k, teamstate.ErrPowerupActive)
				counts.powerupActiveRefused.Add(1)
			} else if use(k, nil) {
				m.held[k], m.shortUntil = false, now.Add(l.rules.ShortQRWindow)
				counts.shortActivations.Add(1)
			}
		case teamstate.PowerupInstantCast:
			l.instantCast(t, st, leader, counts)
			return
		}
	}
	if (step+st.i)%2 == 0 {
		l.castRandom(t, st, false, counts, rotate(st))
	}
}

// instantCast has st's leader use its instant cast. Before the unlock, one
// try in three names a big pattern: pattern_locked, and it keeps the
// powerup. Otherwise it picks a pattern it has unlocked, preferring one the
// cooldowns would refuse a scan for (its class, or a cooling waypoint), and
// checks the cast against the model.
func (l *league) instantCast(t *testing.T, st *simTeam, leader uuid.UUID, counts *simCounts) {
	ctx := context.Background()
	m := &st.model
	unlocked := m.unlocked(l.rules.LockedPatternPoints, l.rules.UnlockAfterBosses, l.rules.MaxPatternPoints)
	if unlocked < l.rules.MaxPatternPoints && len(l.big) > 0 && st.rng.IntN(3) == 0 {
		p := l.big[st.rng.IntN(len(l.big))]
		if _, err := l.state.InstantCast(ctx, leader, l.atlas.WaypointIDs(l.atlas.Pattern(p).Waypoints)); !errors.Is(err, teamstate.ErrPatternLocked) {
			t.Errorf("team %d: instant-casting a locked pattern: %v, want pattern_locked", st.i, err)
		}
		counts.patternLockedRefused.Add(1)
		return
	}
	var bypass, open []int
	for p := range l.atlas.PatternCount() {
		if l.atlas.Pattern(p).Waypoints.Len() > unlocked {
			continue
		}
		if l.castable(st, p) {
			open = append(open, p)
		} else {
			bypass = append(bypass, p)
		}
	}
	pool := bypass
	if len(pool) == 0 {
		pool = open
	}
	p := pool[st.rng.IntN(len(pool))]
	v, err := l.state.InstantCast(ctx, leader, l.atlas.WaypointIDs(l.atlas.Pattern(p).Waypoints))
	if err != nil || v.LastCast == nil {
		t.Errorf("team %d: instant cast: %v", st.i, err)
		return
	}
	m.held[teamstate.PowerupInstantCast] = false
	counts.instantCasts.Add(1)
	if len(bypass) > 0 {
		counts.instantBypassed.Add(1)
	}
	l.expectCast(t, st, p, teamstate.ScanResult{Outcome: teamstate.OutcomePatternCompleted,
		AlreadyCompleted: v.LastCast.AlreadyCompleted, State: v}, counts, true)
}

// startQuiz has an admin start a quiz, open for one step, earning the kinds
// in turn; the right option turns too.
func (l *league) startQuiz(t *testing.T, step int, counts *simCounts) {
	t.Helper()
	n := int(counts.quizzes.Add(1))
	window := int(simStep / time.Second)
	l.quiz = pgtest.Must(l.qtes.Create(context.Background(), l.sims[0].members[0], qte.Draft{
		Kind: qte.KindQuiz, Powerup: teamstate.Powerup(n % 4).ID(), WindowS: &window,
		Quiz: &qte.Quiz{Question: fmt.Sprintf("Quiz %d?", n), Options: []string{"A", "B", "C", "D"}, CorrectOption: n % 4},
	}, true))
	l.quizStep = step
}

// quizzer is a quizzer team's step. At a quiz's start it answers (right
// half the time), but one time in four waits and answers the step after,
// refused as closed; a team of two or more sometimes has two members answer
// at once (exactly one recorded), else sometimes a teammate resends
// (refused). Knocked out, its answers are refused.
func (l *league) quizzer(t *testing.T, st *simTeam, step int, knocked bool, counts *simCounts) {
	ctx := context.Background()
	q := l.quiz
	late := step == l.quizStep+1 && q.ID != 0
	if (step != l.quizStep && !late) || (step == l.quizStep && st.rng.IntN(4) == 0) {
		return
	}
	if late && st.model.answer != nil && st.model.answer.QTEID == q.ID {
		return // answered in time
	}
	option := q.Quiz.CorrectOption
	if st.rng.IntN(2) == 0 {
		option = (option + 1 + st.rng.IntN(3)) % 4
	}
	who := rotate(st)
	if late || knocked {
		_, err := l.state.AnswerQTE(ctx, st.members[who(step)], q.ID, option)
		switch {
		case knocked && !late && errors.Is(err, teamstate.ErrKnockedOut):
			counts.qteKnocked.Add(1)
		case late && errors.Is(err, teamstate.ErrQTEClosed):
			counts.qteClosed.Add(1)
		default:
			t.Errorf("team %d: answering (late %v, knocked out %v): %v", st.i, late, knocked, err)
		}
		return
	}
	var v teamstate.View
	if len(st.members) > 1 && st.rng.IntN(3) == 0 {
		views, errs := make([]teamstate.View, 2), make([]error, 2)
		var wg sync.WaitGroup
		for r := range 2 {
			wg.Go(func() { views[r], errs[r] = l.state.AnswerQTE(ctx, st.members[who(step+r)], q.ID, option) })
		}
		wg.Wait()
		if (errs[0] == nil) == (errs[1] == nil) || !errors.Is(errors.Join(errs...), teamstate.ErrQTEAlreadyAnswered) {
			t.Errorf("team %d: two members answering at once: %v", st.i, errs)
			return
		}
		v = views[0]
		if errs[1] == nil {
			v = views[1]
		}
		counts.qteRaces.Add(1)
	} else {
		var err error
		if v, err = l.state.AnswerQTE(ctx, st.members[who(step)], q.ID, option); err != nil {
			t.Errorf("team %d: answering: %v", st.i, err)
			return
		}
		if st.rng.IntN(3) == 0 {
			if _, err := l.state.AnswerQTE(ctx, st.members[who(step+1)], q.ID, q.Quiz.CorrectOption); !errors.Is(err, teamstate.ErrQTEAlreadyAnswered) {
				t.Errorf("team %d: a resend: %v, want qte_already_answered", st.i, err)
				return
			}
			counts.qteResent.Add(1)
		}
	}
	kind, _ := teamstate.PowerupByID(q.Powerup)
	m := &st.model
	a := teamstate.QTEAnswerView{QTEID: q.ID, Option: option, Correct: option == q.Quiz.CorrectOption, AnsweredAt: l.clock.Now()}
	a.Granted = a.Correct && !m.held[kind]
	if v.QTE == nil {
		t.Errorf("team %d: an answer without team_state.qte", st.i)
		return
	}
	a.AnsweredBy = v.QTE.AnsweredBy // whichever member won
	m.answer = &a
	switch {
	case a.Granted:
		m.held[kind] = true
		counts.qteGranted.Add(1)
		counts.qteRight.Add(1)
	case a.Correct:
		counts.qteWasted.Add(1)
		counts.qteRight.Add(1)
	default:
		counts.qteWrong.Add(1)
	}
}

// sameAnswer reports that a and b are the same answer, times compared as
// instants.
func sameAnswer(a, b teamstate.QTEAnswerView) bool {
	at := a.AnsweredAt.Equal(b.AnsweredAt)
	a.AnsweredAt, b.AnsweredAt = time.Time{}, time.Time{}
	return at && a == b
}
