package teamstate

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/bosses"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/teams"
)

// The in-memory layout. Team state is separate from teams: its own map and
// its own per-team lock.
//
//	atlas     Atlas                    static waypoints/shapes/patterns (atlas use case), no lock
//	bosses    Bosses                   the static boss sequence (bosses use case), no lock
//	qr        QR                       verifies scanned tokens (qr use case), no lock
//	clock     Clock                    the time scans and casts are stamped with, and team timers
//	Store.mu (RWMutex) guards:
//	  states  map[teamID]*entry        created on a team's first write
//	entry
//	  mu          Mutex                writers to this team's state queue here
//	  cur         atomic.Pointer[State] the current snapshot; readers load it lock-free
//	  members     []uuid.UUID          the roster events go to, cached on first publish, dropped by RosterChanged (under mu)
//	  stopTimer   func() bool          cancels the pending timer (next attack or QR cooldown end), or nil (under mu)
//
// A State is fixed size (bitsets, a selection and a fixed-array stack), so a
// new snapshot is a plain struct copy.

// entry holds one team's state: its write lock and its current snapshot.
type entry struct {
	mu  sync.Mutex
	cur atomic.Pointer[State]
	// members are the user IDs the team's events go to, cached by publish
	// the first time it sees the team IN_GAME, and dropped by RosterChanged
	// when an admin removes a member. Nil until the next publish refills it.
	// Guarded by mu.
	members []uuid.UUID
	// stopTimer cancels the team's pending timer (schedule): its next boss
	// attack or the end of its soonest QR cooldown, whichever comes first.
	// Nil if none is pending. Guarded by mu.
	stopTimer func() bool
	// closed is set by Close: no timer is armed again, and one already
	// running does nothing. Guarded by mu.
	closed bool
}

