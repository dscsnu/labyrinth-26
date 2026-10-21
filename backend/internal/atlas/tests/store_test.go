package tests

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"labyrinth/internal/atlas"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/postgres/pgtest"
)

// fixtureMaxPoints is the most waypoints in any fixture pattern (the
// triangles). With a smaller game.max_pattern_points in config.toml the
// fixture can't load, so the atlas tests skip rather than fail.
const fixtureMaxPoints = 3

// seedMaxPoints is the most waypoints in any 2026 seed pattern (its hexagons). With a
// smaller game.max_pattern_points the seed can't load, so tests on it skip.
const seedMaxPoints = 6

// maxPoints returns config.toml's game.max_pattern_points, skipping the test
// if the fixture's patterns wouldn't fit.
func maxPoints(tb testing.TB) int {
	tb.Helper()
	n := configtest.Settings(tb).Game.MaxPatternPoints
	if n < fixtureMaxPoints {
		tb.Skipf("atlas tests need game.max_pattern_points >= %d", fixtureMaxPoints)
	}
	return n
}

// fixtureClasses are spell classes class_1 to class_15, one per pattern size
// the game allows, deliberately out of order (New sorts them by points).
func fixtureClasses() []atlas.SpellClass {
	var cs []atlas.SpellClass
	for n := 15; n >= 1; n-- {
		cs = append(cs, atlas.SpellClass{ID: fmt.Sprintf("class_%d", n), Name: fmt.Sprintf("Class %d", n), Points: n})
	}
	return cs
}

// classOf returns the spell class ID of a pattern of these waypoints: the
// class of its waypoint count.
func classOf(waypoints []string) string { return fmt.Sprintf("class_%d", len(waypoints)) }

// fixture is four waypoints, 1001-1004 (A-D), forming triangle ABC, line AD
// and triangle BCD, with spell classes class_1 to class_15.
func fixture() atlas.Source {
	return atlas.Source{
		SpellClasses: fixtureClasses(),
		Waypoints:    []atlas.Waypoint{{ID: "1001"}, {ID: "1002"}, {ID: "1003"}, {ID: "1004"}},
		Shapes:       []atlas.Shape{{ID: "line"}, {ID: "triangle"}},
		Patterns: []atlas.PatternSource{
			{ID: 10, Shape: "triangle", Waypoints: []string{"1001", "1002", "1003"}, Damage: 50, Class: classOf([]string{"1001", "1002", "1003"})},
			{ID: 11, Shape: "line", Waypoints: []string{"1001", "1004"}, Damage: 30, Class: classOf([]string{"1001", "1004"})},
			{ID: 12, Shape: "triangle", Waypoints: []string{"1002", "1003", "1004"}, Damage: 50, Class: classOf([]string{"1002", "1003", "1004"})},
		},
	}
}

// TestPatternLookupIgnoresScanOrder checks that a pattern is found by its set
// of waypoints in any order, and that partial or extra sets don't match.
func TestPatternLookupIgnoresScanOrder(t *testing.T) {
	c, err := atlas.New(fixture(), atlas.Rules{MaxPatternPoints: maxPoints(t)})
	if err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]string{{"1001", "1002", "1003"}, {"1003", "1001", "1002"}, {"1002", "1003", "1001"}} {
		set, _ := c.SetOf(order)
		p, ok := c.PatternFor(set)
		if !ok || c.Pattern(p).ID != 10 || c.Shape(c.Pattern(p).Shape).ID != "triangle" {
			t.Fatalf("%v: got %d %v", order, p, ok)
		}
		if ids := c.WaypointIDs(c.Pattern(p).Waypoints); !slices.Equal(ids, []string{"1001", "1002", "1003"}) {
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

// TestPatternIndexes checks the lookup tables atlas.New builds: waypoint and
// shape indexes in ID order, each shape's patterns (what FitsShapeCastable and
// newGame walk), patterns by Postgres key, and WaypointIDs turning a set back
// into sorted IDs.
func TestPatternIndexes(t *testing.T) {
	c, err := atlas.New(fixture(), atlas.Rules{MaxPatternPoints: maxPoints(t)})
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
			out = append(out, c.Pattern(i).ID)
		}
		slices.Sort(out)
		return out
	}
	if got := ids(c.ShapePatterns(triangle)); !slices.Equal(got, []int{10, 12}) {
		t.Fatalf("triangle patterns = %v", got)
	}
	if p, ok := c.PatternByID(12); !ok || c.Pattern(p).ID != 12 {
		t.Fatalf("pattern 12 by key = %d %v", p, ok)
	}
	all, _ := c.SetOf([]string{"1004", "1002", "1001", "1003"})
	if got := c.WaypointIDs(all); !slices.Equal(got, []string{"1001", "1002", "1003", "1004"}) {
		t.Fatalf("WaypointIDs = %v", got)
	}
}

