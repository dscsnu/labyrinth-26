package teamstate

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/teams"
)

// State is everything a team has done in the game. Snapshots are never
// modified once stored; writers build a new one.
type State struct {
	// Waypoints is every waypoint the team has ever had a scan accepted at.
	Waypoints WaypointSet
	// Patterns is every pattern the team has completed.
	Patterns PatternSet
	// Shapes is every shape the team has completed at least one pattern of.
	Shapes ShapeSet
	// Selection is the shape the team is attempting, if any.
	Selection Selection
	// Stack is the team's accepted scans since it selected its shape: what
	// completion is checked against.
	Stack Stack
}

// Selection is the shape a team has chosen to attempt.
type Selection struct {
	// Active reports whether a shape is selected; the other fields are zero
	// when it isn't.
	Active bool
	// Shape is the selected shape's index in Content.Shapes.
	Shape int
	// By is the member who selected it.
	By uuid.UUID
	// At is when it was selected.
	At time.Time
}

// StackScan is one accepted scan in a team's stack.
type StackScan struct {
	// Waypoint is the waypoint's index in Content.Waypoints.
	Waypoint int
	// UserID is the member who scanned it.
	UserID uuid.UUID
	// At is when it was scanned.
	At time.Time
}

// Stack is a team's accepted scans since it selected its shape, oldest at the
// bottom and newest on top: one scan per waypoint and per member, all within
// one pattern of the selected shape, so never more than Cap (config.toml
// game.max_pattern_points). Scans older than the scan limit (window + grace)
// are stale: Live leaves them out, and Scan drops them before pushing.
//
// It's a value: Push returns a new Stack and leaves the receiver unchanged,
// so snapshots stay immutable. Storage is a circular buffer, so pushing and
// dropping the bottom never move the other scans.
type Stack struct {
	// scans is the storage; only Cap slots are used. Sized for the largest
	// allowed team, since a pattern can't have more points than members.
	scans [config.MaxTeamSize]StackScan
	// bottom is the slot of the oldest scan.
	bottom int
	// n is how many scans the stack holds, 0…Cap.
	n int
	// Cap is the stack's capacity.
	Cap int
}

// newStack returns an empty stack of the given capacity.
func newStack(capacity int) Stack { return Stack{Cap: capacity} }

// Len returns how many scans the stack holds, stale ones included.
func (s Stack) Len() int { return s.n }

// At returns the i-th scan from the bottom: At(0) is the oldest, At(Len()-1)
// the top.
func (s Stack) At(i int) StackScan { return s.scans[(s.bottom+i)%s.Cap] }

// Push returns the stack with sc on top. Scan never pushes onto a full stack
// (the scans always fit one pattern); if it did, the bottom scan would be
// dropped.
func (s Stack) Push(sc StackScan) Stack {
	if s.n < s.Cap {
		s.scans[(s.bottom+s.n)%s.Cap] = sc
		s.n++
		return s
	}
	s.scans[s.bottom] = sc
	s.bottom = (s.bottom + 1) % s.Cap
	return s
}

// Live returns the scans no older than limit at now, bottom to top. Scans are
// pushed in time order, so the live scans are always the top of the stack.
func (s Stack) Live(now time.Time, limit time.Duration) []StackScan {
	var out []StackScan
	for i := range s.n {
		if sc := s.At(i); now.Sub(sc.At) <= limit {
			out = append(out, sc)
		}
	}
	return out
}

// View is a team's State as the API and live events show it: IDs and
// timestamps instead of indexes and bits.
type View struct {
	// TeamID is the team the state belongs to.
	TeamID string `json:"team_id"`
	// Waypoints is every waypoint ever accepted, sorted.
	Waypoints []string `json:"waypoints"`
	// Patterns is every completed pattern, each as its sorted waypoint IDs.
	Patterns [][]string `json:"patterns"`
	// Shapes is every completed shape, in index order.
	Shapes []string `json:"shapes"`
	// Selection is the selected shape, or null.
	Selection *SelectionView `json:"selection"`
	// Stack is the stack's live scans (within window + grace), bottom
	// (oldest) to top (newest): the ones that can still count toward a
	// pattern.
	Stack []StackScanView `json:"stack"`
}

// SelectionView is a Selection as JSON.
type SelectionView struct {
	// Shape is the selected shape's ID.
	Shape string `json:"shape"`
	// SelectedBy is the member who selected it.
	SelectedBy uuid.UUID `json:"selected_by"`
	// SelectedAt is when it was selected.
	SelectedAt time.Time `json:"selected_at"`
}

// StackScanView is a StackScan as JSON.
type StackScanView struct {
	// WaypointID is the scanned waypoint's 4-digit ID.
	WaypointID string `json:"waypoint_id"`
	// UserID is the member who scanned it.
	UserID uuid.UUID `json:"user_id"`
	// ScannedAt is when it was scanned.
	ScannedAt time.Time `json:"scanned_at"`
	// ExpiresAt is when this scan goes stale (ScannedAt plus window plus
	// grace); clients count down to it.
	ExpiresAt time.Time `json:"expires_at"`
}

