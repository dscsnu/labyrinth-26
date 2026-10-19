package teamstate

import (
	"slices"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/atlas"
	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/teams"
)

// State is everything a team has done in the game. Snapshots are never
// modified once stored; writers build a new one.
type State struct {
	// Waypoints is every waypoint the team has ever had a scan accepted at.
	Waypoints atlas.WaypointSet
	// Patterns is every pattern the team has completed.
	Patterns atlas.PatternSet
	// Shapes is every shape the team has completed at least one pattern of.
	Shapes atlas.ShapeSet
	// Selection is the shape the team is attempting, if any.
	Selection Selection
	// Stack is the team's accepted scans since it selected its shape: what
	// completion is checked against.
	Stack Stack
	// LastCast is the team's most recent cast, if any. It stays until the
	// next cast (a new selection doesn't clear it).
	LastCast Cast
}

// Cast is one completed pattern: the moment a team's stack became exactly a
// pattern of its selected shape.
type Cast struct {
	// Active reports whether there is one; the other fields are zero when
	// there isn't.
	Active bool
	// Pattern is the pattern's index in the atlas.
	Pattern int
	// By is the member whose scan completed it.
	By uuid.UUID
	// At is when.
	At time.Time
	// AlreadyCompleted reports that the team had cast this pattern before,
	// so its progress didn't grow.
	AlreadyCompleted bool
}

// Selection is the shape a team has chosen to attempt.
type Selection struct {
	// Active reports whether a shape is selected; the other fields are zero
	// when it isn't.
	Active bool
	// Shape is the selected shape's index in the atlas.
	Shape int
	// By is the member who selected it.
	By uuid.UUID
	// At is when it was selected.
	At time.Time
}

// StackScan is one accepted scan in a team's stack.
type StackScan struct {
	// Waypoint is the waypoint's index in the atlas.
	Waypoint int
	// UserID is the member who scanned it.
	UserID uuid.UUID
	// At is when it was scanned.
	At time.Time
	// Wrong means the scan didn't fit the selected shape's patterns when it
	// was made. It still occupies the member's one scan and its waypoint slot,
	// poisons the selection (the set can never equal a pattern), and never
	// ages out of the stack. It is internal: View never exposes it.
	Wrong bool
}

// Stack is a team's accepted scans since it selected its shape, oldest at the
// bottom and newest on top: one scan per waypoint and per member, so never
// more than Cap (config.toml game.max_pattern_points). Correct scans older
// than the scan limit (window + grace) are stale: Live leaves them out, and
// Scan drops them before pushing. A wrong scan (one that didn't fit the
// selected shape) never goes stale and never leaves the stack until the
// selection is purged.
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

// NewStack returns an empty stack of the given capacity.
func NewStack(capacity int) Stack { return Stack{Cap: capacity} }

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

// NextExpiry returns the first instant the stack's oldest correct scan is
// stale: its time plus limit plus one microsecond, since a scan exactly limit
// old is still live (Live) and times have microsecond precision. ok is false
// if the stack holds no correct scans; wrong scans never go stale.
func (s Stack) NextExpiry(limit time.Duration) (at time.Time, ok bool) {
	for i := range s.n {
		sc := s.At(i)
		if sc.Wrong {
			continue
		}
		if exp := sc.At.Add(limit + time.Microsecond); !ok || exp.Before(at) {
			at, ok = exp, true
		}
	}
	return at, ok
}