// TestAtlasValidation checks that every kind of bad content is rejected at
// load: bad and unknown IDs, empty, oversized, duplicate and nested patterns,
// and more waypoints, shapes or patterns than their bitsets hold.
func TestAtlasValidation(t *testing.T) {
	limit := maxPoints(t)
	cases := map[string]struct {
		mutate func(*atlas.Source)
		want   string // part of the error
	}{
		"waypoint id not 4 digits": {func(r *atlas.Source) { r.Waypoints[0].ID = "A1" }, "is not 4 digits"},
		"5-digit waypoint id":      {func(r *atlas.Source) { r.Waypoints[0].ID = "10001" }, "is not 4 digits"},
		"unknown waypoint":         {func(r *atlas.Source) { r.Patterns[0].Waypoints = []string{"1001", "9999"} }, "unknown waypoint"},
		"unknown shape":            {func(r *atlas.Source) { r.Patterns[0].Shape = "hexagon" }, "unknown shape"},
		"empty pattern":            {func(r *atlas.Source) { r.Patterns[0].Waypoints = nil }, "has no waypoints"},
		"zero damage":              {func(r *atlas.Source) { r.Patterns[0].Damage = 0 }, "damage must be positive"},
		"no spell class":           {func(r *atlas.Source) { r.Patterns[0].Class = "" }, "pattern 10 has no spell class"},
		"unknown spell class":      {func(r *atlas.Source) { r.Patterns[0].Class = "class_99" }, `unknown spell class "class_99"`},
		"class for another size":   {func(r *atlas.Source) { r.Patterns[0].Class = "class_4" }, `pattern 10 has 3 waypoints but spell class "class_4" is for 4`},
		"duplicate class id": {func(r *atlas.Source) {
			r.SpellClasses = append(r.SpellClasses, atlas.SpellClass{ID: "class_3", Points: 99})
		}, `spell class "class_3" appears twice`},
		"two classes for one size": {func(r *atlas.Source) {
			r.SpellClasses = append(r.SpellClasses, atlas.SpellClass{ID: "trio", Points: 3})
		}, "are both for 3 waypoints"},
		"class of zero points": {func(r *atlas.Source) {
			r.SpellClasses = append(r.SpellClasses, atlas.SpellClass{ID: "none", Points: 0})
		}, "points must be at least 1"},
		"negative damage": {func(r *atlas.Source) { r.Patterns[0].Damage = -10 }, "damage must be positive"},
		"same waypoints": {func(r *atlas.Source) {
			r.Patterns = append(r.Patterns, atlas.PatternSource{ID: 99, Shape: "line", Waypoints: []string{"1003", "1002", "1001"}, Damage: 50, Class: classOf([]string{"1003", "1002", "1001"})})
		}, "use the same waypoints"},
		"pattern bigger than max_pattern_points": {func(r *atlas.Source) {
			for i := range limit {
				r.Waypoints = append(r.Waypoints, atlas.Waypoint{ID: fmt.Sprintf("%04d", 3000+i)})
			}
			var ids []string
			for i := range limit + 1 {
				ids = append(ids, r.Waypoints[i].ID)
			}
			r.Patterns = append(r.Patterns, atlas.PatternSource{ID: 98, Shape: "line", Waypoints: ids, Damage: 50, Class: classOf(ids)})
		}, "game.max_pattern_points is"},
		"pattern inside a larger one of the same shape": {func(r *atlas.Source) {
			r.Patterns = append(r.Patterns, atlas.PatternSource{ID: 97, Shape: "triangle", Waypoints: []string{"1001", "1002"}, Damage: 50, Class: classOf([]string{"1001", "1002"})})
		}, "would complete first"},
		"too many waypoints": {func(r *atlas.Source) {
			for i := range atlas.MaxWaypoints {
				r.Waypoints = append(r.Waypoints, atlas.Waypoint{ID: fmt.Sprintf("%04d", 2000+i)})
			}
		}, "fit in a WaypointSet"},
		"too many shapes": {func(r *atlas.Source) {
			for i := range atlas.MaxShapes {
				r.Shapes = append(r.Shapes, atlas.Shape{ID: fmt.Sprintf("shape%d", i)})
			}
		}, "fit in a ShapeSet"},
		"too many patterns": {func(r *atlas.Source) {
			// The counts are checked before anything else, so the patterns'
			// contents don't matter.
			for i := range atlas.MaxPatterns {
				r.Patterns = append(r.Patterns, atlas.PatternSource{ID: 1000 + i, Shape: "line", Waypoints: []string{"1001"}, Damage: 50, Class: classOf([]string{"1001"})})
			}
		}, "fit in a PatternSet"},
	}
	for name, c := range cases {
		rows := fixture()
		c.mutate(&rows)
		_, err := atlas.New(rows, atlas.Rules{MaxPatternPoints: limit})
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want an error containing %q", name, err, c.want)
		}
	}
}

