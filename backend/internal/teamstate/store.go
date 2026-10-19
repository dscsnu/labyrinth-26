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
//	clock     Clock                    the time scans and casts are stamped with
//	Store.mu (RWMutex) guards:
//	  states  map[teamID]*entry        created on a team's first write
//	entry
//	  mu          Mutex                writers to this team's state queue here
//	  cur         atomic.Pointer[State] the current snapshot; readers load it lock-free
//	  members     []uuid.UUID          the frozen roster events go to, cached on first publish (under mu)
//
// A State is fixed size (bitsets, a selection and a fixed-array stack), so a
// new snapshot is a plain struct copy.

// entry holds one team's state: its write lock and its current snapshot.
type entry struct {
	mu  sync.Mutex
	cur atomic.Pointer[State]
	// members are the user IDs the team's events go to, cached by publish
	// the first time it sees the team IN_GAME, whose roster never changes
	// again. Nil until then. Guarded by mu.
	members []uuid.UUID
}

// Clock is where the store gets the time it stamps scans, selections and
// casts with. systemClock is the real one; tests pass a fake they set by
// hand. Nothing in the game depends on time passing: there's no time limit.
type Clock interface {
	// Now returns the current time, in UTC truncated to microseconds
	// (Postgres's precision), so a restart reproduces times exactly.
	Now() time.Time
}

// systemClock is the real Clock.
type systemClock struct{}

// Now returns the current time in UTC, truncated to microseconds.
func (systemClock) Now() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

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
	// MaxPatternPoints is the most waypoints in a pattern
	// (game.max_pattern_points); the stack holds that many plus one wrong
	// scan.
	MaxPatternPoints int
	// WriteTimeout bounds each write-through (storage.write_timeout).
	WriteTimeout time.Duration
	// Clock is the store's time source; nil means the real one
	// (systemClock). Tests pass a fake. Not in config.toml.
	Clock Clock
}

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
	stored, err := d.selectTeams(ctx, "", rules.MaxPatternPoints+1)
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
	for _, lc := range rows.casts {
		p, ok := s.atlas.PatternByID(lc.patternID)
		if !ok {
			return st, fmt.Errorf("cast: unknown pattern %d", lc.patternID)
		}
		st.Casts = append(st.Casts, Cast{Pattern: p, By: lc.castBy, At: lc.castAt.UTC(), AlreadyCompleted: lc.alreadyCompleted})
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
	stored, err := s.db.selectTeams(ctx, teamID, s.rules.MaxPatternPoints+1)
	if err == nil {
		rows := stored[teamID]
		if rows == nil {
			rows = &teamRows{} // nothing stored: the empty State
		}
		var st State
		if st, err = s.stateOf(rows); err == nil {
			e.cur.Store(&st)
			s.publish(e, teamID, &st)
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
func (s *Store) publish(e *entry, teamID string, st *State) {
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
		if t.Status == teams.InGame { // frozen from here on: safe to keep
			e.members = members
		}
	}
	ev, err := s.event(teamID, st)
	if err != nil {
		s.log.Error("encode team state event", "team_id", teamID, "error", err)
		return
	}
	for _, id := range members {
		s.hub.Send(id, ev)
	}
}

// event encodes a team's state as a "team_state" live event.
func (s *Store) event(teamID string, st *State) (live.Event, error) {
	data, err := json.Marshal(s.view(teamID, *st))
	if err != nil {
		return live.Event{}, err
	}
	return live.NewEvent("team_state", data), nil
}
