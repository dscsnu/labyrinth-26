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

	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/teams"
)

// The in-memory layout. Team state is separate from teams: its own map and
// its own per-team lock.
//
//	content   *Content                 static waypoints/shapes/patterns, no lock
//	Store.mu (RWMutex) guards:
//	  states  map[teamID]*entry        created on a team's first write
//	entry
//	  mu   Mutex                       writers to this team's state queue here
//	  cur  atomic.Pointer[State]       the current snapshot; readers load it lock-free
//
// A State is fixed size (bitsets, a selection and a fixed-array stack), so a
// new snapshot is a plain struct copy.

// entry holds one team's state: its write lock and its current snapshot.
type entry struct {
	mu  sync.Mutex
	cur atomic.Pointer[State]
}

// Teams is what team state needs from the teams use case. A team only gets
// state once it's IN_GAME, and an IN_GAME team's roster and status never go
// back, so reading them without the team's lock is safe.
type Teams interface {
	ForUser(userID uuid.UUID) (teams.Team, error)
	ByID(teamID string) (teams.Team, bool)
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
}

// limit is the most a pattern's scans may span, bottom to top, and the age
// at which a scan goes stale: the scan window plus its grace.
func (r Rules) limit() time.Duration { return r.ScanWindow + r.ScanGrace }

// Store is the team state use case: each team's selection, scan stack and
// completions in memory, written through to Postgres.
type Store struct {
	rules   Rules
	db      db
	content *Content
	game    Game // built once from content and rules
	teams   Teams
	hub     *live.Hub
	log     *slog.Logger

	mu     sync.RWMutex
	states map[string]*entry

	// now is the clock, swappable in tests. Times are UTC and truncated to
	// microseconds, Postgres's precision, so a restart reproduces them exactly.
	now func() time.Time
}

// systemNow is the real clock, in UTC at Postgres's microsecond precision.
func systemNow() time.Time { return time.Now().UTC().Truncate(time.Microsecond) }

// Load reads the static content and every team's stored state, enforcing
// rules. Call once at startup, after teams.Load.
func Load(ctx context.Context, pg *postgres.DB, t Teams, hub *live.Hub, log *slog.Logger, rules Rules) (*Store, error) {
	d := db{pool: pg.Pool()}
	rows, err := d.selectContent(ctx)
	if err != nil {
		return nil, fmt.Errorf("load content: %w", err)
	}
	content, err := newContent(rows, rules.MaxPatternPoints)
	if err != nil {
		return nil, fmt.Errorf("invalid content: %w", err)
	}
	stored, err := d.selectTeams(ctx, "", rules.MaxPatternPoints)
	if err != nil {
		return nil, fmt.Errorf("load team state: %w", err)
	}

	s := &Store{rules: rules, db: d, content: content, game: newGame(content, rules), teams: t, hub: hub, log: log, states: map[string]*entry{}, now: systemNow}
	for teamID, rows := range stored {
		st, err := s.stateOf(rows)
		if err != nil {
			return nil, fmt.Errorf("team %s state: %w", teamID, err)
		}
		e := &entry{}
		e.cur.Store(&st)
		s.states[teamID] = e
	}
	log.Info("loaded team state", "teams", len(s.states),
		"waypoints", len(content.Waypoints), "shapes", len(content.Shapes), "patterns", len(content.Patterns))
	return s, nil
}

// emptyState is the state of a team that hasn't done anything: no progress,
// no selection, and an empty stack of the configured capacity.
func (s *Store) emptyState() State {
	return State{Stack: newStack(s.rules.MaxPatternPoints)}
}

// stateOf rebuilds a State from stored IDs, failing on any ID the content
// doesn't have.
func (s *Store) stateOf(rows *teamRows) (State, error) {
	st := s.emptyState()
	var err error
	if st.Waypoints, err = s.content.SetOf(rows.waypoints); err != nil {
		return st, err
	}
	for _, id := range rows.patterns {
		p, ok := s.content.patternByDBID[id]
		if !ok {
			return st, fmt.Errorf("unknown pattern %d", id)
		}
		st.Patterns = st.Patterns.With(p)
		st.Shapes = st.Shapes.With(s.content.Patterns[p].Shape)
	}
	if sel := rows.selection; sel != nil {
		shape, ok := s.content.ShapeIndex(sel.shapeID)
		if !ok {
			return st, fmt.Errorf("selection: unknown shape %q", sel.shapeID)
		}
		st.Selection = Selection{Active: true, Shape: shape, By: sel.selectedBy, At: sel.selectedAt.UTC()}
	}
	for _, sc := range rows.stack {
		w, ok := s.content.WaypointIndex(sc.waypointID)
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
			return writeErr
		}
	}
	s.log.Error("reload of team state failed; keeping memory as is", "team_id", teamID, "error", err)
	return writeErr
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