// TestSpellClasses checks that every pattern carries the spell class of its
// waypoint count, and that SpellClasses lists the classes ascending by points
// however they were given.
func TestSpellClasses(t *testing.T) {
	a, err := atlas.New(fixture(), atlas.Rules{MaxPatternPoints: maxPoints(t)})
	if err != nil {
		t.Fatal(err)
	}
	for i := range a.PatternCount() {
		p := a.Pattern(i)
		if want := fmt.Sprintf("class_%d", p.Waypoints.Len()); p.Spell.Class != want {
			t.Fatalf("pattern %d (%d waypoints): class %q, want %q", p.ID, p.Waypoints.Len(), p.Spell.Class, want)
		}
	}
	cs := a.SpellClasses()
	if len(cs) != 15 {
		t.Fatalf("%d classes, want 15", len(cs))
	}
	for i, c := range cs {
		if c.Points != i+1 || c.ID != fmt.Sprintf("class_%d", i+1) || c.Name != fmt.Sprintf("Class %d", i+1) {
			t.Fatalf("class %d: %+v, want class_%d ascending", i, c, i+1)
		}
	}
}

// TestBitsets checks the set operations, including atlas.PatternSet across 64-bit
// word boundaries.
func TestBitsets(t *testing.T) {
	var p atlas.PatternSet
	for _, i := range []int{0, 63, 64, 70, atlas.MaxPatterns - 1} {
		p = p.With(i)
	}
	if got := p.Indexes(); !slices.Equal(got, []int{0, 63, 64, 70, atlas.MaxPatterns - 1}) {
		t.Fatalf("PatternSet indexes = %v", got)
	}
	if p.Has(1) || !p.Has(70) {
		t.Fatal("PatternSet.Has wrong")
	}
	w := atlas.WaypointSet(0).With(3).With(63).With(3)
	if w.Len() != 2 || !w.Has(63) || w.Has(4) || !w.Contains(atlas.WaypointSet(0).With(3)) {
		t.Fatalf("WaypointSet ops wrong: %b", w)
	}
}

// TestMixedSizesAllowedWhenNotNested checks that a shape may have patterns
// of different sizes as long as none is inside another.
func TestMixedSizesAllowedWhenNotNested(t *testing.T) {
	rows := fixture()
	rows.Waypoints = append(rows.Waypoints, atlas.Waypoint{ID: "1005"})
	rows.Patterns = append(rows.Patterns, atlas.PatternSource{ID: 13, Shape: "line", Waypoints: []string{"1002", "1003", "1005"}, Damage: 50, Class: classOf([]string{"1002", "1003", "1005"})})
	if _, err := atlas.New(rows, atlas.Rules{MaxPatternPoints: maxPoints(t)}); err != nil {
		t.Fatal(err)
	}
}

