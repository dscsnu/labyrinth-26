package teamstate

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"labyrinth/internal/platform/config/configtest"
)

// fixtureMaxPoints is the most waypoints in any fixture pattern (the
// triangles). With a smaller game.max_pattern_points in config.toml the
// fixture can't load, so the content tests skip rather than fail.
const fixtureMaxPoints = 3

// maxPoints returns config.toml's game.max_pattern_points, skipping the test
// if the fixture's patterns wouldn't fit.
func maxPoints(tb testing.TB) int {
	tb.Helper()
	n := configtest.Settings(tb).Game.MaxPatternPoints
	if n < fixtureMaxPoints {
		tb.Skipf("content tests need game.max_pattern_points >= %d", fixtureMaxPoints)
	}
	return n
}

// fixture is four waypoints, 1001-1004 (A-D), forming triangle ABC, line AD
// and triangle BCD.
func fixture() contentRows {
	return contentRows{
		waypoints: []Waypoint{{ID: "1001"}, {ID: "1002"}, {ID: "1003"}, {ID: "1004"}},
		shapes:    []Shape{{ID: "line"}, {ID: "triangle"}},
		patterns: []patternRow{
			{id: 10, shapeID: "triangle", waypoints: []string{"1001", "1002", "1003"}},
			{id: 11, shapeID: "line", waypoints: []string{"1001", "1004"}},
			{id: 12, shapeID: "triangle", waypoints: []string{"1002", "1003", "1004"}},
		},
	}
}

