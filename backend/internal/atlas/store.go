package atlas

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"

	"labyrinth/internal/platform/postgres"
)

// The in-memory layout. Everything is built once by Load and never changes,
// so there are no locks:
//
//	Atlas
//	  waypoints []Waypoint          index → waypoint (ID order)
//	  shapes    []Shape             index → shape (ID order)
//	  patterns  []Pattern           index → pattern (Postgres key order), each
//	                                with its Spell
//	  waypointIndex, shapeIndex     ID → index
//	  patternByID                   Postgres key → index
//	  patternBySet                  exact WaypointSet → index
//	  shapePatterns                 shape index → its pattern indexes

// waypointIDPattern matches a waypoint ID: exactly 4 digits. A format, not a
// tunable; it matches the waypoint.id CHECK in migration 0003.
var waypointIDPattern = regexp.MustCompile(`^[0-9]{4}$`)

// Source is the content an Atlas is built from, by IDs, as stored in
// Postgres. Load reads it from the content tables; tests build one by hand.
type Source struct {
	// Waypoints are every waypoint, in ID order.
	Waypoints []Waypoint
	// Shapes are every shape, in ID order.
	Shapes []Shape
	// Patterns are every pattern, in key order.
	Patterns []PatternSource
}

// PatternSource is one pattern as stored: its key, its shape's ID and its
// waypoint IDs.
type PatternSource struct {
	// ID is pattern.id.
	ID int
	// Shape is the shape's ID, e.g. "square".
	Shape string
	// Waypoints are the waypoint IDs, in any order.
	Waypoints []string
}

// Rules are the atlas's settings from config.toml.
type Rules struct {
	// MaxPatternPoints is the most waypoints in any pattern
	// (game.max_pattern_points); content with a bigger pattern is refused.
	MaxPatternPoints int
	// Spell holds the spell model's coefficients. Not in config.toml yet:
	// zero until the spell model is added (README.md, "Spells").
	Spell SpellRules
}

// SpellRules are the coefficients that turn a pattern's geometry into its
// spell's damage. Not wired yet: the spell model adds them to config.toml (a
// [spell] table), settings.go and server.Build, and computes spells in Load.
type SpellRules struct {
	// PerPoint is damage per waypoint in the pattern (one per member who
	// had to be there).
	PerPoint float64
	// Per100m is damage per 100 m of estimated walking distance (WalkM).
	Per100m float64
	// Walkability scales straight-line distance to walking distance
	// (Geometry.WalkM = TourM × Walkability).
	Walkability float64
}

// Load reads the content tables, validates them against rules, and builds the
// Atlas. Call once at startup, before any use case that reads it (teamstate).
func Load(ctx context.Context, pg *postgres.DB, log *slog.Logger, rules Rules) (*Atlas, error) {
	src, err := db{pool: pg.Pool()}.selectAtlas(ctx)
	if err != nil {
		return nil, fmt.Errorf("load atlas: %w", err)
	}
	a, err := New(src, rules)
	if err != nil {
		return nil, fmt.Errorf("invalid atlas: %w", err)
	}

	// Spells are computed here, once, when they're added (README.md,
	// "Spells"): each Waypoint.Position from its latitude and longitude, then
	// each Pattern.Spell from its waypoints' positions and rules.Spell. Both
	// live only in memory and are rebuilt from Postgres on every start.

	log.Info("loaded atlas", "waypoints", len(a.waypoints), "shapes", len(a.shapes), "patterns", len(a.patterns))
	return a, nil
}

// New validates src against rules and builds the Atlas from it: indexes,
// lookup tables and the startup checks (README.md, "Rules and cases"). Load
// calls it with what Postgres holds; tests call it with a fixture.
func New(src Source, rules Rules) (*Atlas, error) {
	if len(src.Waypoints) > MaxWaypoints {
		return nil, fmt.Errorf("%d waypoints; at most %d fit in a WaypointSet", len(src.Waypoints), MaxWaypoints)
	}
	if len(src.Shapes) > MaxShapes {
		return nil, fmt.Errorf("%d shapes; at most %d fit in a ShapeSet", len(src.Shapes), MaxShapes)
	}
	if len(src.Patterns) > MaxPatterns {
		return nil, fmt.Errorf("%d patterns; at most %d fit in a PatternSet", len(src.Patterns), MaxPatterns)
	}

	a := &Atlas{
		waypointIndex: make(map[string]int, len(src.Waypoints)),
		shapeIndex:    make(map[string]int, len(src.Shapes)),
		patternByID:   make(map[int]int, len(src.Patterns)),
		patternBySet:  make(map[WaypointSet]int, len(src.Patterns)),
		shapePatterns: make([][]int, len(src.Shapes)),
	}
	for i, w := range src.Waypoints {
		if !waypointIDPattern.MatchString(w.ID) {
			return nil, fmt.Errorf("waypoint id %q is not 4 digits", w.ID)
		}
		a.waypoints = append(a.waypoints, w)
		a.waypointIndex[w.ID] = i
	}
	for i, s := range src.Shapes {
		a.shapes = append(a.shapes, s)
		a.shapeIndex[s.ID] = i
	}
	for i, p := range src.Patterns {
		shape, ok := a.shapeIndex[p.Shape]
		if !ok {
			return nil, fmt.Errorf("pattern %d: unknown shape %q", p.ID, p.Shape)
		}
		set, err := a.SetOf(p.Waypoints)
		if err != nil {
			return nil, fmt.Errorf("pattern %d: %w", p.ID, err)
		}
		if set == 0 {
			return nil, fmt.Errorf("pattern %d has no waypoints", p.ID)
		}
		if set.Len() > rules.MaxPatternPoints {
			return nil, fmt.Errorf("pattern %d has %d waypoints; game.max_pattern_points is %d", p.ID, set.Len(), rules.MaxPatternPoints)
		}
		if other, dup := a.patternBySet[set]; dup {
			return nil, fmt.Errorf("patterns %d and %d use the same waypoints", a.patterns[other].ID, p.ID)
		}

		a.patterns = append(a.patterns, Pattern{ID: p.ID, Shape: shape, Waypoints: set})
		a.patternByID[p.ID] = i
		a.patternBySet[set] = i
		a.shapePatterns[shape] = append(a.shapePatterns[shape], i)
	}
	for shape, ps := range a.shapePatterns {
		if err := a.checkNesting(shape, ps); err != nil {
			return nil, err
		}
	}
	return a, nil
}

// checkNesting rejects a pattern contained in a larger pattern of the same
// shape: scanning the larger one, the stack would pass through the smaller
// one first and complete it early.
func (a *Atlas) checkNesting(shape int, patterns []int) error {
	for _, small := range patterns {
		for _, big := range patterns {
			inner, outer := a.patterns[small].Waypoints, a.patterns[big].Waypoints
			if inner.Len() < outer.Len() && outer.Contains(inner) {
				return fmt.Errorf("shape %q: pattern %d is inside pattern %d, so it would complete first",
					a.shapes[shape].ID, a.patterns[small].ID, a.patterns[big].ID)
			}
		}
	}
	return nil
}