// TestFitsShapeAll checks which sets can still grow into a pattern of a
// shape when every pattern counts (FitsShapeCastable with maxPoints at
// game.max_pattern_points and no cooldown): subsets of one of its patterns
// fit, anything spanning two patterns or touching none doesn't.
func TestFitsShapeAll(t *testing.T) {
	limit := maxPoints(t)
	c, err := atlas.New(fixture(), atlas.Rules{MaxPatternPoints: limit})
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
		if got := c.FitsShapeCastable(c2.shape, set, limit, ""); got != c2.want {
			t.Errorf("FitsShapeCastable(%s, %v, %d, \"\") = %v, want %v", c.Shape(c2.shape).ID, c2.ids, limit, got, c2.want)
		}
	}
}

// TestFitsShapeSizeLimit checks FitsShapeCastable's size limit, with no
// cooldown: only patterns of at most maxPoints waypoints count. With the line
// shape holding a 2-point and a 3-point pattern, a point only in the 3-point
// one fits within 3 but not within 2, a limit below every pattern fits
// nothing, and a limit at least the biggest pattern equals no limit
// (game.max_pattern_points).
func TestFitsShapeSizeLimit(t *testing.T) {
	rows := fixture()
	rows.Waypoints = append(rows.Waypoints, atlas.Waypoint{ID: "1005"})
	rows.Patterns = append(rows.Patterns, atlas.PatternSource{ID: 13, Shape: "line", Waypoints: []string{"1002", "1003", "1005"}, Damage: 50, Class: classOf([]string{"1002", "1003", "1005"})})
	c, err := atlas.New(rows, atlas.Rules{MaxPatternPoints: maxPoints(t)})
	if err != nil {
		t.Fatal(err)
	}
	line, _ := c.ShapeIndex("line")
	cases := []struct {
		ids  []string
		max  int
		want bool
	}{
		{[]string{"1001"}, 2, true},          // in the 2-point line
		{[]string{"1001"}, 1, false},         // no line has 1 point
		{[]string{"1002"}, 3, true},          // in the 3-point line
		{[]string{"1002"}, 2, false},         // only in the 3-point line
		{[]string{"1002", "1005"}, 3, true},  // the 3-point line grows
		{[]string{"1001", "1002"}, 3, false}, // spans both lines
		{nil, 3, false},                      // the empty set fits nothing
	}
	for _, c2 := range cases {
		set, _ := c.SetOf(c2.ids)
		if got := c.FitsShapeCastable(line, set, c2.max, ""); got != c2.want {
			t.Errorf("FitsShapeCastable(line, %v, %d, \"\") = %v, want %v", c2.ids, c2.max, got, c2.want)
		}
		if c2.max >= 3 && c.FitsShapeCastable(line, set, c2.max, "") != c.FitsShapeCastable(line, set, maxPoints(t), "") {
			t.Errorf("FitsShapeCastable(line, %v, %d, \"\") should equal no size limit when nothing is excluded", c2.ids, c2.max)
		}
	}
}

