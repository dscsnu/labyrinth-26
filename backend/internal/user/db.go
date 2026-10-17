package user

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"labyrinth/internal/platform/postgres"
)

// db is the user use case's SQL against the userprofile table.
type db struct {
	pool *pgxpool.Pool
}

// userColumns is the column list scanUser expects, in order.
const userColumns = "id, email, name, avatar_url, role, created_at"

// scanUser reads one userprofile row, mapping no rows to
// postgres.ErrNotFound.
func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.Role, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, postgres.ErrNotFound
	}
	return u, err
}

// selectByID reads the user with the given ID.
func (d db) selectByID(ctx context.Context, id uuid.UUID) (User, error) {
	return scanUser(d.pool.QueryRow(ctx, "SELECT "+userColumns+" FROM userprofile WHERE id = $1", id))
}

// emailConstraint is the unique constraint on userprofile.email (migration
// 0001, Postgres's default name for it).
const emailConstraint = "userprofile_email_key"

// upsertGoogle creates the user on first sign-in and refreshes their profile
// later, in one statement: INSERT INTO userprofile … ON CONFLICT (google_sub)
// DO UPDATE … RETURNING the row. Keyed by Google's stable subject ID, not email. Role is never
// touched, and a sign-in without a picture keeps the stored avatar.
// ErrEmailInUse if the email belongs to another Google account's user.
func (d db) upsertGoogle(ctx context.Context, p GoogleProfile) (User, error) {
	var avatar *string
	if p.AvatarURL != "" {
		avatar = &p.AvatarURL
	}
	u, err := scanUser(d.pool.QueryRow(ctx, `
		INSERT INTO userprofile (google_sub, email, name, avatar_url)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (google_sub) DO UPDATE SET
			email         = EXCLUDED.email,
			name          = EXCLUDED.name,
			avatar_url    = COALESCE(EXCLUDED.avatar_url, userprofile.avatar_url),
			last_login_at = now()
		RETURNING `+userColumns,
		p.Subject, p.Email, p.Name, avatar))
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == emailConstraint {
		return u, ErrEmailInUse
	}
	return u, err
}
