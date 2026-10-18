package teamstate

import (
	"math"
	"slices"
	"sort"
	"testing"
)

// campusPoint is a 2D metric coordinate for a physical QR checkpoint.
//
// Architectural boundary:
//  1. Convex hull classification is a property of the unordered set of waypoints.
//  2. Traversal order (the sequence in which players scan) is independent of hull
//     classification; the teamstate scan buffer matches sets regardless of order.
//  3. Physical walking distance (P_walk or P_tour) and damage scoring are
//     decoupled from shape convexity/concavity to avoid artificial game balancing biases.
type campusPoint struct {
	name string
	x    float64
	y    float64
}

// canonicalCampusWaypoints returns the 11 physical landmark checkpoints of SNU
// in local planar metric coordinates (meters from campus barycenter).
func canonicalCampusWaypoints() []campusPoint {
	return []campusPoint{
		{name: "T6", x: 336.921, y: 309.178},
		{name: "R", x: 450.779, y: 210.196},
		{name: "A", x: 271.088, y: 149.056},
		{name: "CnD", x: 219.361, y: -6.840},
		{name: "G", x: 60.040, y: 268.825},
		{name: "Arc", x: -132.823, y: 183.925},
		{name: "SARC", x: 8.155, y: -222.241},
		{name: "C1", x: -117.838, y: -130.304},
		{name: "Dib", x: -348.341, y: -38.402},
		{name: "DH3", x: -451.155, y: -269.188},
		{name: "ISC", x: -296.187, y: -454.204},
	}
}

// crossProduct2D returns the 2D cross product (signed area) of vector OA and OB.
// Positive indicates a left turn (counter-clockwise), negative a right turn.
func crossProduct2D(o, a, b campusPoint) float64 {
	return (a.x-o.x)*(b.y-o.y) - (a.y-o.y)*(b.x-o.x)
}

// monotoneChainConvexHull computes the strict convex hull vertices of pts using
// Andrew's Monotone Chain algorithm. Returns vertices in counter-clockwise order.
func monotoneChainConvexHull(pts []campusPoint) []campusPoint {
	sorted := make([]campusPoint, len(pts))
	copy(sorted, pts)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].x == sorted[j].x {
			return sorted[i].y < sorted[j].y
		}
		return sorted[i].x < sorted[j].x
	})

	var lower []campusPoint
	for _, p := range sorted {
		for len(lower) >= 2 && crossProduct2D(lower[len(lower)-2], lower[len(lower)-1], p) <= 0 {
			lower = lower[:len(lower)-1]
		}
		lower = append(lower, p)
	}

	var upper []campusPoint
	for i := len(sorted) - 1; i >= 0; i-- {
		p := sorted[i]
		for len(upper) >= 2 && crossProduct2D(upper[len(upper)-2], upper[len(upper)-1], p) <= 0 {
			upper = upper[:len(upper)-1]
		}
		upper = append(upper, p)
	}

	hull := append(lower[:len(lower)-1], upper[:len(upper)-1]...)
	return hull
}

// forEachCombination calls fn on every combination of k points from pts.
func forEachCombination(pts []campusPoint, k int, fn func(subset []campusPoint)) {
	n := len(pts)
	comb := make([]int, k)
	for i := range comb {
		comb[i] = i
	}
	buf := make([]campusPoint, k)

	for {
		for i, idx := range comb {
			buf[i] = pts[idx]
		}
		fn(buf)

		i := k - 1
		for i >= 0 && comb[i] == n-k+i {
			i--
		}
		if i < 0 {
			break
		}
		comb[i]++
		for j := i + 1; j < k; j++ {
			comb[j] = comb[j-1] + 1
		}
	}
}

// TestCampusWaypointGeometryNonCollinear verifies that no three campus checkpoints
// are collinear, establishing non-degeneracy across all 165 triplets.
func TestCampusWaypointGeometryNonCollinear(t *testing.T) {
	pts := canonicalCampusWaypoints()
	minAbsCross := math.Inf(1)

	forEachCombination(pts, 3, func(tri []campusPoint) {
		cp := math.Abs(crossProduct2D(tri[0], tri[1], tri[2]))
		if cp < minAbsCross {
			minAbsCross = cp
		}
		if cp < 1.0 {
			t.Fatalf("points %s, %s, %s are nearly collinear (|cross| = %f)", tri[0].name, tri[1].name, tri[2].name, cp)
		}
	})

	const expectedMinCross = 1980.47
	if math.Abs(minAbsCross-expectedMinCross) > 1.0 {
		t.Fatalf("expected min cross product ~%.2f m^2, got %.2f m^2", expectedMinCross, minAbsCross)
	}
}

