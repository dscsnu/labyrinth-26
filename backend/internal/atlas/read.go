package atlas

import (
	"fmt"
	"slices"
)

// Waypoint is a physical location with a QR code.
type Waypoint struct {
	// ID is the waypoint's identity everywhere: 4 digits, e.g. "2001".
	ID string `json:"id"`
	// Name is for display, e.g. "G Block Junc".
	Name string `json:"name"`
	// Latitude is in degrees (WGS 84), from Postgres.
	Latitude float64 `json:"latitude"`
	// Longitude is in degrees (WGS 84), from Postgres.
	Longitude float64 `json:"longitude"`
}

// Shape is a pattern type a team selects, e.g. "quadrilateral" (the client
// calls it a type).
type Shape struct {
	// ID is what a selection names, e.g. "quadrilateral".
	ID string `json:"id"`
	// Name is for display, e.g. "Quadrilateral".
	Name string `json:"name"`
}

// Pattern is one valid set of waypoints that forms a shape (the client calls
// it a shape). It's identified by that set.
type Pattern struct {
	// ID is Postgres's key for it (pattern.id); team_pattern stores it.
	ID int
	// Shape is the index of its shape.
	Shape int
	// Waypoints is its set of waypoints.
	Waypoints WaypointSet
	// Spell is what completing it casts, read from pattern.damage.
	Spell Spell
}

// Spell is what a completed pattern casts: the team's reward for it. Read
// once at startup from pattern.damage and
// pattern.spell_class_id, so reading it during a scan is a plain field
// access.
type Spell struct {
	// Damage is what the spell deals.
	Damage int
	// Class is the ID of its spell class, e.g. "class_4": the class of
	// patterns with its waypoint count.
	Class string
}

// SpellClass groups patterns by how many waypoints they have: every pattern
// of Points waypoints, whatever its shape, is in this class.
type SpellClass struct {
	// ID is the stable identifier clients see, e.g. "class_4".
	ID string `json:"id"`
	// Name is for display, e.g. "Class 4".
	Name string `json:"name"`
	// Points is the waypoint count of its patterns.
	Points int `json:"points"`
}

// Atlas is the static game content and its lookup tables. Never modified
// after Load returns, so it's safe to read from any goroutine without locks.
type Atlas struct {
	waypoints []Waypoint   // index → waypoint, in ID order
	shapes    []Shape      // index → shape, in ID order
	patterns  []Pattern    // index → pattern, in Postgres key order
	classes   []SpellClass // spell classes, ascending by points

	waypointIndex map[string]int      // waypoint ID → index
	shapeIndex    map[string]int      // shape ID → index
	patternByID   map[int]int         // Postgres pattern ID → index
	patternBySet  map[WaypointSet]int // exact waypoint set → pattern index
	shapePatterns [][]int             // shape index → its patterns
}

// WaypointCount returns how many waypoints there are; indexes are 0 to
// WaypointCount()-1.
func (a *Atlas) WaypointCount() int { return len(a.waypoints) }

// ShapeCount returns how many shapes there are; indexes are 0 to
// ShapeCount()-1.
func (a *Atlas) ShapeCount() int { return len(a.shapes) }

// PatternCount returns how many patterns there are; indexes are 0 to
// PatternCount()-1.
func (a *Atlas) PatternCount() int { return len(a.patterns) }

// Waypoint returns waypoint i.
func (a *Atlas) Waypoint(i int) Waypoint { return a.waypoints[i] }

// Shape returns shape i.
func (a *Atlas) Shape(i int) Shape { return a.shapes[i] }

// Pattern returns pattern i.
func (a *Atlas) Pattern(i int) Pattern { return a.patterns[i] }

// WaypointIndex looks up a waypoint by its 4-digit ID.
func (a *Atlas) WaypointIndex(id string) (int, bool) {
	i, ok := a.waypointIndex[id]
	return i, ok
}

// ShapeIndex looks up a shape by its ID.
func (a *Atlas) ShapeIndex(id string) (int, bool) {
	i, ok := a.shapeIndex[id]
	return i, ok
}

// PatternByID looks up a pattern by its Postgres key.
func (a *Atlas) PatternByID(id int) (int, bool) {
	i, ok := a.patternByID[id]
	return i, ok
}

// SetOf turns waypoint IDs into a WaypointSet.
func (a *Atlas) SetOf(ids []string) (WaypointSet, error) {
	var set WaypointSet
	for _, id := range ids {
		i, ok := a.waypointIndex[id]
		if !ok {
			return 0, fmt.Errorf("unknown waypoint %q", id)
		}
		set = set.With(i)
	}
	return set, nil
}

// PatternFor returns the index of the pattern made of exactly these
// waypoints.
func (a *Atlas) PatternFor(set WaypointSet) (int, bool) {
	i, ok := a.patternBySet[set]
	return i, ok
}

// ShapePatterns returns the indexes of shape's patterns. The slice is shared:
// don't modify it.
func (a *Atlas) ShapePatterns(shape int) []int { return a.shapePatterns[shape] }

// FitsShapeCastable reports whether some pattern of shape that a team may
// cast now contains every waypoint in set: whether set can still grow into
// one. Only the shape's patterns of at most maxPoints waypoints count (the
// team's unlocked size), and none of spell class cooldown (its last cast's;
// "" leaves none out, since every pattern has a class). With maxPoints at
// game.max_pattern_points and cooldown "", every pattern of the shape
// counts. The empty set fits nothing. Team state's wrong-scan check.
//
// It's one AND-compare per pattern of the shape (pattern & set == set), plus
// a popcount and a string comparison, stopping at the first match. Measured
// once, by hand, on the 2025 seed (156 patterns in its largest shape; the
// 2026 seed's largest, pentagon, has 252), before the size and class
// conditions were added: ~1 ns on a hit, ~9 ns on a miss. A precomputed map
// of every subset of every pattern was measured there too, at a flat ~15-19
// ns: hashing costs more than the scan at this size, so it isn't used.
// (BenchmarkLookups runs on the small test fixture, not a seed.)
func (a *Atlas) FitsShapeCastable(shape int, set WaypointSet, maxPoints int, cooldown string) bool {
	if set == 0 {
		return false
	}
	for _, p := range a.shapePatterns[shape] {
		pat := &a.patterns[p]
		if pat.Waypoints.Len() <= maxPoints && pat.Spell.Class != cooldown && pat.Waypoints.Contains(set) {
			return true
		}
	}
	return false
}

// WaypointIDs turns a set back into IDs, sorted.
func (a *Atlas) WaypointIDs(set WaypointSet) []string {
	ids := make([]string, 0, set.Len())
	for _, i := range set.Indexes() {
		ids = append(ids, a.waypoints[i].ID)
	}
	slices.Sort(ids)
	return ids
}

// SpellClasses returns every spell class, ascending by points. The slice is
// the atlas's own, never modified: callers must not change it.
func (a *Atlas) SpellClasses() []SpellClass { return a.classes }
