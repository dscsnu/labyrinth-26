package atlas

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// db is the atlas's SQL: it reads the content tables (waypoint, shape,
// pattern, pattern_waypoint, from migration 0003) once at startup and never
// writes. Content changes only through migrations and seeds, with the backend
// stopped.
type db struct {
	pool *pgxpool.Pool
}

// selectAtlas reads every waypoint, shape and pattern as a Source.
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

	rows, err = d.pool.Query(ctx, `
		SELECT p.id, p.shape_id, COALESCE(array_agg(pw.waypoint_id ORDER BY pw.waypoint_id)
		                                  FILTER (WHERE pw.waypoint_id IS NOT NULL), '{}'),
		       p.damage
		FROM pattern p
		LEFT JOIN pattern_waypoint pw ON pw.pattern_id = p.id
		GROUP BY p.id, p.damage
		ORDER BY p.id`)
	if err != nil {
		return c, err
	}
	c.Patterns, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (PatternSource, error) {
		var p PatternSource
		return p, r.Scan(&p.ID, &p.Shape, &p.Waypoints, &p.Damage)
	})
	return c, err
}