// TestFitsShapeCastable checks the cooldown-aware fit: a line shape with a
// 2-point (class_2) and a 3-point (class_3) pattern. Leaving a class out
// counts only the other pattern; the size limit still applies on top; no
// cooldown ("") excludes nothing, like an unknown class; and the empty set
// fits nothing.
func TestFitsShapeCastable(t *testing.T) {
	rows := fixture()
	rows.Waypoints = append(rows.Waypoints, atlas.Waypoint{ID: "1005"})
	rows.Patterns = append(rows.Patterns, atlas.PatternSource{ID: 13, Shape: "line", Waypoints: []string{"1002", "1003", "1005"}, Damage: 50, Class: "class_3"})
	c, err := atlas.New(rows, atlas.Rules{MaxPatternPoints: maxPoints(t)})
	if err != nil {
		t.Fatal(err)
	}
	line, _ := c.ShapeIndex("line")
	cases := []struct {
		ids      []string
		max      int
		cooldown string
		want     bool
	}{
		{[]string{"1001"}, 3, "", true},                 // in the 2-point line
		{[]string{"1001"}, 3, "class_2", false},         // only in the 2-point line, on cooldown
		{[]string{"1001"}, 3, "class_3", true},          // the other class is on cooldown
		{[]string{"1002"}, 3, "class_3", false},         // only in the 3-point line, on cooldown
		{[]string{"1002"}, 3, "class_2", true},          // the 3-point line is castable
		{[]string{"1002"}, 2, "class_2", false},         // castable class, but bigger than the limit
		{[]string{"1002", "1005"}, 3, "class_2", true},  // the 3-point line grows
		{[]string{"1001", "1004"}, 3, "class_2", false}, // exactly the cooled-down pattern: no fit, so it can't complete
		{[]string{"1001"}, 3, "class_9", true},          // an unknown class excludes nothing
		{nil, 3, "", false},                             // the empty set fits nothing
	}
	for _, c2 := range cases {
		set, _ := c.SetOf(c2.ids)
		if got := c.FitsShapeCastable(line, set, c2.max, c2.cooldown); got != c2.want {
			t.Errorf("FitsShapeCastable(line, %v, %d, %q) = %v, want %v", c2.ids, c2.max, c2.cooldown, got, c2.want)
		}
		if c2.cooldown == "" && c.FitsShapeCastable(line, set, c2.max, "") != c.FitsShapeCastable(line, set, c2.max, "class_9") {
			t.Errorf("FitsShapeCastable(line, %v, %d, \"\") should equal an unknown class: no cooldown excludes nothing", c2.ids, c2.max)
		}
	}
}

// TestSeedLoads checks, against a real Postgres (TEST_DATABASE_URL, schema
// atlas_test), that the 2026 seed loads and validates under config.toml's
// rules: its 10 checkpoints, its 5 shapes, and every combination of 2 to 6
// checkpoints as a pattern of the shape of its size (45 lines, 120
// triangles, 210 quadrilaterals, 252 pentagons, 210 hexagons), each with
// positive damage and the class of its size; and that the seed can be
// applied twice.
func TestSeedLoads(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Open(t, pgtest.URL(t, "atlas_test"))
	if err := db.Seed2026(ctx); err != nil {
		t.Fatal(err)
	}
	limit := configtest.Settings(t).Game.MaxPatternPoints
	if limit < seedMaxPoints {
		t.Skipf("the 2026 seed has %d-point patterns; game.max_pattern_points is %d", seedMaxPoints, limit)
	}
	a, err := atlas.Load(ctx, db, pgtest.Quiet, atlas.Rules{MaxPatternPoints: limit})
	if err != nil {
		t.Fatal(err)
	}
	if a.WaypointCount() != 10 || a.ShapeCount() != 5 || a.PatternCount() != 837 {
		t.Fatalf("loaded %d waypoints, %d shapes, %d patterns", a.WaypointCount(), a.ShapeCount(), a.PatternCount())
	}
	want := map[string]int{"line": 45, "triangle": 120, "quadrilateral": 210, "pentagon": 252, "hexagon": 210}
	for shape := range a.ShapeCount() {
		id := a.Shape(shape).ID
		ps := a.ShapePatterns(shape)
		if len(ps) != want[id] {
			t.Fatalf("shape %s: %d patterns, want %d", id, len(ps), want[id])
		}
		for _, p := range ps {
			pat := a.Pattern(p)
			size := pat.Waypoints.Len()
			if wantShape := []string{"", "", "line", "triangle", "quadrilateral", "pentagon", "hexagon"}[size]; wantShape != id {
				t.Fatalf("pattern %d has %d points but is a %s", pat.ID, size, id)
			}
			if pat.Spell.Damage <= 0 || pat.Spell.Class != fmt.Sprintf("class_%d", size) {
				t.Fatalf("pattern %d: damage %d, class %q", pat.ID, pat.Spell.Damage, pat.Spell.Class)
			}
		}
	}
	quad := []string{"2001", "2002", "2003", "2004"}
	set, _ := a.SetOf(quad)
	if p, ok := a.PatternFor(set); !ok || a.Shape(a.Pattern(p).Shape).ID != "quadrilateral" {
		t.Fatalf("%v should be a quadrilateral", quad)
	}
	if w := a.Waypoint(0); w.ID != "2001" || w.Name != "G Block Junc" || w.Latitude == 0 || w.Longitude == 0 {
		t.Fatalf("waypoint 0: %+v", w)
	}
	if err := db.Seed2026(ctx); err != nil {
		t.Fatalf("seeding again: %v", err)
	}
	if again := pgtest.Must(atlas.Load(ctx, db, pgtest.Quiet, atlas.Rules{MaxPatternPoints: limit})); again.PatternCount() != 837 {
		t.Fatalf("after seeding again: %d patterns", again.PatternCount())
	}
}

