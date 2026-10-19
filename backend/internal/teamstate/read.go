package teamstate

import (
	"math"
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
	// Casts is every pattern the team has completed, repeats included,
	// oldest first: the cast log (team_cast). A write never appends to the
	// current snapshot's slice; it builds a new one (copy-on-write), so
	// snapshots stay immutable.
	Casts []Cast
	// Boss is the team's progress through the bosses (bosses use case).
	Boss BossProgress
	// Version orders this team's snapshots (see live.Version): every apply
	// sets the next one. Memory-only; it starts again, in a new Epoch, after a
	// restart.
	Version live.Version
}

// Poisoned reports whether the stack holds a wrong scan: the attempt can't
// cast, and the team must select again.
func (st State) Poisoned() bool {
	for i := range st.Stack.Len() {
		if st.Stack.At(i).Wrong {
			return true
		}
	}
	return false
}

// LastCast returns the team's most recent cast; ok is false if it has none.
func (st State) LastCast() (c Cast, ok bool) {
	if len(st.Casts) == 0 {
		return Cast{}, false
	}
	return st.Casts[len(st.Casts)-1], true
}

// Cast is one completed pattern: the moment a team's stack became exactly a
// pattern of its selected shape.
type Cast struct {
	// Pattern is the pattern's index in the atlas.
	Pattern int
	// By is the member whose scan completed it.
	By uuid.UUID
	// At is when.
	At time.Time
	// AlreadyCompleted reports that the team had cast this pattern before,
	// so its progress didn't grow.
	AlreadyCompleted bool
	// Dealt is the damage the cast dealt to a boss: the pattern's damage ×
	// the team's multiplier at the time, rounded; 0 if every boss was
	// already defeated (or the cast predates bosses).
	Dealt int
	// Boss is the serial of the boss it hit, or 0 if none.
	Boss int
	// DefeatedBoss reports that this cast finished Boss.
	DefeatedBoss bool
}

// BossProgress is a team's progress through the bosses, which every team
// fights in serial order. A value, so a State copy copies it.
type BossProgress struct {
	// Defeated is how many bosses the team has defeated; it's fighting the
	// boss at index Defeated (serial Defeated+1), or none once Defeated is
	// the boss count.
	Defeated int
	// Damage is what the team has dealt to its current boss so far. A cast
	// that brings it to the boss's HP defeats the boss and resets it to 0:
	// damage beyond the HP is lost, not carried to the next boss.
	Damage int
	// Multiplier is what each cast's damage is multiplied by before it's
	// dealt (rounded to an integer). 1 unless something sets it; nothing
	// does yet.
	Multiplier float64
}

// deal applies a cast of the given damage to p, against bosses: the new
// progress, the damage dealt (damage × multiplier, rounded), the serial of
// the boss hit (0 if every boss is already defeated, in which case nothing
// is dealt), and whether the cast defeated it.
func deal(p BossProgress, damage int, b Bosses) (next BossProgress, dealt, serial int, defeated bool) {
	if p.Defeated >= b.Count() {
		return p, 0, 0, false
	}
	boss := b.At(p.Defeated)
	dealt = int(math.Round(float64(damage) * p.Multiplier))
	next = p
	next.Damage += dealt
	if next.Damage >= boss.HP {
		next.Defeated++
		next.Damage = 0 // the rest of this cast is lost
		defeated = true
	}
	return next, dealt, boss.Serial, defeated
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
	// Wrong means the scan didn't fit the selected shape's patterns (with the
	// scans before it). It poisons the attempt: nothing more can be scanned
	// until the team selects again. Shown to players (StackScanView.Wrong).
	Wrong bool
}

// Stack is a team's accepted scans since it selected its shape, oldest at the
// bottom and newest on top: one scan per waypoint (any member may scan any
// number of them), all within one pattern of the shape, plus at most one
// wrong scan on top. So it never holds more than Cap = game.max_pattern_points
// + 1. There's no time limit: scans stay until the attempt casts or the team
// selects again.
//
// It's a value: Push returns a new Stack and leaves the receiver unchanged,
// so snapshots stay immutable.
type Stack struct {
	// scans is the storage; only Cap slots are used. Sized for the largest
	// allowed pattern plus one wrong scan.
	scans [config.MaxPatternPoints + 1]StackScan
	// n is how many scans the stack holds, 0…Cap.
	n int
	// Cap is the stack's capacity.
	Cap int
}

// NewStack returns an empty stack with room for a pattern of maxPoints
// waypoints plus one wrong scan.
func NewStack(maxPoints int) Stack { return Stack{Cap: maxPoints + 1} }

// Len returns how many scans the stack holds.
func (s Stack) Len() int { return s.n }

// At returns the i-th scan from the bottom: At(0) is the oldest, At(Len()-1)
// the top.
func (s Stack) At(i int) StackScan { return s.scans[i] }