// Live returns the scans no older than limit at now, bottom to top, plus any
// wrong scan (wrong scans never age out: they stay until the selection is
// purged). Correct scans are pushed in time order, so their live scans are
// always the top of the stack; a wrong scan may be older than the ones around
// it.
func (s Stack) Live(now time.Time, limit time.Duration) []StackScan {
	var out []StackScan
	for i := range s.n {
		if sc := s.At(i); sc.Wrong || now.Sub(sc.At) <= limit {
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
	// LastCast is the team's most recent cast, or null if it has none.
	LastCast *CastView `json:"last_cast"`
	// Stack is the stack's live scans (within window + grace), bottom
	// (oldest) to top (newest): the ones that can still count toward a
	// pattern.
	Stack []StackScanView `json:"stack"`
}

// CastView is a Cast as JSON.
type CastView struct {
	// Pattern is the cast pattern's waypoint IDs, sorted.
	Pattern []string `json:"pattern"`
	// Shape is the pattern's shape ID.
	Shape string `json:"shape"`
	// CastBy is the member whose scan completed it.
	CastBy uuid.UUID `json:"cast_by"`
	// CastAt is when.
	CastAt time.Time `json:"cast_at"`
	// AlreadyCompleted reports that the team had cast this pattern before.
	AlreadyCompleted bool `json:"already_completed"`
	// Damage is what the cast spell deals.
	Damage int `json:"damage"`
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

// DamageRange is the minimum and maximum damage across a shape's patterns.
type DamageRange struct {
	// Min is the minimum damage across the shape's patterns.
	Min int `json:"min"`
	// Max is the maximum damage across the shape's patterns.
	Max int `json:"max"`
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
	// Damage is the min and max damage over the shape's patterns.
	Damage DamageRange `json:"damage"`
}

// Game returns the shapes and timing rules for GET /api/game/shapes.
func (s *Store) Game() Game { return s.game }

// newGame builds the Game from the atlas and rules.
func newGame(a Atlas, r Rules) Game {
	g := Game{
		Shapes:           make([]GameShape, a.ShapeCount()),
		ScanWindowMS:     r.ScanWindow.Milliseconds(),
		ScanGraceMS:      r.ScanGrace.Milliseconds(),
		MaxPatternPoints: r.MaxPatternPoints,
	}
	for i := range a.ShapeCount() {
		sh := a.Shape(i)
		pIndices := a.ShapePatterns(i)
		sizes := map[int]bool{}
		minD, maxD := 0, 0
		if len(pIndices) > 0 {
			minD = a.Pattern(pIndices[0]).Spell.Damage
			maxD = minD
			for _, p := range pIndices[1:] {
				dmg := a.Pattern(p).Spell.Damage
				if dmg < minD {
					minD = dmg
				}
				if dmg > maxD {
					maxD = dmg
				}
			}
		}
		for _, p := range pIndices {
			sizes[a.Pattern(p).Waypoints.Len()] = true
		}
		points := make([]int, 0, len(sizes))
		for n := range sizes {
			points = append(points, n)
		}
		slices.Sort(points)
		g.Shapes[i] = GameShape{
			ID:       sh.ID,
			Name:     sh.Name,
			Patterns: len(pIndices),
			Points:   points,
			Damage:   DamageRange{Min: minD, Max: maxD},
		}
	}
	return g
}

// ErrNotInTeam is the same error the teams use case returns.
var ErrNotInTeam = teams.ErrNotInTeam

// All reads come from memory. The atlas is immutable and needs no lock; a
// team's state is a lock-free load of its snapshot.

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
// Stale correct scans are left out; wrong scans (which never go stale) are
// included, with no flag distinguishing them.
func (s *Store) view(teamID string, st State) View {
	c := s.atlas
	v := View{TeamID: teamID, Waypoints: c.WaypointIDs(st.Waypoints), Patterns: [][]string{}, Shapes: []string{}, Stack: []StackScanView{}}
	for _, p := range st.Patterns.Indexes() {
		v.Patterns = append(v.Patterns, c.WaypointIDs(c.Pattern(p).Waypoints))
	}
	for _, i := range st.Shapes.Indexes() {
		v.Shapes = append(v.Shapes, c.Shape(i).ID)
	}
	if sel := st.Selection; sel.Active {
		v.Selection = &SelectionView{Shape: c.Shape(sel.Shape).ID, SelectedBy: sel.By, SelectedAt: sel.At.UTC()}
	}
	if lc := st.LastCast; lc.Active {
		p := c.Pattern(lc.Pattern)
		v.LastCast = &CastView{Pattern: c.WaypointIDs(p.Waypoints), Shape: c.Shape(p.Shape).ID,
			CastBy: lc.By, CastAt: lc.At.UTC(), AlreadyCompleted: lc.AlreadyCompleted, Damage: p.Spell.Damage}
	}
	for _, sc := range st.Stack.Live(s.now(), s.rules.Limit()) {
		v.Stack = append(v.Stack, StackScanView{
			WaypointID: c.Waypoint(sc.Waypoint).ID, UserID: sc.UserID,
			ScannedAt: sc.At.UTC(), ExpiresAt: sc.At.Add(s.rules.Limit()).UTC(),
		})
	}
	return v
}