// TestPatternLookupIgnoresScanOrder checks that a pattern is found by its set
// of waypoints in any order, and that partial or extra sets don't match.
func TestPatternLookupIgnoresScanOrder(t *testing.T) {
	c, err := newContent(fixture(), maxPoints(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]string{{"1001", "1002", "1003"}, {"1003", "1001", "1002"}, {"1002", "1003", "1001"}} {
		set, _ := c.SetOf(order)
		p, ok := c.PatternFor(set)
		if !ok || c.Patterns[p].dbID != 10 || c.Shapes[c.Patterns[p].Shape].ID != "triangle" {
			t.Fatalf("%v: got %d %v", order, p, ok)
		}
		if ids := c.WaypointIDs(c.Patterns[p].Waypoints); !slices.Equal(ids, []string{"1001", "1002", "1003"}) {
			t.Fatalf("pattern waypoints %v", ids)
		}
	}
	for _, notAPattern := range [][]string{{"1001", "1002"}, {"1001", "1002", "1003", "1004"}, {}} {
		set, _ := c.SetOf(notAPattern)
		if _, ok := c.PatternFor(set); ok {
			t.Fatalf("%v matched a pattern", notAPattern)
		}
	}
	if _, err := c.SetOf([]string{"1001", "9999"}); err == nil {
		t.Fatal("unknown waypoint accepted")
	}
}

// TestPatternIndexes checks the lookup tables newContent builds: waypoint and
// shape indexes in ID order, each shape's patterns (what FitsShape and
// newGame walk), patterns by Postgres key, and WaypointIDs turning a set back
// into sorted IDs.
func TestPatternIndexes(t *testing.T) {
	c, err := newContent(fixture(), maxPoints(t))
	if err != nil {
		t.Fatal(err)
	}
	if d, ok := c.WaypointIndex("1004"); !ok || d != 3 {
		t.Fatalf("waypoint 1004 = %d %v, want index 3", d, ok)
	}
	if _, ok := c.WaypointIndex("1005"); ok {
		t.Fatal("unknown waypoint has an index")
	}
	triangle, ok := c.ShapeIndex("triangle")
	if !ok || triangle != 1 {
		t.Fatalf("triangle = %d %v, want index 1", triangle, ok)
	}
	ids := func(idx []int) []int {
		var out []int
		for _, i := range idx {
			out = append(out, c.Patterns[i].dbID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(c.shapePatterns[triangle]); !slices.Equal(got, []int{10, 12}) {
		t.Fatalf("triangle patterns = %v", got)
	}
	if p, ok := c.patternByDBID[12]; !ok || c.Patterns[p].dbID != 12 {
		t.Fatalf("pattern 12 by key = %d %v", p, ok)
	}
	all, _ := c.SetOf([]string{"1004", "1002", "1001", "1003"})
	if got := c.WaypointIDs(all); !slices.Equal(got, []string{"1001", "1002", "1003", "1004"}) {
		t.Fatalf("WaypointIDs = %v", got)
	}
}

// TestContentValidation checks that every kind of bad content is rejected at
// load: bad and unknown IDs, empty, oversized, duplicate and nested patterns,
// and more waypoints, shapes or patterns than their bitsets hold.
func TestContentValidation(t *testing.T) {
	limit := maxPoints(t)
	cases := map[string]struct {
		mutate func(*contentRows)
		want   string // part of the error
	}{
		"waypoint id not 4 digits": {func(r *contentRows) { r.waypoints[0].ID = "A1" }, "is not 4 digits"},
		"5-digit waypoint id":      {func(r *contentRows) { r.waypoints[0].ID = "10001" }, "is not 4 digits"},
		"unknown waypoint":         {func(r *contentRows) { r.patterns[0].waypoints = []string{"1001", "9999"} }, "unknown waypoint"},
		"unknown shape":            {func(r *contentRows) { r.patterns[0].shapeID = "hexagon" }, "unknown shape"},
		"empty pattern":            {func(r *contentRows) { r.patterns[0].waypoints = nil }, "has no waypoints"},
		"same waypoints": {func(r *contentRows) {
			r.patterns = append(r.patterns, patternRow{id: 99, shapeID: "line", waypoints: []string{"1003", "1002", "1001"}})
		}, "use the same waypoints"},
		"pattern bigger than max_pattern_points": {func(r *contentRows) {
			for i := range limit {
				r.waypoints = append(r.waypoints, Waypoint{ID: fmt.Sprintf("%04d", 3000+i)})
			}
			var ids []string
			for i := range limit + 1 {
				ids = append(ids, r.waypoints[i].ID)
			}
			r.patterns = append(r.patterns, patternRow{id: 98, shapeID: "line", waypoints: ids})
		}, "game.max_pattern_points is"},
		"pattern inside a larger one of the same shape": {func(r *contentRows) {
			r.patterns = append(r.patterns, patternRow{id: 97, shapeID: "triangle", waypoints: []string{"1001", "1002"}})
		}, "would complete first"},
		"too many waypoints": {func(r *contentRows) {
			for i := range maxWaypoints {
				r.waypoints = append(r.waypoints, Waypoint{ID: fmt.Sprintf("%04d", 2000+i)})
			}
		}, "fit in a WaypointSet"},
		"too many shapes": {func(r *contentRows) {
			for i := range maxShapes {
				r.shapes = append(r.shapes, Shape{ID: fmt.Sprintf("shape%d", i)})
			}
		}, "fit in a ShapeSet"},
		"too many patterns": {func(r *contentRows) {
			// The counts are checked before anything else, so the patterns'
			// contents don't matter.
			for i := range maxPatterns {
				r.patterns = append(r.patterns, patternRow{id: 1000 + i, shapeID: "line", waypoints: []string{"1001"}})
			}
		}, "fit in a PatternSet"},
	}
	for name, c := range cases {
		rows := fixture()
		c.mutate(&rows)
		_, err := newContent(rows, limit)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
}

// TestBitsets checks the set operations, including PatternSet across 64-bit
// word boundaries.
func TestBitsets(t *testing.T) {
	var p PatternSet
	for _, i := range []int{0, 63, 64, 70, maxPatterns - 1} {
		p = p.With(i)
	}
	if got := p.Indexes(); !slices.Equal(got, []int{0, 63, 64, 70, maxPatterns - 1}) {
		t.Fatalf("PatternSet indexes = %v", got)
	}
	if p.Has(1) || !p.Has(70) {
		t.Fatal("PatternSet.Has wrong")
	}
	w := WaypointSet(0).With(3).With(63).With(3)
	if w.Len() != 2 || !w.Has(63) || w.Has(4) || !w.Contains(WaypointSet(0).With(3)) {
		t.Fatalf("WaypointSet ops wrong: %b", w)
	}
}

// TestMixedSizesAllowedWhenNotNested checks that a shape may have patterns
// of different sizes as long as none is inside another.
func TestMixedSizesAllowedWhenNotNested(t *testing.T) {
	rows := fixture()
	rows.waypoints = append(rows.waypoints, Waypoint{ID: "1005"})
	rows.patterns = append(rows.patterns, patternRow{id: 13, shapeID: "line", waypoints: []string{"1002", "1003", "1005"}})
	if _, err := newContent(rows, maxPoints(t)); err != nil {
		t.Fatal(err)
	}
}

// TestStack checks the stack: bottom-to-top order, dropping the bottom when
// full, Push leaving the original unchanged, Live leaving out stale scans, and
// a wrong scan never going stale.
func TestStack(t *testing.T) {
	base := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	waypoints := func(scans []StackScan) []int {
		var out []int
		for _, sc := range scans {
			out = append(out, sc.Waypoint)
		}
		return out
	}
	all := func(s Stack) []StackScan {
		var out []StackScan
		for i := range s.Len() {
			out = append(out, s.At(i))
		}
		return out
	}

	s := newStack(3)
	for w := range 3 {
		s = s.Push(StackScan{Waypoint: w, At: base.Add(time.Duration(w) * 10 * time.Second)})
	}
	if got := waypoints(all(s)); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("full stack %v", got)
	}
	before := s
	s = s.Push(StackScan{Waypoint: 3, At: base.Add(30 * time.Second)})
	if got := waypoints(all(s)); !slices.Equal(got, []int{1, 2, 3}) || s.Len() != 3 {
		t.Fatalf("after overflow %v", got)
	}
	if got := waypoints(all(before)); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("Push changed the original: %v", got)
	}

	// At 45 s with a 25 s limit: scans at 10 s (35 s old) are stale, 20 s (25 s
	// old, exactly the limit) and 30 s are live.
	if got := waypoints(s.Live(base.Add(45*time.Second), 25*time.Second)); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("live %v", got)
	}
	if got := s.Live(base.Add(time.Hour), time.Second); len(got) != 0 {
		t.Fatalf("everything should be stale: %v", got)
	}

	// A wrong scan stays live however old it is.
	wrong := newStack(3).Push(StackScan{Waypoint: 7, At: base, Wrong: true})
	if got := waypoints(wrong.Live(base.Add(time.Hour), time.Second)); !slices.Equal(got, []int{7}) {
		t.Fatalf("wrong scan should stay live: %v", got)
	}
}

// TestFitsShape checks which sets can still grow into a pattern of a shape:
// subsets of one of its patterns fit, anything spanning two patterns or
// touching none doesn't.
func TestFitsShape(t *testing.T) {
	c, err := newContent(fixture(), maxPoints(t))
	if err != nil {
		t.Fatal(err)
	}
	triangle, _ := c.ShapeIndex("triangle")
	line, _ := c.ShapeIndex("line")
	cases := []struct {
		shape int
		ids   []string
		want  bool
	}{
		{triangle, []string{"1001"}, true},                 // in ABC
		{triangle, []string{"1004"}, true},                 // in BCD
		{triangle, []string{"1002", "1003"}, true},         // in both
		{triangle, []string{"1001", "1004"}, false},        // A and D share no triangle
		{triangle, []string{"1001", "1002", "1003"}, true}, // the whole of ABC
		{line, []string{"1002"}, false},                    // B is on no line
		{line, []string{"1001", "1004"}, true},
		{triangle, nil, false}, // the empty set fits nothing
	}
	for _, c2 := range cases {
		set, _ := c.SetOf(c2.ids)
		if got := c.FitsShape(c2.shape, set); got != c2.want {
			t.Errorf("FitsShape(%s, %v) = %v, want %v", c.Shapes[c2.shape].ID, c2.ids, got, c2.want)
		}
	}
}