// TestCampusShapeClassificationCounts verifies that Andrew's Monotone Chain
// yields the exact canonical counts for all 1,012 combinations.
func TestCampusShapeClassificationCounts(t *testing.T) {
	pts := canonicalCampusWaypoints()

	var (
		lineCount        int
		triangleCount    int
		convexQuadCount  int
		concaveQuadCount int
		convexPentCount  int
		pentHull4Count   int
		pentHull3Count   int
	)

	// k = 2: 55 line segments
	forEachCombination(pts, 2, func(_ []campusPoint) { lineCount++ })
	if lineCount != 55 {
		t.Fatalf("expected 55 line segments, got %d", lineCount)
	}

	// k = 3: 165 triangles (all convex due to non-collinearity)
	forEachCombination(pts, 3, func(tri []campusPoint) {
		triangleCount++
		hull := monotoneChainConvexHull(tri)
		if len(hull) != 3 {
			t.Fatalf("expected 3 hull vertices for triangle %v, got %d", tri, len(hull))
		}
	})
	if triangleCount != 165 {
		t.Fatalf("expected 165 triangles, got %d", triangleCount)
	}

	// k = 4: 330 quadrilaterals (262 convex, 68 concave)
	forEachCombination(pts, 4, func(quad []campusPoint) {
		hull := monotoneChainConvexHull(quad)
		switch len(hull) {
		case 4:
			convexQuadCount++
		case 3:
			concaveQuadCount++
		default:
			t.Fatalf("unexpected hull size %d for quad", len(hull))
		}
	})
	if convexQuadCount != 262 || concaveQuadCount != 68 {
		t.Fatalf("expected 262 convex and 68 concave quads, got %d and %d", convexQuadCount, concaveQuadCount)
	}

	// k = 5: 462 pentagons (235 convex, 216 concave hull=4, 11 concave hull=3)
	forEachCombination(pts, 5, func(pent []campusPoint) {
		hull := monotoneChainConvexHull(pent)
		switch len(hull) {
		case 5:
			convexPentCount++
		case 4:
			pentHull4Count++
		case 3:
			pentHull3Count++
		default:
			t.Fatalf("unexpected hull size %d for pentagon", len(hull))
		}
	})
	if convexPentCount != 235 {
		t.Fatalf("expected 235 convex pentagons, got %d", convexPentCount)
	}
	if pentHull4Count != 216 {
		t.Fatalf("expected 216 pentagons with 4 hull vertices, got %d", pentHull4Count)
	}
	if pentHull3Count != 11 {
		t.Fatalf("expected 11 pentagons with 3 hull vertices, got %d", pentHull3Count)
	}

	total := lineCount + triangleCount + convexQuadCount + concaveQuadCount + convexPentCount + pentHull4Count + pentHull3Count
	if total != 1012 {
		t.Fatalf("expected 1012 total patterns, got %d", total)
	}
}

// TestCampusConcaveInteriorPoints verifies that interior reflex points in
// concave configurations are strictly confined to C1, A, and CnD with exact counts.
func TestCampusConcaveInteriorPoints(t *testing.T) {
	pts := canonicalCampusWaypoints()
	quadInteriorCounts := make(map[string]int)

	forEachCombination(pts, 4, func(quad []campusPoint) {
		hull := monotoneChainConvexHull(quad)
		if len(hull) == 3 {
			hullNames := make([]string, len(hull))
			for i, h := range hull {
				hullNames[i] = h.name
			}
			for _, p := range quad {
				if !slices.Contains(hullNames, p.name) {
					quadInteriorCounts[p.name]++
				}
			}
		}
	})

	expectedQuadInterior := map[string]int{
		"C1":  34,
		"A":   26,
		"CnD": 8,
	}
	for name, count := range expectedQuadInterior {
		if quadInteriorCounts[name] != count {
			t.Errorf("quad interior %s: expected %d, got %d", name, count, quadInteriorCounts[name])
		}
	}
	if len(quadInteriorCounts) != 3 {
		t.Errorf("expected exactly 3 distinct quad interior landmarks, got %d: %v", len(quadInteriorCounts), quadInteriorCounts)
	}

	// 5-point subsets with 3 hull vertices: exactly 2 interior points
	pentInteriorPairCounts := make(map[string]int)
	forEachCombination(pts, 5, func(pent []campusPoint) {
		hull := monotoneChainConvexHull(pent)
		if len(hull) == 3 {
			hullNames := make([]string, len(hull))
			for i, h := range hull {
				hullNames[i] = h.name
			}
			var interior []string
			for _, p := range pent {
				if !slices.Contains(hullNames, p.name) {
					interior = append(interior, p.name)
				}
			}
			sort.Strings(interior)
			key := interior[0] + "+" + interior[1]
			pentInteriorPairCounts[key]++
		}
	})

	expectedPentPairs := map[string]int{
		"A+C1":   5,
		"A+CnD":  4,
		"C1+CnD": 2,
	}
	for pair, count := range expectedPentPairs {
		if pentInteriorPairCounts[pair] != count {
			t.Errorf("pentagon interior pair %s: expected %d, got %d", pair, count, pentInteriorPairCounts[pair])
		}
	}
}