// Clock is where the store gets time: what it stamps scans, selections and
// casts with, and timers for boss attacks, the one change caused by time
// passing rather than by someone acting. systemClock is the real one; tests
// pass a fake they advance by hand.
type Clock interface {
	// Now returns the current time, in UTC truncated to microseconds
	// (Postgres's precision), so a restart reproduces times exactly.
	Now() time.Time
	// AfterFunc calls f in its own goroutine after d (at once if d <= 0),
	// and returns a function that cancels the call if it hasn't run yet.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

// systemClock is the real Clock: time.Now and time.AfterFunc.
type systemClock struct{}

// Now returns the current time in UTC, truncated to microseconds.
func (systemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// AfterFunc schedules f with time.AfterFunc.
func (systemClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Teams is what team state needs from the teams use case. A team only gets
// state once it's IN_GAME, and an IN_GAME team's status never goes back, so
// reading it without the team's lock is safe. Its roster can only shrink, by
// an admin's removal, which teams reports through RosterChanged; writers
// re-check membership under the entry's lock (isMember).
type Teams interface {
	ForUser(userID uuid.UUID) (teams.Team, error)
	ByID(teamID string) (teams.Team, bool)
	// All is every team; Load uses it to arm every started team's attack
	// timer, including teams that haven't written anything yet.
	All() []teams.Team
}

// QR is what team state needs from the qr use case: verifying a scanned
// token to the waypoint it names. The qr use case is stateless (every token
// is derived from the secret and the clock), so Verify needs no lock and
// holds no state. It doesn't check the waypoint exists; Scan does that
// through the atlas.
type QR interface {
	// Verify checks token's signature and freshness and returns the waypoint
	// ID it carries.
	Verify(token string) (string, error)
}

// Atlas is what team state needs from the atlas use case: the static
// waypoints, shapes and patterns. The atlas never changes after startup, so
// reading it needs no lock.
type Atlas interface {
	WaypointIndex(id string) (int, bool)
	ShapeIndex(id string) (int, bool)
	PatternByID(id int) (int, bool)
	SetOf(ids []string) (atlas.WaypointSet, error)
	PatternFor(set atlas.WaypointSet) (int, bool)
	FitsShapeCastable(shape int, set atlas.WaypointSet, maxPoints int, cooldown string) bool
	WaypointIDs(set atlas.WaypointSet) []string
	ShapePatterns(shape int) []int
	ShapeCount() int
	Waypoint(i int) atlas.Waypoint
	Shape(i int) atlas.Shape
	Pattern(i int) atlas.Pattern
	SpellClasses() []atlas.SpellClass
}

// Bosses is what team state needs from the bosses use case: the static
// sequence every team fights. It never changes after startup, so reading it
// needs no lock.
type Bosses interface {
	Count() int
	At(i int) bosses.Boss
}

// Rules are the scanning settings from config.toml.
type Rules struct {
	// MaxPatternPoints is the most waypoints in a pattern
	// (game.max_pattern_points); the stack holds that many plus one wrong
	// scan.
	MaxPatternPoints int
	// Health is every team's starting health, and what a revive restores
	// (game.health).
	Health int
	// LockedPatternPoints is the size from which patterns are locked
	// (game.locked_pattern_points) until a team has defeated
	// UnlockAfterBosses bosses (game.unlock_after_bosses).
	LockedPatternPoints int
	// UnlockAfterBosses is how many bosses unlock the big patterns.
	UnlockAfterBosses int
	// WriteTimeout bounds each write-through (storage.write_timeout).
	WriteTimeout time.Duration
	// QRCooldown is how long a cast's waypoints stay disabled for the team
	// that cast it (game.qr_cooldown); 0 turns it off.
	QRCooldown time.Duration
	// PowerupMultiplier is what the next cast after a damage_multiplier
	// powerup's activation multiplies its damage by
	// (powerups.damage_multiplier).
	PowerupMultiplier float64
	// PowerupHeart is the health a heart powerup gives back, capped at
	// Health (powerups.heart).
	PowerupHeart int
	// ShortQRCooldown is how long a cast's waypoints stay disabled while a
	// short_qr_cooldown powerup is active (powerups.short_qr_cooldown).
	ShortQRCooldown time.Duration
	// ShortQRWindow is how long a short_qr_cooldown powerup stays active
	// (powerups.short_qr_window).
	ShortQRWindow time.Duration
	// OnFinished, if not nil, is called with a team's ID once a cast has
	// defeated its last boss (and at Load for a team found like that still
	// IN_GAME), after the team's lock is released: server.Build passes
	// teams' Finish, which marks it FINISHED. Called without the lock
	// because teams calls back into team state (RosterChanged) under its
	// own lock; holding both the other way round could deadlock. Not in
	// config.toml.
	OnFinished func(teamID string)
	// Clock is the store's time source; nil means the real one
	// (systemClock). Tests pass a fake. Not in config.toml.
	Clock Clock
	// Epoch is the backend process's epoch for live.Version, the same for
	// every use case: server.Build sets it once at startup. Not in
	// config.toml.
	Epoch int64
}

// Store is the team state use case: each team's selection, scan stack and
// completions in memory, written through to Postgres.
type Store struct {
	rules  Rules
	db     db
	atlas  Atlas
	bosses Bosses
	qr     QR
	game   Game // built once from the atlas and rules
	teams  Teams
	hub    *live.Hub
	log    *slog.Logger

	mu     sync.RWMutex
	states map[string]*entry

	// clock is Rules.Clock, or systemClock.
	clock Clock
}

// now returns the store's current time.
func (s *Store) now() time.Time { return s.clock.Now() }

// Load reads every team's stored state, checking it against a and b, brings
// each started team's boss attacks and QR cooldowns up to now (any that fell
// due while the backend was down), and arms each one's timer. Call once at startup,
// after atlas.Load, bosses.Load and teams.Load; Close stops the timers. qr
// verifies the tokens Scan takes.
func Load(ctx context.Context, pg *postgres.DB, a Atlas, b Bosses, t Teams, qr QR, hub *live.Hub, log *slog.Logger, rules Rules) (*Store, error) {
	d := db{pool: pg.Pool()}
	stored, err := d.selectTeams(ctx, "", rules.MaxPatternPoints+1)
	if err != nil {
		return nil, fmt.Errorf("load team state: %w", err)
	}

	s := &Store{rules: rules, db: d, atlas: a, bosses: b, qr: qr, game: newGame(a, rules), teams: t, hub: hub, log: log, states: map[string]*entry{}, clock: rules.Clock}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	for teamID, rows := range stored {
		st, err := s.stateOf(teamID, rows)
		if err != nil {
			return nil, fmt.Errorf("team %s state: %w", teamID, err)
		}
		e := &entry{}
		e.cur.Store(&st)
		s.states[teamID] = e
	}
	// Every started team is attacked, including ones with nothing stored
	// yet. The map is complete before any timer is armed, so no timer runs
	// against a half-built store.
	for _, team := range t.All() {
		if team.Status == teams.InGame {
			e := s.lockTeam(team.ID)
			e.mu.Unlock()
		}
	}
	for teamID, e := range s.states {
		e.mu.Lock()
		s.schedule(e, teamID, e.cur.Load())
		e.mu.Unlock()
	}
	// A team that defeated its last boss but wasn't marked FINISHED (the
	// backend stopped between the cast and the mark) is finished now.
	if rules.OnFinished != nil && b.Count() > 0 {
		for _, team := range t.All() {
			if team.Status == teams.InGame && s.ForTeam(team.ID).Boss.Defeated >= b.Count() {
				rules.OnFinished(team.ID)
			}
		}
	}
	log.Info("loaded team state", "teams", len(s.states))
	return s, nil
}

// Close stops every pending team timer (attacks and QR cooldown ends). Call on shutdown, and before a
// test loads another store on the same database, so a stopped store never
// attacks.
func (s *Store) Close() {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, e := range s.states {
		e.mu.Lock()
		if e.stopTimer != nil {
			e.stopTimer()
			e.stopTimer = nil
		}
		e.closed = true
		e.mu.Unlock()
	}
}

// emptyState is the state of a team that hasn't done anything: no progress,
// no selection, an empty stack of the configured capacity, full health and
// the first version of this process's epoch (seq 0). Its boss phase starts
// at the team's start (or now, for a team that hasn't started), and is then
// brought up to now.
func (s *Store) emptyState(teamID string) State {
	start := s.now()
	if t, ok := s.teams.ByID(teamID); ok && t.StartedAt != nil {
		start = t.StartedAt.UTC()
	}
	st := State{Stack: NewStack(s.rules.MaxPatternPoints), Version: live.Version{Epoch: s.rules.Epoch},
		Boss: BossProgress{Multiplier: 1, PhaseStart: start, PhaseHealth: s.rules.Health, Health: s.rules.Health}}
	st.Boss = st.Boss.at(s.now(), s.bosses)
	return st
}

// stateOf rebuilds teamID's State from stored IDs, failing on any ID the
// atlas doesn't have, and brings its boss attacks up to now.
func (s *Store) stateOf(teamID string, rows *teamRows) (State, error) {
	st := s.emptyState(teamID)
	var err error
	if st.Waypoints, err = s.atlas.SetOf(rows.waypoints); err != nil {
		return st, err
	}
	for _, id := range rows.patterns {
		p, ok := s.atlas.PatternByID(id)
		if !ok {
			return st, fmt.Errorf("unknown pattern %d", id)
		}
		st.Patterns = st.Patterns.With(p)
		st.Shapes = st.Shapes.With(s.atlas.Pattern(p).Shape)
	}
	if sel := rows.selection; sel != nil {
		shape, ok := s.atlas.ShapeIndex(sel.shapeID)
		if !ok {
			return st, fmt.Errorf("selection: unknown shape %q", sel.shapeID)
		}
		st.Selection = Selection{Active: true, Shape: shape, By: sel.selectedBy, At: sel.selectedAt.UTC()}
	}
	for _, lc := range rows.casts {
		p, ok := s.atlas.PatternByID(lc.patternID)
		if !ok {
			return st, fmt.Errorf("cast: unknown pattern %d", lc.patternID)
		}
		st.Casts = append(st.Casts, Cast{Pattern: p, By: lc.castBy, At: lc.castAt.UTC(), AlreadyCompleted: lc.alreadyCompleted,
			Dealt: lc.dealt, Boss: lc.bossSerial, DefeatedBoss: lc.defeatedBoss,
			Instant: lc.instant, Boosted: lc.boosted, ShortQR: lc.shortQR})
	}
	if bp := rows.boss; bp != nil {
		// More bosses defeated than exist means game.bosses was lowered
		// mid-event: refuse to start rather than guess.
		if bp.defeated > s.bosses.Count() {
			return st, fmt.Errorf("team_boss: %d bosses defeated, but game.bosses is %d", bp.defeated, s.bosses.Count())
		}
		st.Boss.Defeated, st.Boss.Damage, st.Boss.Multiplier = bp.defeated, bp.damage, bp.multiplier
		if bp.phaseHealth != nil {
			st.Boss.PhaseHealth = *bp.phaseHealth
		}
		if bp.phaseStartedAt != nil {
			st.Boss.PhaseStart = bp.phaseStartedAt.UTC()
		}
		st.Boss.Health = st.Boss.PhaseHealth
		st.Boss = st.Boss.at(s.now(), s.bosses)
		st.CooldownsReset = bp.cooldownsReset
	}
	for _, pw := range rows.powerups {
		k, ok := PowerupByID(pw.kind)
		if !ok {
			return st, fmt.Errorf("team_powerup: unknown kind %q", pw.kind)
		}
		if pw.held {
			st.Powerups.Held = st.Powerups.Held.With(k)
		}
		switch {
		case k == PowerupDamageMultiplier && pw.activatedCasts != nil:
			// Armed until the cast log grows past its length at activation.
			if *pw.activatedCasts > len(st.Casts) {
				return st, fmt.Errorf("team_powerup: damage_multiplier activated at %d casts, but the log has %d", *pw.activatedCasts, len(st.Casts))
			}
			st.Powerups.MultiplierArmed = *pw.activatedCasts == len(st.Casts)
		case k == PowerupShortQRCooldown && pw.activatedAt != nil:
			if until := pw.activatedAt.UTC().Add(s.rules.ShortQRWindow); s.now().Before(until) {
				st.Powerups.ShortQRUntil = until
			}
		}
	}
	st.QRCooldowns = s.qrCooldowns(st.Casts, st.CooldownsReset, s.now())
	for _, sc := range rows.stack {
		w, ok := s.atlas.WaypointIndex(sc.waypointID)
		if !ok {
			return st, fmt.Errorf("stack scan: unknown waypoint %q", sc.waypointID)
		}
		st.Stack = st.Stack.Push(StackScan{Waypoint: w, UserID: sc.userID, At: sc.scannedAt.UTC(), Wrong: sc.wrong})
	}
	return st, nil
}

// lockTeam returns teamID's entry, creating an empty one if needed, locked;
// the caller unlocks e.mu.
func (s *Store) lockTeam(teamID string) *entry {
	s.mu.RLock()
	e := s.states[teamID]
	s.mu.RUnlock()
	if e == nil {
		s.mu.Lock()
		if e = s.states[teamID]; e == nil {
			e = &entry{}
			st := s.emptyState(teamID)
			e.cur.Store(&st)
			s.states[teamID] = e
		}
		s.mu.Unlock()
	}
	e.mu.Lock()
	return e
}

// schedule arms teamID's timer for st's next time-driven change, replacing
// any pending one: its next boss attack or the end of its soonest QR
// cooldown, whichever comes first. It just cancels it if neither is due
// (every boss defeated or the team knocked out, and no QR cooldown) or the
// store is closed. The caller holds e.mu, and calls it after every apply
// that can move either: a cast, a time-driven change, a revive, a reconcile,
// and at load and start. A timer that fires early or late is harmless: due
// recomputes from the clock.
func (s *Store) schedule(e *entry, teamID string, st *State) {
	if e.stopTimer != nil {
		e.stopTimer()
		e.stopTimer = nil
	}
	if e.closed {
		return
	}
	next, ok := st.Boss.nextAttack(s.bosses)
	if len(st.QRCooldowns) > 0 && (!ok || st.QRCooldowns[0].Until.Before(next)) {
		next, ok = st.QRCooldowns[0].Until, true // the soonest QR cooldown to end
	}
	if !ok {
		return
	}
	e.stopTimer = s.clock.AfterFunc(next.Sub(s.now()), func() { s.due(teamID) })
}

// persist is the write-through step: it runs a db.go write detached from the
// request (a client disconnecting mid-commit must not leave memory unsure
// whether the database committed) and bounded by Rules.WriteTimeout.
func (s *Store) persist(ctx context.Context, write func(context.Context) error) error {
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), s.rules.WriteTimeout)
	defer cancel()
	return write(wctx)
}

// reconcile runs after a failed write-through, with e.mu held: the write may
// or may not have committed, so the team's state is re-read from Postgres,
// which wins on disagreement. Returns writeErr.
func (s *Store) reconcile(e *entry, teamID string, writeErr error) error {
	s.log.Error("team state write-through failed; reloading from database", "team_id", teamID, "error", writeErr)
	ctx, cancel := context.WithTimeout(context.Background(), s.rules.WriteTimeout)
	defer cancel()
	stored, err := s.db.selectTeams(ctx, teamID, s.rules.MaxPatternPoints+1)
	if err == nil {
		rows := stored[teamID]
		if rows == nil {
			rows = &teamRows{} // nothing stored: the empty State
		}
		var st State
		if st, err = s.stateOf(teamID, rows); err == nil {
			st.Version = e.cur.Load().Version.Next(s.rules.Epoch) // never back, even on a reload
			e.cur.Store(&st)
			s.publish(e, teamID, &st, live.Resync)
			s.schedule(e, teamID, &st)
			return writeErr
		}
	}
	s.log.Error("reload of team state failed; keeping memory as is", "team_id", teamID, "error", err)
	return writeErr
}

// publish sends a team's state to its members as a "team_state" event. The
// caller holds e.mu. The roster comes from e's cache, filled from teams on
// the first publish of an IN_GAME team, so a publish normally reads nothing
// outside the entry.
func (s *Store) publish(e *entry, teamID string, st *State, typ live.EventType) {
	members := e.members
	if members == nil {
		t, ok := s.teams.ByID(teamID)
		if !ok {
			return
		}
		members = make([]uuid.UUID, len(t.Members))
		for i, m := range t.Members {
			members[i] = m.ID
		}
		if t.Status == teams.InGame { // RosterChanged drops it on a removal
			e.members = members
		}
	}
	ev, err := s.event(teamID, st, typ)
	if err != nil {
		s.log.Error("encode team state event", "team_id", teamID, "error", err)
		return
	}
	for _, id := range members {
		s.hub.Send(id, ev)
	}
}

// event encodes a team's state as a "team_state" live event of type typ;
// its message is the same JSON as GET /api/teams/me/state.
func (s *Store) event(teamID string, st *State, typ live.EventType) (live.Event, error) {
	return live.NewEvent("team_state", typ, s.view(teamID, *st))
}

// The causes of "team_state" events (the envelope's type), besides
// live.Snapshot and live.Resync. Each goes to every member after the change;
// the message is always the whole state.
const (
	// GameStarted: the team just started; this is its state (empty for a new
	// team), sent right after the "team" event of type started.
	GameStarted live.EventType = "game_started"
	// ShapeSelected: the leader selected a shape (or reselected it), which
	// purged the stack: a spell attempt starts.
	ShapeSelected live.EventType = "shape_selected"
	// SelectionCleared: the leader cleared the selection, purging the stack.
	SelectionCleared live.EventType = "selection_cleared"
	// ScanAdded: a scan joined the stack; the attempt goes on.
	ScanAdded live.EventType = "scan_added"
	// WrongScan: a scan joined the stack marked wrong; the attempt is
	// poisoned until the leader selects again.
	WrongScan live.EventType = "wrong_scan"
	// QRCooldownEnded: by itself, with nobody acting, a QR cooldown ended:
	// waypoints of a cast game.qr_cooldown ago can be scanned again
	// (qr_cooldowns shrank). One event per change, however many ended.
	QRCooldownEnded live.EventType = "qr_cooldown_ended"
	// CooldownsReset: an admin reset the team's cooldowns: cooldown_class
	// is null and qr_cooldowns empty.
	CooldownsReset live.EventType = "cooldowns_reset"
	// SpellCast: a scan completed a pattern: last_cast is the new cast,
	// cast_count went up, the selection and stack were purged, and the
	// cast's damage went to the current boss.
	SpellCast live.EventType = "spell_cast"
	// BossDefeated: a SpellCast whose damage defeated the current boss:
	// bosses_defeated went up and boss is the next one (or null after the
	// last), whose attack clock starts now.
	BossDefeated live.EventType = "boss_defeated"
	// BossAttacked: the current boss attacked (its interval passed), with
	// nobody acting: health went down and boss.attacks up.
	BossAttacked live.EventType = "boss_attacked"
	// KnockedOut: a BossAttacked that brought health to 0: knocked_out is
	// true, and selecting and scanning are refused until a revive.
	KnockedOut live.EventType = "knocked_out"
	// Revived: an admin revived the team: health is back to game.health and
	// the current boss's attack clock starts again.
	Revived live.EventType = "revived"
)
