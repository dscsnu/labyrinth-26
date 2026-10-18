package teamstate

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// db is the team state use case's SQL. The content tables (waypoint, shape,
// pattern, pattern_waypoint) are read once at startup. The per-team tables
// (team_scan, team_pattern, team_selection) are read at startup and by
// reconcile, and written through by write.go, one transaction per operation.
type db struct {
	pool *pgxpool.Pool
}

// patternRow is one pattern as stored: its key, shape and waypoint IDs.
type patternRow struct {
	id        int
	shapeID   string
	waypoints []string
}

// contentRows is everything in the content tables.
type contentRows struct {
	waypoints []Waypoint
	shapes    []Shape
	patterns  []patternRow
}

// selectContent reads every waypoint, shape and pattern.
func (d db) selectContent(ctx context.Context) (contentRows, error) {
	var c contentRows

	rows, err := d.pool.Query(ctx, "SELECT id, name, latitude, longitude FROM waypoint ORDER BY id")
	if err != nil {
		return c, err
	}
	if c.waypoints, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Waypoint, error) {
		var w Waypoint
		return w, r.Scan(&w.ID, &w.Name, &w.Latitude, &w.Longitude)
	}); err != nil {
		return c, err
	}

	rows, err = d.pool.Query(ctx, "SELECT id, name FROM shape ORDER BY id")
	if err != nil {
		return c, err
	}
	if c.shapes, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Shape, error) {
		var s Shape
		return s, r.Scan(&s.ID, &s.Name)
	}); err != nil {
		return c, err
	}

	rows, err = d.pool.Query(ctx, `
		SELECT p.id, p.shape_id, COALESCE(array_agg(pw.waypoint_id ORDER BY pw.waypoint_id)
		                                  FILTER (WHERE pw.waypoint_id IS NOT NULL), '{}')
		FROM pattern p
		LEFT JOIN pattern_waypoint pw ON pw.pattern_id = p.id
		GROUP BY p.id
		ORDER BY p.id`)
	if err != nil {
		return c, err
	}
	c.patterns, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (patternRow, error) {
		var p patternRow
		return p, r.Scan(&p.id, &p.shapeID, &p.waypoints)
	})
	return c, err
}

// teamRows is one team's stored state, by IDs.
type teamRows struct {
	waypoints []string      // distinct waypoints in team_scan
	patterns  []int         // team_pattern keys
	selection *selectionRow // team_selection, if any
	stack     []scanRow     // the stack: newest scans after the selection, oldest first
}

// selectionRow is a team_selection row.
type selectionRow struct {
	shapeID    string
	selectedBy uuid.UUID
	selectedAt time.Time
}

// scanRow is a team_scan row in a team's stack.
type scanRow struct {
	waypointID string
	userID     uuid.UUID
	scannedAt  time.Time
	wrong      bool
}

