package teamstate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/teams"
)

// The in-memory layout. Team state is separate from teams: its own map and
// its own per-team lock.
//
//	atlas     Atlas                    static waypoints/shapes/patterns (atlas use case), no lock
//	clock     Clock                    now and timers (real, or a fake in tests)
//	Store.mu (RWMutex) guards:
//	  states  map[teamID]*entry        created on a team's first write
//	entry
//	  mu          Mutex                writers to this team's state queue here
//	  cur         atomic.Pointer[State] the current snapshot; readers load it lock-free
//	  stopExpiry  func() bool          cancels the team's one pending expiry timer (under mu)
//
// A State is fixed size (bitsets, a selection and a fixed-array stack), so a
// new snapshot is a plain struct copy.

// entry holds one team's state: its write lock, its current snapshot, and
// the timer for its next time-driven change.
type entry struct {
	mu  sync.Mutex
	cur atomic.Pointer[State]
	// stopExpiry cancels the pending expiry timer (scheduleExpiry), or is nil
	// if none is pending. Guarded by mu.
	stopExpiry func() bool
}

// Clock is where the store gets time from: the current time, and timers for
// changes that happen because time passed (a correct scan going stale) rather
// than because someone acted. systemClock is the real one; tests pass a fake
// they advance by hand.
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
// state once it's IN_GAME, and an IN_GAME team's roster and status never go
// back, so reading them without the team's lock is safe.
type Teams interface {
	ForUser(userID uuid.UUID) (teams.Team, error)
	ByID(teamID string) (teams.Team, bool)
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
	FitsShape(shape int, set atlas.WaypointSet) bool
	WaypointIDs(set atlas.WaypointSet) []string
	ShapePatterns(shape int) []int
	ShapeCount() int
	Waypoint(i int) atlas.Waypoint
	Shape(i int) atlas.Shape
	Pattern(i int) atlas.Pattern
}

// Rules are the scanning settings from config.toml.
type Rules struct {
	// ScanWindow is the span a pattern's scans must fit in, bottom to top
	// (game.scan_window).
	ScanWindow time.Duration
	// ScanGrace is extra tolerance on ScanWindow (game.scan_grace).
	ScanGrace time.Duration
	// MaxPatternPoints is the most waypoints in a pattern, and so the stack's
	// capacity (game.max_pattern_points).
	MaxPatternPoints int
	// WriteTimeout bounds each write-through (storage.write_timeout).
	WriteTimeout time.Duration
	// Clock is the store's time source; nil means the real one
	// (systemClock). Tests pass a fake. Not in config.toml.
	Clock Clock
}

// Limit is the most a pattern's scans may span, bottom to top, and the age
// at which a correct scan goes stale: the scan window plus its grace.
func (r Rules) Limit() time.Duration { return r.ScanWindow + r.ScanGrace }

// Store is the team state use case: each team's selection, scan stack and
// completions in memory, written through to Postgres.
type Store struct {
	rules Rules
	db    db
	atlas Atlas
	game  Game // built once from the atlas and rules
	teams Teams
	hub   *live.Hub
	log   *slog.Logger

	mu     sync.RWMutex
	states map[string]*entry

	// clock is Rules.Clock, or systemClock.
	clock Clock
}

// now returns the store's current time.
func (s *Store) now() time.Time { return s.clock.Now() }

// Load reads every team's stored state, checking it against a. Call once at
// startup, after atlas.Load and teams.Load.
func Load(ctx context.Context, pg *postgres.DB, a Atlas, t Teams, hub *live.Hub, log *slog.Logger, rules Rules) (*Store, error) {
	d := db{pool: pg.Pool()}
	stored, err := d.selectTeams(ctx, "", rules.MaxPatternPoints)
	if err != nil {
		return nil, fmt.Errorf("load team state: %w", err)
	}

	s := &Store{rules: rules, db: d, atlas: a, game: newGame(a, rules), teams: t, hub: hub, log: log, states: map[string]*entry{}, clock: rules.Clock}
	if s.clock == nil {
		s.clock = systemClock{}
	}
	for teamID, rows := range stored {
		st, err := s.stateOf(rows)
		if err != nil {
			return nil, fmt.Errorf("team %s state: %w", teamID, err)
		}
		e := &entry{}
		e.cur.Store(&st)
		s.states[teamID] = e
	}
	// Stacks from before the restart keep ageing. Timers are armed only once
	// the map is complete: one that's already due fires at once, and its
	// callback reads the map.
	for teamID, e := range s.states {
		e.mu.Lock()
		s.scheduleExpiry(e, teamID, e.cur.Load())
		e.mu.Unlock()
	}
	log.Info("loaded team state", "teams", len(s.states))
	return s, nil
}

