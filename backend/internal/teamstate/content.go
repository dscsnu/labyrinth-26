package teamstate

import (
	"fmt"
	"math/bits"
	"regexp"
	"slices"
)

// Static game content in memory: waypoints, shapes and the patterns that form
// them. Built once at startup and never modified, so every lookup is
// lock-free.
//
// Each waypoint, shape and pattern gets a small index (its position in the
// slices below; waypoints and shapes in ID order, patterns in Postgres key
// order), and sets of them are bitmasks: "has the team scanned X?" is one AND,
// and "do these waypoints form a pattern?" is one map lookup keyed by the set
// itself. Indexes exist only in memory; Postgres stores IDs, so indexes may
// differ between restarts.
//
// Example with the 2025 seed: waypoints 1001, 1002, 1007, 1008 are indexes
// 0, 1, 6, 7, so their WaypointSet is 0b11000011 = 195, and patternBySet[195]
// is that square. See README.md, "How waypoints and patterns are stored".

const (
	// maxWaypoints is the most waypoints a WaypointSet holds.
	maxWaypoints = 64
	// maxShapes is the most shapes a ShapeSet holds.
	maxShapes = 64
	// maxPatterns is the most patterns a PatternSet holds.
	maxPatterns = 512
)

// waypointIDPattern matches a waypoint ID: exactly 4 digits.
var waypointIDPattern = regexp.MustCompile(`^[0-9]{4}$`)

// WaypointSet is a set of waypoint indexes: bit i means waypoint i. Unordered
// by nature, so scanning A,B,C and C,A,B give the same set. Comparable, so it
// can key a map.
type WaypointSet uint64

// Has reports whether waypoint i is in the set.
func (s WaypointSet) Has(i int) bool { return s&(1<<i) != 0 }

// With returns the set plus waypoint i.
func (s WaypointSet) With(i int) WaypointSet { return s | 1<<i }

// Contains reports whether every waypoint in o is in the set.
func (s WaypointSet) Contains(o WaypointSet) bool { return s&o == o }

// Len returns how many waypoints are in the set.
func (s WaypointSet) Len() int { return bits.OnesCount64(uint64(s)) }

// Indexes returns the set's waypoint indexes in ascending order.
func (s WaypointSet) Indexes() []int { return setBits(uint64(s), nil, 0) }

// ShapeSet is a set of shape indexes.
type ShapeSet uint64

// Has reports whether shape i is in the set.
func (s ShapeSet) Has(i int) bool { return s&(1<<i) != 0 }

// With returns the set plus shape i.
func (s ShapeSet) With(i int) ShapeSet { return s | 1<<i }

// Indexes returns the set's shape indexes in ascending order.
func (s ShapeSet) Indexes() []int { return setBits(uint64(s), nil, 0) }

// PatternSet is a set of pattern indexes. A fixed-size array, so copying a
// state snapshot copies it without allocating.
type PatternSet [maxPatterns / 64]uint64

// Has reports whether pattern i is in the set.
func (s PatternSet) Has(i int) bool { return s[i/64]&(1<<(i%64)) != 0 }

// With returns the set plus pattern i. s is an array, so the receiver is a
// copy and the original is unchanged.
func (s PatternSet) With(i int) PatternSet {
	s[i/64] |= 1 << (i % 64)
	return s
}

// Indexes returns the set's pattern indexes in ascending order.
func (s PatternSet) Indexes() []int {
	var out []int
	for w, word := range s {
		out = setBits(word, out, w*64)
	}
	return out
}

// setBits appends offset plus the position of each set bit in word to out.
func setBits(word uint64, out []int, offset int) []int {
	for word != 0 {
		i := bits.TrailingZeros64(word)
		out = append(out, offset+i)
		word &^= 1 << i
	}
	return out
}

