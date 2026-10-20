package atlas

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// db is the atlas's SQL: it reads the content tables (waypoint, shape,
// pattern, pattern_waypoint, from migration 0003; spell_class, from 0012)
// once at startup and never writes. Content changes only through migrations
// and seeds, with the backend stopped.
type db struct {
	pool *pgxpool.Pool
}

// selectAtlas reads every waypoint, shape, spell class and pattern as a
// Source. A pattern with no damage yet (migration 0008 ran but the spells seed
// didn't) reads as damage 0, and one with no spell class yet (migration 0012)
// as class "", so New can refuse it with a message naming the pattern instead
// of the scan failing on NULL.
func (d db) selectAtlas(ctx context.Context) (Source, error) {
	var c Source

	rows, err := d.pool.Query(ctx, "SELECT id, name, latitude, longitude FROM waypoint ORDER BY id")
	if err != nil {
		return c, err
	}
	if c.Waypoints, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Waypoint, error) {
		var w Waypoint
		return w, r.Scan(&w.ID, &w.Name, &w.Latitude, &w.Longitude)
	}); err != nil {
		return c, err
	}

	rows, err = d.pool.Query(ctx, "SELECT id, name FROM shape ORDER BY id")
	if err != nil {
		return c, err
	}
	if c.Shapes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Shape, error) {
		var s Shape
		return s, r.Scan(&s.ID, &s.Name)
	}); err != nil {
		return c, err
	}

	rows, err = d.pool.Query(ctx, "SELECT id, name, points FROM spell_class ORDER BY points")
	if err != nil {
		return c, err
	}
	if c.SpellClasses, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (SpellClass, error) {
		var s SpellClass
		return s, r.Scan(&s.ID, &s.Name, &s.Points)
	}); err != nil {
		return c, err
	}

	rows, err = d.pool.Query(ctx, `
		SELECT p.id, p.shape_id, COALESCE(array_agg(pw.waypoint_id ORDER BY pw.waypoint_id)
		                                  FILTER (WHERE pw.waypoint_id IS NOT NULL), '{}'),
		       COALESCE(p.damage, 0), COALESCE(p.spell_class_id, '')
		FROM pattern p
		LEFT JOIN pattern_waypoint pw ON pw.pattern_id = p.id
		GROUP BY p.id, p.damage, p.spell_class_id
		ORDER BY p.id`)
	if err != nil {
		return c, err
	}
	c.Patterns, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PatternSource, error) {
		var p PatternSource
		return p, r.Scan(&p.ID, &p.Shape, &p.Waypoints, &p.Damage, &p.Class)
	})
	return c, err
}