// Benchmark sinks: each benchmark stores its results here, so the compiler
// can't discard the lookups as unused.
var (
	// sinkBool holds boolean results.
	sinkBool bool
	// sinkInt holds index results.
	sinkInt int
)

// BenchmarkLookups measures the lookups a scan relies on, on the small
// fixture (four waypoints, three patterns). Numbers measured on the 2025 seed are in
// FitsShapeCastable's comment and the README.
func BenchmarkLookups(b *testing.B) {
	a, err := atlas.New(fixture(), atlas.Rules{MaxPatternPoints: maxPoints(b)})
	if err != nil {
		b.Fatal(err)
	}
	scanned, _ := a.SetOf([]string{"1001", "1002", "1003"})
	triangle, _ := a.ShapeIndex("triangle")

	limit := maxPoints(b)
	b.Run("FitsShapeCastable", func(b *testing.B) {
		for b.Loop() {
			sinkBool = a.FitsShapeCastable(triangle, scanned, limit, "")
		}
	})
	b.Run("PatternFor", func(b *testing.B) {
		for b.Loop() {
			sinkInt, sinkBool = a.PatternFor(scanned)
		}
	})
	b.Run("WaypointIndex+Has", func(b *testing.B) {
		for b.Loop() {
			i, _ := a.WaypointIndex("1002")
			sinkBool = scanned.Has(i)
		}
	})
}

// TestLoadRefusesMissingDamage checks, against a real Postgres, that a pattern
// with no damage (NULL, as after migration 0008 without the spells seed) makes
// Load fail with a message naming the pattern, rather than a scan error.
func TestLoadRefusesMissingDamage(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Open(t, pgtest.URL(t, "atlas_test"))
	if err := db.Seed2026(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, "UPDATE pattern SET damage = NULL WHERE id = 7"); err != nil {
		t.Fatal(err)
	}
	limit := configtest.Settings(t).Game.MaxPatternPoints
	if limit < seedMaxPoints {
		t.Skipf("the 2026 seed has %d-point patterns; game.max_pattern_points is %d", seedMaxPoints, limit)
	}
	_, err := atlas.Load(ctx, db, pgtest.Quiet, atlas.Rules{MaxPatternPoints: limit})
	if err == nil || !strings.Contains(err.Error(), "pattern 7: damage must be positive") {
		t.Fatalf("got %v, want an error naming pattern 7's missing damage", err)
	}
}

// TestLoadRefusesMissingSpellClass checks, against a real Postgres, that a
// pattern with no spell class (NULL, as after migration 0012 without the
// seed) makes Load fail with a message naming the pattern.
func TestLoadRefusesMissingSpellClass(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Open(t, pgtest.URL(t, "atlas_test"))
	if err := db.Seed2026(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Pool().Exec(ctx, "UPDATE pattern SET spell_class_id = NULL WHERE id = 7"); err != nil {
		t.Fatal(err)
	}
	limit := configtest.Settings(t).Game.MaxPatternPoints
	if limit < seedMaxPoints {
		t.Skipf("the 2026 seed has %d-point patterns; game.max_pattern_points is %d", seedMaxPoints, limit)
	}
	_, err := atlas.Load(ctx, db, pgtest.Quiet, atlas.Rules{MaxPatternPoints: limit})
	if err == nil || !strings.Contains(err.Error(), "pattern 7 has no spell class") {
		t.Fatalf("got %v, want an error naming pattern 7's missing spell class", err)
	}
}