// emptyState is the state of a team that hasn't done anything: no progress,
// no selection, and an empty stack of the configured capacity.
func (s *Store) emptyState() State {
	return State{Stack: NewStack(s.rules.MaxPatternPoints)}
}

// stateOf rebuilds a State from stored IDs, failing on any ID the atlas
// doesn't have.
func (s *Store) stateOf(rows *teamRows) (State, error) {
	st := s.emptyState()
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
	if lc := rows.lastCast; lc != nil {
		p, ok := s.atlas.PatternByID(lc.patternID)
		if !ok {
			return st, fmt.Errorf("last cast: unknown pattern %d", lc.patternID)
		}
		st.LastCast = Cast{Active: true, Pattern: p, By: lc.castBy, At: lc.castAt.UTC(), AlreadyCompleted: lc.alreadyCompleted}
	}
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
			st := s.emptyState()
			e.cur.Store(&st)
			s.states[teamID] = e
		}
		s.mu.Unlock()
	}
	e.mu.Lock()
	return e
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
	stored, err := s.db.selectTeams(ctx, teamID, s.rules.MaxPatternPoints)
	if err == nil {
		rows := stored[teamID]
		if rows == nil {
			rows = &teamRows{} // nothing stored: the empty State
		}
		var st State
		if st, err = s.stateOf(rows); err == nil {
			e.cur.Store(&st)
			s.publish(teamID, &st)
			s.scheduleExpiry(e, teamID, &st)
			return writeErr
		}
	}
	s.log.Error("reload of team state failed; keeping memory as is", "team_id", teamID, "error", err)
	return writeErr
}

// scheduleExpiry sets e's timer for st's next time-driven change: the moment
// its oldest correct scan goes stale (wrong scans never do). It cancels any
// pending timer first, so each team has at most one. Call with e.mu held,
// after every apply.
func (s *Store) scheduleExpiry(e *entry, teamID string, st *State) {
	if e.stopExpiry != nil {
		e.stopExpiry()
		e.stopExpiry = nil
	}
	at, ok := st.Stack.NextExpiry(s.rules.Limit())
	if !ok {
		return
	}
	e.stopExpiry = s.clock.AfterFunc(at.Sub(s.now()), func() { s.expire(teamID) })
}

// expire runs when a correct scan in teamID's stack goes stale. It drops the
// stale scans from memory and publishes the result, so every client sees the
// stack shrink without anyone acting, then schedules the next expiry.
//
// A memory-only change (AGENTS.md §6): nothing is persisted, because
// Postgres never stores the stack itself; it's rebuilt from team_scan by time
// on load, which drops the same scans.
func (s *Store) expire(teamID string) {
	// 1. lock
	e := s.lockTeam(teamID)
	defer e.mu.Unlock()
	cur := e.cur.Load()

	// 2. validate: is anything stale yet? (A write may have purged or
	// rebuilt the stack since this timer was set.)
	live := cur.Stack.Live(s.now(), s.rules.Limit())
	if len(live) == cur.Stack.Len() {
		s.scheduleExpiry(e, teamID, cur)
		return
	}

	// 3. build
	next := *cur
	next.Stack = NewStack(s.rules.MaxPatternPoints)
	for _, sc := range live {
		next.Stack = next.Stack.Push(sc)
	}

	// 5. apply
	e.cur.Store(&next)
	// 6. publish
	s.publish(teamID, &next)
	s.scheduleExpiry(e, teamID, &next)
}

// publish sends a team's state to its members as a "team_state" event.
func (s *Store) publish(teamID string, st *State) {
	t, ok := s.teams.ByID(teamID)
	if !ok {
		return
	}
	ev, err := s.event(teamID, st)
	if err != nil {
		s.log.Error("encode team state event", "team_id", teamID, "error", err)
		return
	}
	for _, m := range t.Members {
		s.hub.Send(m.ID, ev)
	}
}

// event encodes a team's state as a "team_state" live event.
func (s *Store) event(teamID string, st *State) (live.Event, error) {
	data, err := json.Marshal(s.view(teamID, *st))
	return live.Event{Name: "team_state", Data: data}, err
}
