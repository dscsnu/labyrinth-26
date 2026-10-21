package qte

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// db is the qte use case's SQL: table qte only (team_qte_answer is
// teamstate's).
type db struct {
	pool *pgxpool.Pool
}

// errOverlap means Postgres refused a run that overlaps another: the
// exclusion constraint behind "one QTE at a time".
var errOverlap = errors.New("qte run overlaps another")

// overlapConstraint is the qte table's exclusion constraint (migration 0015).
const overlapConstraint = "qte_tstzrange_excl"

// selectQTEs reads every QTE, newest first: SELECT … FROM qte ORDER BY id
// DESC.
func (d db) selectQTEs(ctx context.Context) ([]QTE, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, kind, powerup, question, options, correct_option, window_s, created_by, created_at, started_at, ends_at
		FROM qte ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	out := []QTE{}
	var (
		q        QTE
		question *string
		options  []string
		correct  *int
	)
	if _, err := pgx.ForEachRow(rows, []any{&q.ID, &q.Kind, &q.Powerup, &question, &options, &correct, &q.WindowS,
		&q.CreatedBy, &q.CreatedAt, &q.StartedAt, &q.EndsAt}, func() error {
		row := q
		row.CreatedAt = row.CreatedAt.UTC()
		row.StartedAt, row.EndsAt = utc(row.StartedAt), utc(row.EndsAt)
		if question != nil && correct != nil {
			row.Quiz = &Quiz{Question: *question, Options: options, CorrectOption: *correct}
		}
		if row.WindowS != nil {
			w := *row.WindowS
			row.WindowS = &w
		}
		if row.CreatedBy != nil {
			by := *row.CreatedBy
			row.CreatedBy = &by
		}
		out = append(out, row)
		return nil
	}); err != nil {
		return nil, err
	}
	return out, nil
}

// utc returns *t in UTC as a new pointer, or nil.
func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// insertQTE writes a new QTE, started or a draft: one INSERT INTO qte …
// RETURNING id, created_at (Postgres generates both). errOverlap if it
// starts while another runs.
func (d db) insertQTE(ctx context.Context, q QTE, by uuid.UUID) (id int64, createdAt time.Time, err error) {
	var question *string
	var options []string
	var correct *int
	if q.Quiz != nil {
		question, options, correct = &q.Quiz.Question, q.Quiz.Options, &q.Quiz.CorrectOption
	}
	err = d.pool.QueryRow(ctx, `
		INSERT INTO qte (kind, powerup, question, options, correct_option, window_s, created_by, started_at, ends_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		q.Kind, q.Powerup, question, options, correct, q.WindowS, by, q.StartedAt, q.EndsAt).Scan(&id, &createdAt)
	return id, createdAt.UTC(), mapOverlap(err)
}

// updateQTEStarted starts QTE id: one UPDATE qte SET started_at, ends_at
// WHERE it's still a draft. Exactly one row (memory said it was a draft);
// errOverlap if another runs.
func (d db) updateQTEStarted(ctx context.Context, id int64, startedAt, endsAt time.Time) error {
	return mapOverlap(execOne(ctx, d.pool,
		"UPDATE qte SET started_at = $2, ends_at = $3 WHERE id = $1 AND started_at IS NULL", id, startedAt, endsAt))
}

// updateQTEEnded ends QTE id early at endsAt: one UPDATE qte SET ends_at
// WHERE it's started and still open then. Exactly one row (memory said it
// was open).
func (d db) updateQTEEnded(ctx context.Context, id int64, endsAt time.Time) error {
	return execOne(ctx, d.pool,
		"UPDATE qte SET ends_at = $2 WHERE id = $1 AND started_at IS NOT NULL AND ends_at > $2", id, endsAt)
}

// mapOverlap turns the exclusion constraint's violation into errOverlap.
func mapOverlap(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23P01" && pgErr.ConstraintName == overlapConstraint {
		return errOverlap
	}
	return err
}

// execOne runs a statement that must affect exactly one row. Anything else
// means memory and Postgres disagree, which the caller reconciles.
func execOne(ctx context.Context, q *pgxpool.Pool, sql string, args ...any) error {
	tag, err := q.Exec(ctx, sql, args...)
	if err != nil {
		return err
	}
	if n := tag.RowsAffected(); n != 1 {
		return fmt.Errorf("expected 1 row affected, got %d: %s", n, sql)
	}
	return nil
}