// selectTeams reads the stored state of every team, or of one team when
// teamID is non-empty, with at most stackSize stack scans each. Teams with
// nothing stored are absent from the map.
func (d db) selectTeams(ctx context.Context, teamID string, stackSize int) (map[string]*teamRows, error) {
	out := map[string]*teamRows{}
	get := func(id string) *teamRows {
		if out[id] == nil {
			out[id] = &teamRows{}
		}
		return out[id]
	}
	const filter = " WHERE ($1 = '' OR team_id = $1)"

	rows, err := d.pool.Query(ctx, "SELECT DISTINCT team_id, waypoint_id FROM team_scan"+filter, teamID)
	if err != nil {
		return nil, err
	}
	var id, waypointID string
	if _, err := pgx.ForEachRow(rows, []any{&id, &waypointID}, func() error {
		t := get(id)
		t.waypoints = append(t.waypoints, waypointID)
		return nil
	}); err != nil {
		return nil, err
	}

	rows, err = d.pool.Query(ctx, "SELECT team_id, pattern_id FROM team_pattern"+filter, teamID)
	if err != nil {
		return nil, err
	}
	var patternID int
	if _, err := pgx.ForEachRow(rows, []any{&id, &patternID}, func() error {
		t := get(id)
		t.patterns = append(t.patterns, patternID)
		return nil
	}); err != nil {
		return nil, err
	}

	rows, err = d.pool.Query(ctx, "SELECT team_id, shape_id, selected_by, selected_at FROM team_selection"+filter, teamID)
	if err != nil {
		return nil, err
	}
	var sel selectionRow
	if _, err := pgx.ForEachRow(rows, []any{&id, &sel.shapeID, &sel.selectedBy, &sel.selectedAt}, func() error {
		row := sel
		get(id).selection = &row
		return nil
	}); err != nil {
		return nil, err
	}

	// The stack: for each team with a selection, its newest stackSize scans
	// after the selection was made, returned oldest first. The wrong flag is
	// read too, so wrong scans can be rebuilt as never-stale.
	rows, err = d.pool.Query(ctx, `
		SELECT sel.team_id, r.waypoint_id, r.user_id, r.scanned_at, r.wrong
		FROM team_selection sel
		JOIN LATERAL (
			SELECT ts.id, ts.waypoint_id, ts.user_id, ts.scanned_at, ts.wrong
			FROM team_scan ts
			WHERE ts.team_id = sel.team_id AND ts.id > sel.after_scan_id
			ORDER BY ts.id DESC
			LIMIT $2
		) r ON true
		WHERE ($1 = '' OR sel.team_id = $1)
		ORDER BY sel.team_id, r.id`, teamID, stackSize)
	if err != nil {
		return nil, err
	}
	var scan scanRow
	_, err = pgx.ForEachRow(rows, []any{&id, &scan.waypointID, &scan.userID, &scan.scannedAt, &scan.wrong}, func() error {
		t := get(id)
		t.stack = append(t.stack, scan)
		return nil
	})
	return out, err
}

// insertScan logs one accepted scan in team_scan, which is both the audit
// trail and the source of the stack after a restart. wrong marks a scan that
// didn't fit the selected shape.
func (d db) insertScan(ctx context.Context, teamID, waypointID string, userID uuid.UUID, at time.Time, wrong bool) error {
	_, err := d.pool.Exec(ctx,
		"INSERT INTO team_scan (team_id, waypoint_id, user_id, scanned_at, wrong) VALUES ($1, $2, $3, $4, $5)",
		teamID, waypointID, userID, at, wrong)
	return err
}

// insertCompletingScan logs a scan that completed a pattern, in one
// transaction: the team_scan row, the team_pattern row (no-op if the pattern
// was completed before), and deleting the selection, which also empties the
// stack. wrong is always false here (a wrong scan can't complete), but it's
// passed through for one uniform call.
func (d db) insertCompletingScan(ctx context.Context, teamID, waypointID string, userID uuid.UUID, at time.Time, patternID int, wrong bool) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			"INSERT INTO team_scan (team_id, waypoint_id, user_id, scanned_at, wrong) VALUES ($1, $2, $3, $4, $5)",
			teamID, waypointID, userID, at, wrong); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO team_pattern (team_id, pattern_id, completed_at) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, teamID, patternID, at); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "DELETE FROM team_selection WHERE team_id = $1", teamID)
		return err
	})
}

// upsertSelection sets the team's selected shape and starts its stack empty,
// in one statement: INSERT INTO team_selection … ON CONFLICT (team_id) DO
// UPDATE, with after_scan_id set to the team's latest team_scan.id (0 if
// none), so earlier scans don't count.
func (d db) upsertSelection(ctx context.Context, teamID, shapeID string, by uuid.UUID, at time.Time) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO team_selection (team_id, shape_id, selected_by, selected_at, after_scan_id)
		SELECT $1::text, $2, $3, $4, COALESCE((SELECT max(id) FROM team_scan WHERE team_id = $1::text), 0)
		ON CONFLICT (team_id) DO UPDATE SET
			shape_id = EXCLUDED.shape_id, selected_by = EXCLUDED.selected_by,
			selected_at = EXCLUDED.selected_at, after_scan_id = EXCLUDED.after_scan_id`,
		teamID, shapeID, by, at)
	return err
}

// deleteSelection removes the team's selection, which also empties its stack.
// It fails unless exactly one row is deleted: memory had a selection, so
// Postgres must too, and anything else means they disagree.
func (d db) deleteSelection(ctx context.Context, teamID string) error {
	tag, err := d.pool.Exec(ctx, "DELETE FROM team_selection WHERE team_id = $1", teamID)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("delete selection of team %s: %d rows, expected 1", teamID, n)
	}
	return nil
}
