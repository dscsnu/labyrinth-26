package auth

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"labyrinth/internal/platform/postgres"
)

// db is the auth use case's SQL against the auth_code table. Codes are stored
// only as their SHA-256 hash.
type db struct {
	pool *pgxpool.Pool
}

// insertCode stores the hash of a new login code for userID, valid for ttl.
// Expiry is computed by Postgres (now() + ttl), the same clock consumeCode
// checks it against, so clock skew can't stretch or shrink it.
func (d db) insertCode(ctx context.Context, code string, userID uuid.UUID, ttl time.Duration) error {
	hash := sha256.Sum256([]byte(code))
	_, err := d.pool.Exec(ctx,
		"INSERT INTO auth_code (code_hash, user_id, expires_at) VALUES ($1, $2, now() + $3::interval)",
		hash[:], userID, ttl)
	return err
}

// consumeCode uses up an unexpired code. Named for the operation rather than
// its statement, it runs in one transaction:
//  1. DELETE FROM auth_code … RETURNING user_id (unexpired codes only)
//  2. use(userID), which may run its own reads (loading the user)
//
// The deletion commits only if use succeeds, so a failure after the lookup
// (loading the user, signing the token) leaves the code redeemable. A
// concurrent redemption of the same code waits on the row lock and then finds
// it gone. postgres.ErrNotFound if it doesn't exist or expired.
func (d db) consumeCode(ctx context.Context, code string, use func(uuid.UUID) error) error {
	hash := sha256.Sum256([]byte(code))
	return pgx.BeginFunc(ctx, d.pool, func(tx pgx.Tx) error {
		var userID uuid.UUID
		err := tx.QueryRow(ctx,
			"DELETE FROM auth_code WHERE code_hash = $1 AND expires_at > now() RETURNING user_id",
			hash[:]).Scan(&userID)
		if errors.Is(err, pgx.ErrNoRows) {
			return postgres.ErrNotFound
		}
		if err != nil {
			return err
		}
		return use(userID)
	})
}

// deleteExpiredCodes removes expired codes and returns how many it removed.
func (d db) deleteExpiredCodes(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, "DELETE FROM auth_code WHERE expires_at <= now()")
	return tag.RowsAffected(), err
}
