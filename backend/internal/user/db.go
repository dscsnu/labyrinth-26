package user

import (
	"context"
	"errors"
	"strings"

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
const userColumns = "id, email, name, avatar_url, role, waypoint_id, created_at"

// scanUser reads one userprofile row, mapping no rows to
// postgres.ErrNotFound.
func scanUser(row pgx.Row) (User, error) {
	var u User
	err := row.Scan(&u.ID, &u.Email, &u.Name, &u.AvatarURL, &u.Role, &u.WaypointID, &u.CreatedAt)
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

// updateRole sets a user's role and returns the row: UPDATE … RETURNING,
// one statement that also clears waypoint_id when the role is PLAYER (the
// userprofile_waypoint_staff CHECK, migration 0016, requires it).
// postgres.ErrNotFound if there's no such user.
func (d db) updateRole(ctx context.Context, id uuid.UUID, role Role) (User, error) {
	return scanUser(d.pool.QueryRow(ctx, `
		UPDATE userprofile SET role = $2,
			waypoint_id = CASE WHEN $2 = 'PLAYER' THEN NULL ELSE waypoint_id END
		WHERE id = $1 RETURNING `+userColumns, id, role))
}

// updateWaypoint sets a HELPER's waypoint_id and returns the row: UPDATE …
// WHERE role = 'HELPER' RETURNING. postgres.ErrNotFound if no helper has
// that ID (no such user, or not a helper).
func (d db) updateWaypoint(ctx context.Context, id uuid.UUID, waypointID *string) (User, error) {
	return scanUser(d.pool.QueryRow(ctx,
		"UPDATE userprofile SET waypoint_id = $2 WHERE id = $1 AND role = 'HELPER' RETURNING "+userColumns, id, waypointID))
}

// selectByEmail reads the user whose email is email, ignoring case.
// postgres.ErrNotFound if there's none.
func (d db) selectByEmail(ctx context.Context, email string) (User, error) {
	return scanUser(d.pool.QueryRow(ctx, "SELECT "+userColumns+" FROM userprofile WHERE lower(email) = lower($1)", email))
}

// selectHelpers returns every HELPER, ordered by email.
func (d db) selectHelpers(ctx context.Context) ([]User, error) {
	rows, err := d.pool.Query(ctx, "SELECT "+userColumns+" FROM userprofile WHERE role = 'HELPER' ORDER BY email")
	if err != nil {
		return nil, err
	}
	return collectUsers(rows)
}

// collectUsers reads every row of rows (closing it), as users.
func collectUsers(rows pgx.Rows) ([]User, error) {
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// selectByEmailLike returns up to limit users whose email contains query,
// case insensitive (the % and _ in query are matched literally), by email.
func (d db) selectByEmailLike(ctx context.Context, query string, limit int) ([]User, error) {
	escaped := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(query)
	rows, err := d.pool.Query(ctx,
		"SELECT "+userColumns+" FROM userprofile WHERE email ILIKE '%' || $1 || '%' ORDER BY email LIMIT $2", escaped, limit)
	if err != nil {
		return nil, err
	}
	return collectUsers(rows)
}
