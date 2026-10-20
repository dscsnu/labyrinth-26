package spells

import (
	"fmt"
	"math"
	"slices"
	"sort"
)

// EarthRadiusM is Earth's volumetric mean radius in metres (IUGG recommendation).
// A physical constant, not a tunable; not in config.toml (AGENTS.md §10).
const EarthRadiusM = 6371000.0

// GPS represents geographic coordinates in degrees (WGS 84).
type GPS struct {
	// Latitude is degrees north of the equator.
	Latitude float64
	// Longitude is degrees east of the prime meridian.
	Longitude float64
}

// Point represents a 2D local planar metric coordinate in metres.
type Point struct {
	// X is metres east of the projection origin.
	X float64
	// Y is metres north of the projection origin.
	Y float64
}

// Waypoint is a physical landmark identified by its 4-digit ID and GPS coordinates.
type Waypoint struct {
	// ID is the 4-digit waypoint ID, e.g. "1001".
	ID string
	// GPS is the geographic location of the waypoint.
	GPS GPS
}

// MeanOrigin computes the arithmetic mean latitude and longitude of a slice of GPS points.
// If the slice is empty, it returns a zero GPS coordinate.
func MeanOrigin(points []GPS) GPS {
	if len(points) == 0 {
		return GPS{}
	}
	var sumLat, sumLon float64
	for _, p := range points {
		sumLat += p.Latitude
		sumLon += p.Longitude
	}
	n := float64(len(points))
	return GPS{
		Latitude:  sumLat / n,
		Longitude: sumLon / n,
	}
}

// Project converts a GPS coordinate to a 2D Point in metres using an equirectangular
// projection centered on origin.
func Project(target GPS, origin GPS) Point {
	lat0Rad := origin.Latitude * math.Pi / 180.0
	dLatRad := (target.Latitude - origin.Latitude) * math.Pi / 180.0
	dLonRad := (target.Longitude - origin.Longitude) * math.Pi / 180.0

	return Point{
		X: EarthRadiusM * dLonRad * math.Cos(lat0Rad),
		Y: EarthRadiusM * dLatRad,
	}
}

// ProjectWaypoints projects a slice of Waypoints onto the local metric plane
// using the mean coordinate of all provided waypoints as the projection origin.
// Returns a map from waypoint ID to its projected Point.
func ProjectWaypoints(waypoints []Waypoint) map[string]Point {
	gpsList := make([]GPS, len(waypoints))
	for i, w := range waypoints {
		gpsList[i] = w.GPS
	}
	origin := MeanOrigin(gpsList)
	out := make(map[string]Point, len(waypoints))
	for _, w := range waypoints {
		out[w.ID] = Project(w.GPS, origin)
	}
	return out
}

// Distance returns the Euclidean straight-line distance in metres between points a and b.
func Distance(a, b Point) float64 {
	return math.Hypot(a.X-b.X, a.Y-b.Y)
}

// CrossProduct returns the 2D cross product of vector OA and OB.
// A positive result indicates a counter-clockwise (left) turn, negative indicates clockwise.
func CrossProduct(o, a, b Point) float64 {
	return (a.X-o.X)*(b.Y-o.Y) - (a.Y-o.Y)*(b.X-o.X)
}

// ConvexHull computes the strict convex hull vertices of pts using Andrew's
// Monotone Chain algorithm. Vertices are returned in counter-clockwise order.
// For degenerate inputs (0, 1, or 2 points), a shallow copy is returned.
func ConvexHull(pts []Point) []Point {
	n := len(pts)
	if n <= 2 {
		return slices.Clone(pts)
	}

	sorted := slices.Clone(pts)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].X == sorted[j].X {
			return sorted[i].Y < sorted[j].Y
		}
		return sorted[i].X < sorted[j].X
	})

	var lower []Point
	for _, p := range sorted {
		for len(lower) >= 2 && CrossProduct(lower[len(lower)-2], lower[len(lower)-1], p) <= 0 {
			lower = lower[:len(lower)-1]
		}
		lower = append(lower, p)
	}

	var upper []Point
	for i := len(sorted) - 1; i >= 0; i-- {
		p := sorted[i]
		for len(upper) >= 2 && CrossProduct(upper[len(upper)-2], upper[len(upper)-1], p) <= 0 {
			upper = upper[:len(upper)-1]
		}
		upper = append(upper, p)
	}

	return append(lower[:len(lower)-1], upper[:len(upper)-1]...)
}

