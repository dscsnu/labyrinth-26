package atlas

import "math/bits"

// Sets of waypoints, shapes and patterns are bitmasks over their indexes (a
// waypoint's, shape's or pattern's position in the Atlas). "Has the team
// scanned X?" is one AND, and "do these waypoints form a pattern?" is one map
// lookup keyed by the set itself. Indexes exist only in memory and may differ
// between restarts; Postgres and the API always use IDs.
//
// Example with the 2025 seed: waypoints 1001, 1002, 1007, 1008 are indexes
// 0, 1, 6, 7, so their WaypointSet is 0b11000011 = 195.

// Set capacities: Go type sizes, not tunables (AGENTS.md §10). Raising one
// means widening its set type; see README.md, "Limits and how to change them".
const (
	// MaxWaypoints is the most waypoints a WaypointSet holds.
	MaxWaypoints = 64
	// MaxShapes is the most shapes a ShapeSet holds.
	MaxShapes = 64
	// MaxPatterns is the most patterns a PatternSet holds.
	MaxPatterns = 2048
)

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
type PatternSet [MaxPatterns / 64]uint64

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