// Push returns the stack with sc on top. Scan never pushes onto a full stack
// (correct scans fit one pattern, and nothing is pushed after a wrong one);
// Push panics if it's asked to, since that would be a bug.
func (s Stack) Push(sc StackScan) Stack {
	if s.n == s.Cap {
		panic("teamstate: push onto a full stack")
	}
	s.scans[s.n] = sc
	s.n++
	return s
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
	// LastCast is the team's most recent cast, or null if it has none. The
	// full history is GET /api/teams/me/casts.
	LastCast *CastView `json:"last_cast"`
	// Stack is the attempt's scans, bottom (oldest) to top (newest), each
	// marked wrong if it didn't fit.
	Stack []StackScanView `json:"stack"`
	// Poisoned reports that a scan in the stack is wrong: nothing more can be
	// scanned until the team selects again.
	Poisoned bool `json:"poisoned"`
	// BossesDefeated is how many bosses the team has defeated.
	BossesDefeated int `json:"bosses_defeated"`
	// Boss is the boss the team is fighting and the damage dealt to it so
	// far, or null once every boss is defeated. The boss itself (name, HP,
	// asset) is in GET /api/game/bosses.
	Boss *BossView `json:"boss"`
	// DamageMultiplier is what each cast's damage is multiplied by before
	// it's dealt to the boss (rounded): 1 unless something sets it.
	DamageMultiplier float64 `json:"damage_multiplier"`
	// CastCount is how many casts are in the team's cast log, repeats
	// included. A client holding the log (GET /api/teams/me/casts) re-fetches
	// it when this is more than its length plus the cast it just saw.
	CastCount int `json:"cast_count"`
	// Version orders this team's snapshots: keep a copy only if its epoch
	// differs from the one held, or it has a higher seq (live.Version).
	Version live.Version `json:"version"`
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
	// Damage is what the cast spell deals: the pattern's damage.
	Damage int `json:"damage"`
	// Dealt is the damage it dealt to a boss: Damage × the team's multiplier
	// then, rounded; 0 if every boss was already defeated.
	Dealt int `json:"dealt"`
	// Boss is the serial of the boss it hit, or null if none.
	Boss *int `json:"boss"`
	// DefeatedBoss reports that this cast defeated that boss.
	DefeatedBoss bool `json:"defeated_boss"`
}

// BossView is the boss a team is fighting, as JSON.
type BossView struct {
	// Serial is the boss's serial (see GET /api/game/bosses).
	Serial int `json:"serial"`
	// Damage is what the team has dealt to it so far, below its HP.
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
	// Wrong reports that the scan didn't fit the selected shape (with the
	// scans before it); it poisons the attempt.
	Wrong bool `json:"wrong"`
}

// DamageRange is the minimum and maximum damage across a shape's patterns.
type DamageRange struct {
	// Min is the minimum damage across the shape's patterns.
	Min int `json:"min"`
	// Max is the maximum damage across the shape's patterns.
	Max int `json:"max"`
}

// Game is what a client needs before playing, from GET /api/game/shapes: the
// shapes a team can select. It's the same for everyone and never changes
// while the backend runs.
type Game struct {
	// Shapes are the selectable shapes, in index order.
	Shapes []GameShape `json:"shapes"`
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

// Game returns the shapes for GET /api/game/shapes.
func (s *Store) Game() Game { return s.game }

// newGame builds the Game from the atlas and rules.
func newGame(a Atlas, r Rules) Game {
	g := Game{
		Shapes:           make([]GameShape, a.ShapeCount()),
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
	ev, err := s.event(t.ID, &st, live.Snapshot)
	if err != nil {
		s.log.Error("encode team state event", "error", err)
		return live.Event{}, false
	}
	return ev, true
}

// view turns a State into IDs and timestamps for the API and live events.
func (s *Store) view(teamID string, st State) View {
	c := s.atlas
	v := View{TeamID: teamID, Waypoints: c.WaypointIDs(st.Waypoints), Patterns: [][]string{}, Shapes: []string{},
		Stack: []StackScanView{}, Poisoned: st.Poisoned(), CastCount: len(st.Casts), Version: st.Version,
		BossesDefeated: st.Boss.Defeated, DamageMultiplier: st.Boss.Multiplier}
	if st.Boss.Defeated < s.bosses.Count() {
		v.Boss = &BossView{Serial: s.bosses.At(st.Boss.Defeated).Serial, Damage: st.Boss.Damage}
	}
	for _, p := range st.Patterns.Indexes() {
		v.Patterns = append(v.Patterns, c.WaypointIDs(c.Pattern(p).Waypoints))
	}
	for _, i := range st.Shapes.Indexes() {
		v.Shapes = append(v.Shapes, c.Shape(i).ID)
	}
	if sel := st.Selection; sel.Active {
		v.Selection = &SelectionView{Shape: c.Shape(sel.Shape).ID, SelectedBy: sel.By, SelectedAt: sel.At.UTC()}
	}
	if lc, ok := st.LastCast(); ok {
		cv := s.castView(lc)
		v.LastCast = &cv
	}
	for i := range st.Stack.Len() {
		sc := st.Stack.At(i)
		v.Stack = append(v.Stack, StackScanView{
			WaypointID: c.Waypoint(sc.Waypoint).ID, UserID: sc.UserID, ScannedAt: sc.At.UTC(), Wrong: sc.Wrong,
		})
	}
	return v
}

// castView turns a Cast into JSON, with its pattern's damage from the atlas.
func (s *Store) castView(lc Cast) CastView {
	p := s.atlas.Pattern(lc.Pattern)
	cv := CastView{Pattern: s.atlas.WaypointIDs(p.Waypoints), Shape: s.atlas.Shape(p.Shape).ID,
		CastBy: lc.By, CastAt: lc.At.UTC(), AlreadyCompleted: lc.AlreadyCompleted, Damage: p.Spell.Damage,
		Dealt: lc.Dealt, DefeatedBoss: lc.DefeatedBoss}
	if lc.Boss != 0 {
		serial := lc.Boss
		cv.Boss = &serial
	}
	return cv
}

// CastsForUser returns userID's team's cast log, newest first, for
// GET /api/teams/me/casts; ErrNotInTeam if they have no team. Read from
// memory, like every other read.
func (s *Store) CastsForUser(userID uuid.UUID) ([]CastView, error) {
	t, err := s.teams.ForUser(userID)
	if err != nil {
		return nil, err
	}
	st := s.ForTeam(t.ID)
	out := make([]CastView, 0, len(st.Casts))
	for i := len(st.Casts) - 1; i >= 0; i-- {
		out = append(out, s.castView(st.Casts[i]))
	}
	return out, nil
}