// TourM calculates the shortest closed Euclidean tour visiting all points in pts
// (the Traveling Salesperson closed loop).
// - For 0 or 1 point, TourM is 0.
// - For 2 points, TourM is out and back: 2 * Distance(p0, p1).
// - For 3 to 6 points, all (n-1)! / 2 cyclic directions are evaluated.
func TourM(pts []Point) float64 {
	n := len(pts)
	switch n {
	case 0, 1:
		return 0
	case 2:
		return 2.0 * Distance(pts[0], pts[1])
	}

	rest := make([]int, n-1)
	for i := range rest {
		rest[i] = i + 1
	}

	minTour := math.Inf(1)
	permute(rest, 0, func(order []int) {
		d := Distance(pts[0], pts[order[0]])
		for i := 0; i < len(order)-1; i++ {
			d += Distance(pts[order[i]], pts[order[i+1]])
		}
		d += Distance(pts[order[len(order)-1]], pts[0])
		if d < minTour {
			minTour = d
		}
	})

	return minTour
}

// permute generates all permutations of arr in place and invokes fn on each.
func permute(arr []int, k int, fn func([]int)) {
	if k == len(arr) {
		fn(arr)
		return
	}
	for i := k; i < len(arr); i++ {
		arr[k], arr[i] = arr[i], arr[k]
		permute(arr, k+1, fn)
		arr[k], arr[i] = arr[i], arr[k]
	}
}

// quadPoints is how many waypoints a quadrilateral has: the size the concave
// bonus applies to. Geometry, not a tunable, so not in config.toml (AGENTS.md
// §10); the bonus itself is (spell.bonus.concave_quad).
const quadPoints = 4

// DamageRules are the damage formula's coefficients, from config.toml: the
// [spell] table and game.max_pattern_points. Build one per run and reuse it.
type DamageRules struct {
	// Base is the base coordination damage by waypoint count (spell.base).
	Base map[int]int
	// ConcaveQuadBonus is added for a quadrilateral whose convex hull has 3
	// corners (spell.bonus.concave_quad).
	ConcaveQuadBonus int
	// Per100m is the damage per 100 m of estimated walking (spell.per_100m).
	Per100m float64
	// Walkability scales a straight-line tour to walking distance
	// (spell.walkability).
	Walkability float64
	// MaxPoints is the biggest pattern size (game.max_pattern_points). A
	// pattern of that size deals Base[MaxPoints] flat, with no distance or
	// bonus.
	MaxPoints int
}

// Damage returns the integer spell damage of the pattern made of pts:
// Base[k] flat for the biggest size (k = MaxPoints); otherwise
// round(Base[k] + bonus + Per100m × Walkability × TourM(pts) / 100), the
// bonus being ConcaveQuadBonus for a 4-point pattern whose convex hull has 3
// corners. An error if k is outside 1–MaxPoints or Base has no entry for it.
func (r DamageRules) Damage(pts []Point) (int, error) {
	k := len(pts)
	if k < 1 || k > r.MaxPoints {
		return 0, fmt.Errorf("a %d-point pattern: sizes are 1-%d", k, r.MaxPoints)
	}
	base, ok := r.Base[k]
	if !ok {
		return 0, fmt.Errorf("no base damage for %d points", k)
	}
	if k == r.MaxPoints {
		return base, nil
	}
	bonus := 0
	if k == quadPoints && len(ConvexHull(pts)) == 3 {
		bonus = r.ConcaveQuadBonus
	}
	walkM := TourM(pts) * r.Walkability
	return int(math.Round(float64(base+bonus) + r.Per100m*(walkM/100.0))), nil
}