// Game is what a client needs before playing, from GET /api/game/shapes: the
// shapes a team can select and the timing rules its countdown uses. It's the
// same for everyone and never changes while the backend runs.
type Game struct {
	// Shapes are the selectable shapes, in index order.
	Shapes []GameShape `json:"shapes"`
	// ScanWindowMS is config.toml game.scan_window in milliseconds: how long
	// the client lets a team scan after its first accepted scan.
	ScanWindowMS int64 `json:"scan_window_ms"`
	// ScanGraceMS is game.scan_grace in milliseconds: extra server-side
	// tolerance for network latency. Clients don't add it to their countdown.
	ScanGraceMS int64 `json:"scan_grace_ms"`
	// MaxPatternPoints is game.max_pattern_points.
	MaxPatternPoints int `json:"max_pattern_points"`
}

// GameShape is one selectable shape: what the client calls a pattern type.
type GameShape struct {
	// ID is what PUT /api/teams/me/state/selection takes, e.g. "square".
	ID string `json:"id"`
	// Name is for display, e.g. "Square".
	Name string `json:"name"`
	// Patterns is how many valid patterns the shape has.
	Patterns int `json:"patterns"`
	// Points is the distinct sizes of its patterns, ascending, e.g. [4].
	Points []int `json:"points"`
}

// Game returns the shapes and timing rules for GET /api/game/shapes.
func (s *Store) Game() Game { return s.game }

// newGame builds the Game from the content and rules.
func newGame(c *Content, r Rules) Game {
	g := Game{
		Shapes:           make([]GameShape, len(c.Shapes)),
		ScanWindowMS:     r.ScanWindow.Milliseconds(),
		ScanGraceMS:      r.ScanGrace.Milliseconds(),
		MaxPatternPoints: r.MaxPatternPoints,
	}
	for i, sh := range c.Shapes {
		sizes := map[int]bool{}
		for _, p := range c.shapePatterns[i] {
			sizes[c.Patterns[p].Waypoints.Len()] = true
		}
		points := make([]int, 0, len(sizes))
		for n := range sizes {
			points = append(points, n)
		}
		slices.Sort(points)
		g.Shapes[i] = GameShape{ID: sh.ID, Name: sh.Name, Patterns: len(c.shapePatterns[i]), Points: points}
	}
	return g
}

// ErrNotInTeam is the same error the teams use case returns.
var ErrNotInTeam = teams.ErrNotInTeam

// All reads come from memory. Content is immutable and needs no lock; a
// team's state is a lock-free load of its snapshot.

// Content returns the static game content.
func (s *Store) Content() *Content { return s.content }

// ForTeam returns a team's state; a team that hasn't done anything yet has
// an empty State.
func (s *Store) ForTeam(teamID string) State {
	s.mu.RLock()
	e := s.states[teamID]
	s.mu.RUnlock()
	if e == nil {
		return s.emptyState()
	}
	return *e.cur.Load()
}

// ForUser returns the state of userID's team, or ErrNotInTeam.
func (s *Store) ForUser(userID uuid.UUID) (View, error) {
	t, err := s.teams.ForUser(userID)
	if err != nil {
		return View{}, err
	}
	return s.view(t.ID, s.ForTeam(t.ID)), nil
}

// Snapshot is userID's team state as a live event, sent first on a new event
// stream. ok is false when they aren't in a team.
func (s *Store) Snapshot(userID uuid.UUID) (live.Event, bool) {
	t, err := s.teams.ForUser(userID)
	if err != nil {
		return live.Event{}, false
	}
	st := s.ForTeam(t.ID)
	ev, err := s.event(t.ID, &st)
	if err != nil {
		s.log.Error("encode team state event", "error", err)
		return live.Event{}, false
	}
	return ev, true
}

// view turns a State into IDs and timestamps for the API and live events.
// Stale stack scans are left out.
func (s *Store) view(teamID string, st State) View {
	c := s.content
	v := View{TeamID: teamID, Waypoints: c.WaypointIDs(st.Waypoints), Patterns: [][]string{}, Shapes: []string{}, Stack: []StackScanView{}}
	for _, p := range st.Patterns.Indexes() {
		v.Patterns = append(v.Patterns, c.WaypointIDs(c.Patterns[p].Waypoints))
	}
	for _, i := range st.Shapes.Indexes() {
		v.Shapes = append(v.Shapes, c.Shapes[i].ID)
	}
	if sel := st.Selection; sel.Active {
		v.Selection = &SelectionView{Shape: c.Shapes[sel.Shape].ID, SelectedBy: sel.By, SelectedAt: sel.At.UTC()}
	}
	for _, sc := range st.Stack.Live(s.now(), s.rules.limit()) {
		v.Stack = append(v.Stack, StackScanView{
			WaypointID: c.Waypoints[sc.Waypoint].ID, UserID: sc.UserID,
			ScannedAt: sc.At.UTC(), ExpiresAt: sc.At.Add(s.rules.limit()).UTC(),
		})
	}
	return v
}
