package teams

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"labyrinth/internal/platform/postgres"
)

// db is the teams use case's SQL against the team and teammember tables. It's
// called by store.go (loading at startup, reloading after a failed write) and
// by write.go (the write-through step). Rules are enforced in memory; the
// schema's constraints are a backstop.
type db struct {
	pool *pgxpool.Pool
}

var (
	// errTeamIDTaken means the random team ID collided; Create tries another.
	errTeamIDTaken = errors.New("team id already taken")
	// errUserInTeam means the unique constraint on teammember.user_id fired: the
	// user is already in a team.
	errUserInTeam = errors.New("user is already in a team")
)

// querier is a pool or a transaction, for queries that run in either.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// memberSelect selects members with their profile; callers add WHERE and
// ORDER BY.
const memberSelect = `
	SELECT tm.team_id, u.id, u.name, u.avatar_url, tm.is_ready, tm.joined_at
	FROM teammember tm
	JOIN userprofile u ON u.id = tm.user_id`

// memberRow is a member plus the team it belongs to.
type memberRow struct {
	TeamID string
	Member
}

// scanMember reads one memberSelect row.
func scanMember(row pgx.CollectableRow) (memberRow, error) {
	var m memberRow
	err := row.Scan(&m.TeamID, &m.ID, &m.Name, &m.AvatarURL, &m.IsReady, &m.JoinedAt)
	return m, err
}

// selectAll reads every team with its members, in join order.
func (d db) selectAll(ctx context.Context) ([]Team, error) {
	rows, err := d.pool.Query(ctx,
		"SELECT id, name, leader_id, status, created_at, started_at FROM team ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	teams, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (Team, error) {
		var t Team
		err := row.Scan(&t.ID, &t.Name, &t.LeaderID, &t.Status, &t.CreatedAt, &t.StartedAt)
		return t, err
	})
	if err != nil {
		return nil, err
	}

	rows, err = d.pool.Query(ctx, memberSelect+" ORDER BY tm.joined_at, u.id")
	if err != nil {
		return nil, err
	}
	members, err := pgx.CollectRows(rows, scanMember)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]*Team, len(teams))
	for i := range teams {
		byID[teams[i].ID] = &teams[i]
	}
	for _, m := range members {
		if t := byID[m.TeamID]; t != nil {
			t.Members = append(t.Members, m.Member)
		}
	}
	return teams, nil
}

// selectOne reads one team with its members, or postgres.ErrNotFound.
func (d db) selectOne(ctx context.Context, teamID string) (Team, error) {
	return selectTeam(ctx, d.pool, teamID)
}

// selectTeam reads one team with its members through q, in join order.
func selectTeam(ctx context.Context, q querier, teamID string) (Team, error) {
	var t Team
	err := q.QueryRow(ctx,
		"SELECT id, name, leader_id, status, created_at, started_at FROM team WHERE id = $1", teamID).
		Scan(&t.ID, &t.Name, &t.LeaderID, &t.Status, &t.CreatedAt, &t.StartedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, postgres.ErrNotFound
	}
	if err != nil {
		return t, err
	}
	rows, err := q.Query(ctx, memberSelect+" WHERE tm.team_id = $1 ORDER BY tm.joined_at, u.id", teamID)
	if err != nil {
		return t, err
	}
	members, err := pgx.CollectRows(rows, scanMember)
	for _, m := range members {
		t.Members = append(t.Members, m.Member)
	}
	return t, err
}

// insertTeam creates a LOBBY team with leaderID as its only member, in one
// transaction, and returns it as stored.
func (d db) insertTeam(ctx context.Context, teamID, name string, leaderID uuid.UUID) (Team, error) {
	var team Team
	err := pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `
			INSERT INTO team (id, name, leader_id) VALUES ($1, $2, $3)
			ON CONFLICT (id) DO NOTHING`, teamID, name, leaderID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return errTeamIDTaken
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO teammember (team_id, user_id) VALUES ($1, $2)", teamID, leaderID); err != nil {
			return mapMemberInsertErr(err)
		}
		team, err = selectTeam(ctx, tx, teamID)
		return err
	})
	return team, err
}

// insertMember adds userID to teamID and returns the stored member.
func (d db) insertMember(ctx context.Context, teamID string, userID uuid.UUID) (Member, error) {
	var m Member
	err := d.pool.QueryRow(ctx, `
		WITH ins AS (
			INSERT INTO teammember (team_id, user_id) VALUES ($1, $2)
			RETURNING user_id, is_ready, joined_at
		)
		SELECT u.id, u.name, u.avatar_url, ins.is_ready, ins.joined_at
		FROM ins JOIN userprofile u ON u.id = ins.user_id`, teamID, userID).
		Scan(&m.ID, &m.Name, &m.AvatarURL, &m.IsReady, &m.JoinedAt)
	return m, mapMemberInsertErr(err)
}

// removeMember takes userID out of teamID. Named for the operation rather
// than one statement, it runs in one transaction:
//  1. DELETE FROM teammember (the member)
//  2. UPDATE team SET leader_id, only if newLeader is set
//  3. DELETE FROM team, only if deleteTeam is set (the team is now empty)
//
// Each statement must affect exactly one row (execOne); anything else rolls
// the whole transaction back.
func (d db) removeMember(ctx context.Context, teamID string, userID uuid.UUID, newLeader *uuid.UUID, deleteTeam bool) error {
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		if err := execOne(ctx, tx,
			"DELETE FROM teammember WHERE team_id = $1 AND user_id = $2", teamID, userID); err != nil {
			return err
		}
		if newLeader != nil {
			if err := execOne(ctx, tx,
				"UPDATE team SET leader_id = $2 WHERE id = $1", teamID, *newLeader); err != nil {
				return err
			}
		}
		if deleteTeam {
			return execOne(ctx, tx, "DELETE FROM team WHERE id = $1", teamID)
		}
		return nil
	})
}

// updateReady sets a member's ready flag.
func (d db) updateReady(ctx context.Context, teamID string, userID uuid.UUID, ready bool) error {
	return execOne(ctx, d.pool,
		"UPDATE teammember SET is_ready = $3 WHERE team_id = $1 AND user_id = $2", teamID, userID, ready)
}

// updateStarted moves a LOBBY team to IN_GAME and returns its start time.
func (d db) updateStarted(ctx context.Context, teamID string) (time.Time, error) {
	var startedAt time.Time
	err := d.pool.QueryRow(ctx, `
		UPDATE team SET status = 'IN_GAME', started_at = now()
		WHERE id = $1 AND status = 'LOBBY'
		RETURNING started_at`, teamID).Scan(&startedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return startedAt, fmt.Errorf("start team %s: no LOBBY team with that id", teamID)
	}
	return startedAt, err
}

// execer is a pool or a transaction, for statements that run in either.
type execer interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// execOne runs a statement that must affect exactly one row. Anything else
// means memory and database disagree, which the caller reconciles.
func execOne(ctx context.Context, q execer, sql string, args ...any) error {
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("expected 1 row affected, got %d: %s", n, sql)
	}
	return nil
}

// mapMemberInsertErr turns the one-team-per-user constraint violation into
// errUserInTeam and returns other errors unchanged.
func mapMemberInsertErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "teammember_user_id_key" {
		return errUserInTeam
	}
	return err
}
