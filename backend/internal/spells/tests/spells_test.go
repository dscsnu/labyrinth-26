package tests

import (
	"context"
	"encoding/json"
	"maps"
	"math"
	"os"
	"strconv"
	"testing"

	"labyrinth/internal/platform/config"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/spells"
)

// damageRules builds the damage formula's rules from config.toml, as
// cmd/spells does.
func damageRules(set config.Settings) spells.DamageRules {
	return spells.DamageRules{
		Base:             set.Spell.BaseDamage(),
		ConcaveQuadBonus: set.Spell.Bonus.ConcaveQuad,
		Per100m:          set.Spell.Per100m,
		Walkability:      set.Spell.Walkability,
		MaxPoints:        set.Game.MaxPatternPoints,
	}
}

// mustDamage returns r.Damage(pts), failing the test on an error.
func mustDamage(t *testing.T, r spells.DamageRules, pts []spells.Point) int {
	t.Helper()
	d, err := r.Damage(pts)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// TestPureMathGeometry checks geometry algorithms and damage formulas on
// synthetic coordinates without needing a database connection.
func TestPureMathGeometry(t *testing.T) {
	set := configtest.Settings(t)
	rules := damageRules(set)
	baseMap := rules.Base
	walk := rules.Walkability

	t.Run("1-point pattern damage", func(t *testing.T) {
		p1 := []spells.Point{{X: 100, Y: 100}}
		tour := spells.TourM(p1)
		if tour != 0 {
			t.Fatalf("expected 0 tour for 1 point, got %f", tour)
		}
		dmg := mustDamage(t, rules, p1)
		expected := baseMap[1]
		if dmg != expected {
			t.Fatalf("expected 1-point damage %d, got %d", expected, dmg)
		}
	})

	t.Run("2-point pattern out and back", func(t *testing.T) {
		p2 := []spells.Point{{X: 0, Y: 0}, {X: 300, Y: 400}}
		tour := spells.TourM(p2)
		const expectedTour = 1000.0 // 2 * 500
		if math.Abs(tour-expectedTour) > 1e-6 {
			t.Fatalf("expected tour %f, got %f", expectedTour, tour)
		}
		dmg := mustDamage(t, rules, p2)
		if dmg <= 0 {
			t.Fatalf("expected positive damage, got %d", dmg)
		}
	})

	t.Run("hand-computed 100m square tour within 1m", func(t *testing.T) {
		square := []spells.Point{
			{X: 0, Y: 0},
			{X: 100, Y: 0},
			{X: 100, Y: 100},
			{X: 0, Y: 100},
		}
		tour := spells.TourM(square)
		const expectedSquareTour = 400.0
		if math.Abs(tour-expectedSquareTour) > 1.0 {
			t.Fatalf("expected square tour ~400m, got %f", tour)
		}
	})

	t.Run("hull is subset and excludes interior point", func(t *testing.T) {
		pts := []spells.Point{
			{X: 0, Y: 0},
			{X: 200, Y: 0},
			{X: 200, Y: 200},
			{X: 0, Y: 200},
			{X: 100, Y: 100}, // interior
		}
		hull := spells.ConvexHull(pts)
		if len(hull) != 4 {
			t.Fatalf("expected 4 hull vertices, got %d", len(hull))
		}
		for _, h := range hull {
			if h.X == 100 && h.Y == 100 {
				t.Fatalf("interior point (100, 100) included in convex hull")
			}
		}
	})

	t.Run("concave quad bonus awarded", func(t *testing.T) {
		convexQuad := []spells.Point{
			{X: 0, Y: 0},
			{X: 200, Y: 0},
			{X: 200, Y: 200},
			{X: 0, Y: 200},
		}
		concaveQuad := []spells.Point{
			{X: 0, Y: 0},
			{X: 200, Y: 0},
			{X: 200, Y: 200},
			{X: 50, Y: 50}, // interior point gives hull size 3
		}
		dmgConvex := mustDamage(t, rules, convexQuad)
		dmgConcave := mustDamage(t, rules, concaveQuad)
		if dmgConcave <= dmgConvex {
			t.Fatalf("expected concave quad damage (%d) > convex quad (%d)", dmgConcave, dmgConvex)
		}
		noBonus := rules
		noBonus.ConcaveQuadBonus = 0
		if got := dmgConcave - mustDamage(t, noBonus, concaveQuad); got != rules.ConcaveQuadBonus {
			t.Fatalf("the concave quad's bonus is %d, want spell.bonus.concave_quad = %d", got, rules.ConcaveQuadBonus)
		}
	})

	t.Run("distance scaling changes damage by formula", func(t *testing.T) {
		pts := []spells.Point{
			{X: 0, Y: 0},
			{X: 150, Y: 50},
			{X: 80, Y: 200},
		}
		tour := spells.TourM(pts)
		doubled := rules
		doubled.Per100m *= 2
		diff := mustDamage(t, doubled, pts) - mustDamage(t, rules, pts)
		expectedDiff := int(math.Round(rules.Per100m * (tour * walk / 100.0)))
		if math.Abs(float64(diff-expectedDiff)) > 1.0 {
			t.Fatalf("expected damage difference %d, got %d", expectedDiff, diff)
		}
	})

	t.Run("6-point pattern", func(t *testing.T) {
		hex := []spells.Point{
			{X: 0, Y: 100},
			{X: 86.6, Y: 50},
			{X: 86.6, Y: -50},
			{X: 0, Y: -100},
			{X: -86.6, Y: -50},
			{X: -86.6, Y: 50},
		}
		tour := spells.TourM(hex)
		if tour <= 0 {
			t.Fatalf("expected positive tour for hexagon, got %f", tour)
		}
		if len(hex) != rules.MaxPoints {
			t.Fatalf("test assumes game.max_pattern_points = %d (got %d)", len(hex), rules.MaxPoints)
		}
		// The biggest size deals its base flat: no distance, however long the tour.
		if dmg := mustDamage(t, rules, hex); dmg != baseMap[rules.MaxPoints] {
			t.Fatalf("hexagon damage %d, want spell.base.%d = %d", dmg, rules.MaxPoints, baseMap[rules.MaxPoints])
		}
		hull := spells.ConvexHull(hex)
		if len(hull) != 6 {
			t.Fatalf("expected 6 hull vertices for regular hexagon, got %d", len(hull))
		}
	})

	t.Run("sizes outside the rules are errors", func(t *testing.T) {
		tooMany := make([]spells.Point, rules.MaxPoints+1)
		for i := range tooMany {
			tooMany[i] = spells.Point{X: float64(i * 100), Y: float64(i * i * 10)}
		}
		if _, err := rules.Damage(nil); err == nil {
			t.Fatal("an empty pattern should be an error")
		}
		if _, err := rules.Damage(tooMany); err == nil {
			t.Fatalf("a %d-point pattern should be an error", len(tooMany))
		}
		missing := rules
		missing.Base = map[int]int{1: baseMap[1]}
		if _, err := missing.Damage([]spells.Point{{X: 0, Y: 0}, {X: 100, Y: 0}}); err == nil {
			t.Fatal("a size with no base damage should be an error, not 0")
		}
	})
}

// TestSeedPatternGeometryProperties checks geometric invariants and damage properties
// against the real database seed (SeedDev2025).
func TestSeedPatternGeometryProperties(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Open(t, pgtest.URL(t, "spells_geom_test"))
	if err := db.SeedDev2025(ctx); err != nil {
		t.Fatal(err)
	}

	set := configtest.Settings(t)

	rows, err := db.Pool().Query(ctx, "SELECT id, latitude, longitude FROM waypoint ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var wps []spells.Waypoint
	for rows.Next() {
		var w spells.Waypoint
		if err := rows.Scan(&w.ID, &w.GPS.Latitude, &w.GPS.Longitude); err != nil {
			t.Fatal(err)
		}
		wps = append(wps, w)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	projected := spells.ProjectWaypoints(wps)

	pRows, err := db.Pool().Query(ctx, `
		SELECT p.id, p.damage, COALESCE(array_agg(pw.waypoint_id ORDER BY pw.waypoint_id)
		                                FILTER (WHERE pw.waypoint_id IS NOT NULL), '{}')
		FROM pattern p
		LEFT JOIN pattern_waypoint pw ON pw.pattern_id = p.id
		GROUP BY p.id, p.damage
		ORDER BY p.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer pRows.Close()

	var patternCount int
	for pRows.Next() {
		var pid, dmg int
		var wids []string
		if err := pRows.Scan(&pid, &dmg, &wids); err != nil {
			t.Fatal(err)
		}
		patternCount++

		if dmg <= 0 {
			t.Errorf("pattern %d: damage %d must be positive", pid, dmg)
		}

		pts := make([]spells.Point, len(wids))
		for i, wid := range wids {
			pt, ok := projected[wid]
			if !ok {
				t.Fatalf("pattern %d references unknown waypoint %s", pid, wid)
			}
			pts[i] = pt
		}

		hull := spells.ConvexHull(pts)
		if len(hull) > len(pts) {
			t.Errorf("pattern %d: hull size %d exceeds waypoint count %d", pid, len(hull), len(pts))
		}

		// Verify square 87 ("1001", "1002", "1007", "1008") tour is within 1 m of 1558 m
		if pid == 87 {
			tour := spells.TourM(pts)
			const expectedTour = 1557.96
			if math.Abs(tour-expectedTour) > 1.0 {
				t.Errorf("pattern 87 tour %f, expected ~%f", tour, expectedTour)
			}
		}

		// Verify formula property on this pattern
		recomputed := mustDamage(t, damageRules(set), pts)
		if recomputed != dmg {
			t.Errorf("pattern %d: recomputed damage %d != stored damage %d", pid, recomputed, dmg)
		}
	}

	if patternCount != 359 {
		t.Fatalf("expected 359 patterns in dev seed, got %d", patternCount)
	}
}

// TestPatternDamageDrift verifies that recomputing every pattern's damage with
// internal/spells matches pattern.damage in the database. Any drift in coordinates
// or coefficients without re-running cmd/spells fails this test.
func TestPatternDamageDrift(t *testing.T) {
	ctx := context.Background()
	db := pgtest.Open(t, pgtest.URL(t, "spells_drift_test"))
	if err := db.SeedDev2025(ctx); err != nil {
		t.Fatal(err)
	}

	set := configtest.Settings(t)

	rows, err := db.Pool().Query(ctx, "SELECT id, latitude, longitude FROM waypoint ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()

	var wps []spells.Waypoint
	for rows.Next() {
		var w spells.Waypoint
		if err := rows.Scan(&w.ID, &w.GPS.Latitude, &w.GPS.Longitude); err != nil {
			t.Fatal(err)
		}
		wps = append(wps, w)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	projected := spells.ProjectWaypoints(wps)

	pRows, err := db.Pool().Query(ctx, `
		SELECT p.id, p.damage, COALESCE(array_agg(pw.waypoint_id ORDER BY pw.waypoint_id)
		                                FILTER (WHERE pw.waypoint_id IS NOT NULL), '{}')
		FROM pattern p
		LEFT JOIN pattern_waypoint pw ON pw.pattern_id = p.id
		GROUP BY p.id, p.damage
		ORDER BY p.id`)
	if err != nil {
		t.Fatal(err)
	}
	defer pRows.Close()

	type patternDrift struct {
		id       int
		stored   int
		computed int
	}
	var drifted []patternDrift

	for pRows.Next() {
		var pid, storedDamage int
		var wids []string
		if err := pRows.Scan(&pid, &storedDamage, &wids); err != nil {
			t.Fatal(err)
		}

		pts := make([]spells.Point, len(wids))
		for i, wid := range wids {
			pts[i] = projected[wid]
		}

		computed := mustDamage(t, damageRules(set), pts)
		if computed != storedDamage {
			drifted = append(drifted, patternDrift{id: pid, stored: storedDamage, computed: computed})
		}
	}

	if len(drifted) > 0 {
		for _, d := range drifted {
			t.Errorf("drift detected for pattern %d: stored=%d, computed=%d", d.id, d.stored, d.computed)
		}
		t.Fatalf("%d patterns drifted. Re-run `go run ./cmd/spells` to regenerate dev_2025_spells.sql", len(drifted))
	}
}

// damageLookupPath is damage_lookup.json, the 2026 damage table at the
// repository root, relative to this package's directory (where go test runs).
const damageLookupPath = "../../../../damage_lookup.json"

// damageLookup is the part of damage_lookup.json this test reads.
type damageLookup struct {
	// Metadata holds the checkpoints and the coefficients the table was
	// computed with.
	Metadata struct {
		// Checkpoints are the 2026 checkpoints by name.
		Checkpoints map[string]struct {
			// Latitude is degrees north.
			Latitude float64 `json:"latitude"`
			// Longitude is degrees east.
			Longitude float64 `json:"longitude"`
		} `json:"checkpoints"`
		// DamageCoefficients mirror config.toml's [spell] table.
		DamageCoefficients struct {
			// SpellBase is spell.base, by point count.
			SpellBase map[string]int `json:"spell_base"`
			// BonusConcaveQuad is spell.bonus.concave_quad.
			BonusConcaveQuad int `json:"bonus_concave_quad"`
			// Per100m is spell.per_100m.
			Per100m float64 `json:"per_100m"`
			// Walkability is spell.walkability.
			Walkability float64 `json:"walkability"`
		} `json:"damage_coefficients"`
	} `json:"metadata"`
	// Patterns are every combination of checkpoints, by key.
	Patterns map[string]struct {
		// Points are the checkpoint names.
		Points []string `json:"points"`
		// Damage is the combination's damage.
		Damage int `json:"damage"`
	} `json:"patterns"`
}

// TestDamageLookupMatchesConfig checks that damage_lookup.json, the source
// of truth for 2026 damage, agrees with config.toml: its coefficients equal
// the [spell] table, and every one of its combinations, recomputed from its
// own checkpoint coordinates with the config's rules, has its damage. A
// change to either alone fails here: retune both, or regenerate the table.
func TestDamageLookupMatchesConfig(t *testing.T) {
	data, err := os.ReadFile(damageLookupPath)
	if err != nil {
		t.Fatal(err)
	}
	var lookup damageLookup
	if err := json.Unmarshal(data, &lookup); err != nil {
		t.Fatal(err)
	}
	rules := damageRules(configtest.Settings(t))

	c := lookup.Metadata.DamageCoefficients
	if !maps.Equal(intKeyed(t, c.SpellBase), rules.Base) || c.BonusConcaveQuad != rules.ConcaveQuadBonus ||
		c.Per100m != rules.Per100m || c.Walkability != rules.Walkability {
		t.Fatalf("damage_lookup.json's coefficients %+v differ from config.toml's [spell] %+v", c, rules)
	}

	var cps []spells.Waypoint
	for name, cp := range lookup.Metadata.Checkpoints {
		cps = append(cps, spells.Waypoint{ID: name, GPS: spells.GPS{Latitude: cp.Latitude, Longitude: cp.Longitude}})
	}
	projected := spells.ProjectWaypoints(cps)
	if len(lookup.Patterns) == 0 {
		t.Fatal("damage_lookup.json has no patterns")
	}
	for key, p := range lookup.Patterns {
		pts := make([]spells.Point, len(p.Points))
		for i, name := range p.Points {
			pt, ok := projected[name]
			if !ok {
				t.Fatalf("%s: unknown checkpoint %q", key, name)
			}
			pts[i] = pt
		}
		if got := mustDamage(t, rules, pts); got != p.Damage {
			t.Errorf("%s: config.toml gives %d, damage_lookup.json says %d", key, got, p.Damage)
		}
	}
}

// intKeyed returns m with its keys, decimal point counts, as ints.
func intKeyed(t *testing.T, m map[string]int) map[int]int {
	t.Helper()
	out := make(map[int]int, len(m))
	for k, v := range m {
		n, err := strconv.Atoi(k)
		if err != nil {
			t.Fatalf("spell_base key %q isn't a point count", k)
		}
		out[n] = v
	}
	return out
}