// Waypoint is a physical location with a QR code.
type Waypoint struct {
	ID        string  `json:"id"` // 4 digits
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`
}

// Shape is what a pattern looks like on the map, e.g. "square".
type Shape struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Pattern is a set of waypoints that forms a shape. It's identified by that
// set; dbID is only Postgres's key for it.
type Pattern struct {
	dbID      int
	Shape     int // index into Content.Shapes
	Waypoints WaypointSet
}

// Content is the static game content and its lookup tables. Never modified
// after newContent returns.
type Content struct {
	Waypoints []Waypoint
	Shapes    []Shape
	Patterns  []Pattern

	waypointIndex map[string]int      // waypoint ID → index
	shapeIndex    map[string]int      // shape ID → index
	patternByDBID map[int]int         // Postgres pattern ID → index
	patternBySet  map[WaypointSet]int // exact waypoint set → pattern index
	shapePatterns [][]int             // shape index → its patterns (FitsShape, newGame)
}

// newContent validates the rows from db.go and builds the lookup tables.
// maxPoints is config.toml game.max_pattern_points: no pattern may have more
// waypoints.
func newContent(rows contentRows, maxPoints int) (*Content, error) {
	if len(rows.waypoints) > maxWaypoints {
		return nil, fmt.Errorf("%d waypoints; at most %d fit in a WaypointSet", len(rows.waypoints), maxWaypoints)
	}
	if len(rows.shapes) > maxShapes {
		return nil, fmt.Errorf("%d shapes; at most %d fit in a ShapeSet", len(rows.shapes), maxShapes)
	}
	if len(rows.patterns) > maxPatterns {
		return nil, fmt.Errorf("%d patterns; at most %d fit in a PatternSet", len(rows.patterns), maxPatterns)
	}

	c := &Content{
		waypointIndex: make(map[string]int, len(rows.waypoints)),
		shapeIndex:    make(map[string]int, len(rows.shapes)),
		patternByDBID: make(map[int]int, len(rows.patterns)),
		patternBySet:  make(map[WaypointSet]int, len(rows.patterns)),
		shapePatterns: make([][]int, len(rows.shapes)),
	}
	for i, w := range rows.waypoints {
		if !waypointIDPattern.MatchString(w.ID) {
			return nil, fmt.Errorf("waypoint id %q is not 4 digits", w.ID)
		}
		c.Waypoints = append(c.Waypoints, w)
		c.waypointIndex[w.ID] = i
	}
	for i, s := range rows.shapes {
		c.Shapes = append(c.Shapes, s)
		c.shapeIndex[s.ID] = i
	}
	for i, p := range rows.patterns {
		shape, ok := c.shapeIndex[p.shapeID]
		if !ok {
			return nil, fmt.Errorf("pattern %d: unknown shape %q", p.id, p.shapeID)
		}
		set, err := c.SetOf(p.waypoints)
		if err != nil {
			return nil, fmt.Errorf("pattern %d: %w", p.id, err)
		}
		if set == 0 {
			return nil, fmt.Errorf("pattern %d has no waypoints", p.id)
		}
		if set.Len() > maxPoints {
			return nil, fmt.Errorf("pattern %d has %d waypoints; game.max_pattern_points is %d", p.id, set.Len(), maxPoints)
		}
		if other, dup := c.patternBySet[set]; dup {
			return nil, fmt.Errorf("patterns %d and %d use the same waypoints", c.Patterns[other].dbID, p.id)
		}

		c.Patterns = append(c.Patterns, Pattern{dbID: p.id, Shape: shape, Waypoints: set})
		c.patternByDBID[p.id] = i
		c.patternBySet[set] = i
		c.shapePatterns[shape] = append(c.shapePatterns[shape], i)
	}
	for shape, ps := range c.shapePatterns {
		if err := c.checkNesting(shape, ps); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// checkNesting rejects a pattern contained in a larger pattern of the same
// shape: scanning the larger one, the stack would pass through the smaller
// one first and complete it early.
func (c *Content) checkNesting(shape int, patterns []int) error {
	for _, small := range patterns {
		for _, big := range patterns {
			a, b := c.Patterns[small].Waypoints, c.Patterns[big].Waypoints
			if a.Len() < b.Len() && b.Contains(a) {
				return fmt.Errorf("shape %q: pattern %d is inside pattern %d, so it would complete first",
					c.Shapes[shape].ID, c.Patterns[small].dbID, c.Patterns[big].dbID)
			}
		}
	}
	return nil
}

// WaypointIndex looks up a waypoint by its 4-digit ID.
func (c *Content) WaypointIndex(id string) (int, bool) {
	i, ok := c.waypointIndex[id]
	return i, ok
}

// ShapeIndex looks up a shape by its ID.
func (c *Content) ShapeIndex(id string) (int, bool) {
	i, ok := c.shapeIndex[id]
	return i, ok
}

// SetOf turns waypoint IDs into a WaypointSet.
func (c *Content) SetOf(ids []string) (WaypointSet, error) {
	var set WaypointSet
	for _, id := range ids {
		i, ok := c.waypointIndex[id]
		if !ok {
			return 0, fmt.Errorf("unknown waypoint %q", id)
		}
		set = set.With(i)
	}
	return set, nil
}

// PatternFor returns the index of the pattern made of exactly these
// waypoints.
func (c *Content) PatternFor(set WaypointSet) (int, bool) {
	i, ok := c.patternBySet[set]
	return i, ok
}

// FitsShape reports whether some pattern of shape contains every waypoint in
// set: whether set can still grow into a pattern of that shape. The empty set
// fits no shape.
//
// It's one AND-compare per pattern of the shape (pattern & set == set),
// stopping at the first match. Measured once on the 2025 seed (156 patterns in
// the largest shape): ~1 ns on a hit, ~9 ns on a miss. A precomputed map of
// every subset of every pattern was measured once on the seed too, at a flat
// ~15-19 ns: hashing costs more than the scan at this size, so it isn't used.
// (BenchmarkScanLookups runs on the small test fixture, not the seed.)
func (c *Content) FitsShape(shape int, set WaypointSet) bool {
	if set == 0 {
		return false
	}
	for _, p := range c.shapePatterns[shape] {
		if c.Patterns[p].Waypoints.Contains(set) {
			return true
		}
	}
	return false
}

// WaypointIDs turns a set back into IDs, sorted.
func (c *Content) WaypointIDs(set WaypointSet) []string {
	ids := make([]string, 0, set.Len())
	for _, i := range set.Indexes() {
		ids = append(ids, c.Waypoints[i].ID)
	}
	slices.Sort(ids)
	return ids
}
