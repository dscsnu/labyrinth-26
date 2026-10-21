package teamstate

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// db is the team state use case's SQL: the per-team tables (team_scan,
// team_pattern, team_selection, team_cast), read at startup and by reconcile, and
// written through by write.go, one transaction per operation. The content
// tables belong to the atlas use case.
type db struct {
	pool *pgxpool.Pool
}

// teamRows is one team's stored state, by IDs.
type teamRows struct {
	waypoints []string      // distinct waypoints in team_scan
	patterns  []int         // team_pattern keys
	selection *selectionRow // team_selection, if any
	stack     []scanRow     // the stack: newest scans after the selection, oldest first
	casts     []castRow     // every team_cast row, oldest first
	boss      *bossRow      // team_boss, if the team has a row
}

// bossRow is a team_boss row: the team's progress through the bosses and
// where its current boss phase began (NULLs: see migration 0010).
type bossRow struct {
	defeated       int
	damage         int
	multiplier     float64
	phaseHealth    *int       // NULL: full health (game.health)
	phaseStartedAt *time.Time // NULL: the team's start
	cooldownsReset int        // the cast log's length at the last reset; 0: never
}

// castRow is a team_cast row.
type castRow struct {
	patternID        int
	castBy           uuid.UUID
	castAt           time.Time
	alreadyCompleted bool
	dealt            int
	bossSerial       int // 0 for NULL
	defeatedBoss     bool
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

	// Every cast per team, oldest first: the cast log.
	rows, err = d.pool.Query(ctx, `
		SELECT team_id, pattern_id, cast_by, cast_at, already_completed, dealt, COALESCE(boss_serial, 0), defeated_boss
		FROM team_cast`+filter+`
		ORDER BY team_id, id`, teamID)
	if err != nil {
		return nil, err
	}
	var cast castRow
	if _, err := pgx.ForEachRow(rows, []any{&id, &cast.patternID, &cast.castBy, &cast.castAt, &cast.alreadyCompleted,
		&cast.dealt, &cast.bossSerial, &cast.defeatedBoss}, func() error {
		t := get(id)
		t.casts = append(t.casts, cast)
		return nil
	}); err != nil {
		return nil, err
	}

	// Each team's progress through the bosses, if it has a row.
	rows, err = d.pool.Query(ctx,
		"SELECT team_id, defeated, damage, multiplier, phase_health, phase_started_at, cooldowns_reset_casts FROM team_boss"+filter, teamID)
	if err != nil {
		return nil, err
	}
	var boss bossRow
	if _, err := pgx.ForEachRow(rows, []any{&id, &boss.defeated, &boss.damage, &boss.multiplier,
		&boss.phaseHealth, &boss.phaseStartedAt, &boss.cooldownsReset}, func() error {
		row := boss
		get(id).boss = &row
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
	// after the selection was made, returned oldest first, with each scan's
	// wrong flag (so a poisoned attempt stays poisoned across a restart).
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

// insertCompletingScan logs a scan that completed a pattern (a cast), in one
// transaction, in order:
//  1. INSERT team_scan: the scan itself
//  2. INSERT team_pattern … ON CONFLICT DO NOTHING: the first completion only
//  3. INSERT team_cast: every cast, repeats included, with what it did to the
//     bosses (c.Dealt, c.Boss, NULL when 0, c.DefeatedBoss)
//  4. UPSERT team_boss: the team's progress after the cast (p.Defeated,
//     p.Damage, and the phase: p.PhaseHealth, p.PhaseStart, which a defeat
//     moves to the next boss). A new row takes the multiplier's default
//     (1); an existing one keeps its multiplier, which casts never change.
//  5. DELETE team_selection: which also empties the stack
//
// The scan is written with wrong = false: a wrong scan can't complete.
func (d db) insertCompletingScan(ctx context.Context, teamID, waypointID string, userID uuid.UUID, at time.Time, patternID int, c Cast, p BossProgress) error {
	var bossSerial *int
	if c.Boss != 0 {
		bossSerial = &c.Boss
	}
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			"INSERT INTO team_scan (team_id, waypoint_id, user_id, scanned_at, wrong) VALUES ($1, $2, $3, $4, false)",
			teamID, waypointID, userID, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO team_pattern (team_id, pattern_id, completed_at) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, teamID, patternID, at); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO team_cast (team_id, pattern_id, cast_by, cast_at, already_completed, dealt, boss_serial, defeated_boss)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			teamID, patternID, userID, at, c.AlreadyCompleted, c.Dealt, bossSerial, c.DefeatedBoss); err != nil {
			return err
		}
		if err := upsertBoss(ctx, tx, teamID, p); err != nil {
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

// upsertBossPhase writes teamID's boss progress (a revive): one UPSERT of
// team_boss (upsertBoss).
func (d db) upsertBossPhase(ctx context.Context, teamID string, p BossProgress) error {
	return upsertBoss(ctx, d.pool, teamID, p)
}

// upsertBoss is the one INSERT … ON CONFLICT DO UPDATE of team_boss that
// every boss write uses, on the pool or inside a transaction: defeated,
// damage, phase_health and phase_started_at from p. The multiplier is left
// as stored (1 for a new row): nothing in a cast or revive changes it.
func upsertBoss(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, teamID string, p BossProgress) error {
	_, err := q.Exec(ctx, `
		INSERT INTO team_boss (team_id, defeated, damage, phase_health, phase_started_at) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (team_id) DO UPDATE SET defeated = EXCLUDED.defeated, damage = EXCLUDED.damage,
			phase_health = EXCLUDED.phase_health, phase_started_at = EXCLUDED.phase_started_at`,
		teamID, p.Defeated, p.Damage, p.PhaseHealth, p.PhaseStart)
	return err
}

// upsertCooldownsReset records an admin's cooldown reset of teamID with the
// cast log's length then: one INSERT … ON CONFLICT DO UPDATE of
// team_boss.cooldowns_reset_casts (a new row takes every other column's
// default: no progress).
func (d db) upsertCooldownsReset(ctx context.Context, teamID string, casts int) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO team_boss (team_id, cooldowns_reset_casts) VALUES ($1, $2)
		ON CONFLICT (team_id) DO UPDATE SET cooldowns_reset_casts = EXCLUDED.cooldowns_reset_casts`, teamID, casts)
	return err
}
