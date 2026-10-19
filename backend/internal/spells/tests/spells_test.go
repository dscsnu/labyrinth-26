package tests

import (
	"context"
	"math"
	"testing"

	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/spells"
)

// TestPureMathGeometry checks geometry algorithms and damage formulas on
// synthetic coordinates without needing a database connection.
func TestPureMathGeometry(t *testing.T) {
	set := configtest.Settings(t)
	perPoint := set.Spell.PerPoint
	per100m := set.Spell.Per100m
	walk := set.Spell.Walkability

	t.Run("1-point pattern damage", func(t *testing.T) {
		p1 := []spells.Point{{X: 100, Y: 100}}
		tour := spells.TourM(p1)
		if tour != 0 {
			t.Fatalf("expected 0 tour for 1 point, got %f", tour)
		}
		dmg := spells.Damage(1, tour, perPoint, per100m, walk)
		expected := int(math.Round(perPoint))
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
		dmg := spells.Damage(2, tour, perPoint, per100m, walk)
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

	t.Run("doubling per_point changes damage by formula", func(t *testing.T) {
		pts := []spells.Point{
			{X: 0, Y: 0},
			{X: 150, Y: 50},
			{X: 80, Y: 200},
		}
		k := len(pts)
		tour := spells.TourM(pts)
		dmg1 := spells.Damage(k, tour, perPoint, per100m, walk)
		dmg2 := spells.Damage(k, tour, perPoint*2.0, per100m, walk)
		diff := dmg2 - dmg1
		expectedDiff := int(math.Round(perPoint * float64(k)))
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
		dmg := spells.Damage(6, tour, perPoint, per100m, walk)
		if dmg <= 0 {
			t.Fatalf("expected positive damage for hexagon, got %d", dmg)
		}
		hull := spells.ConvexHull(hex)
		if len(hull) != 6 {
			t.Fatalf("expected 6 hull vertices for regular hexagon, got %d", len(hull))
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
		tour := spells.TourM(pts)
		recomputed := spells.Damage(len(pts), tour, set.Spell.PerPoint, set.Spell.Per100m, set.Spell.Walkability)
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

		tour := spells.TourM(pts)
		computed := spells.Damage(len(pts), tour, set.Spell.PerPoint, set.Spell.Per100m, set.Spell.Walkability)
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
