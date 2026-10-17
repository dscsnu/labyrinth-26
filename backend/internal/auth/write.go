package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/user"
)

var (
	// ErrInvalidCode means a login code is unknown, expired or already used.
	// Redeem returns it wrapping postgres.ErrNotFound.
	ErrInvalidCode = errors.New("invalid or expired code")
	// errUserGone means a login code's user was deleted before it was
	// redeemed. POST /auth/token's redemption returns it from inside Redeem,
	// so it isn't confused with ErrInvalidCode.
	errUserGone = errors.New("the code's user no longer exists")
)

// IssueAccess signs an access token for u, valid for the configured TTL.
// The returned expiry is whole seconds, exactly the token's exp claim.
func (t *Tokens) IssueAccess(u user.User) (string, time.Time, error) {
	now := time.Now().Truncate(time.Second)
	exp := now.Add(t.ttl)
	token := jwt.NewWithClaims(signingMethod, AccessClaims{
		Email:     u.Email,
		Name:      u.Name,
		AvatarURL: u.AvatarURL,
		Role:      u.Role,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer:    issuer,
			Audience:  jwt.ClaimStrings{accessAudience},
			Subject:   u.ID.String(),
			IssuedAt:  jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(exp),
		},
	})
	signed, err := token.SignedString(t.key)
	return signed, exp, err
}

// issueState signs an OAuth state cookie valid for ttl.
func (t *Tokens) issueState(s oauthState, ttl time.Duration) (string, error) {
	now := time.Now()
	s.RegisteredClaims = jwt.RegisteredClaims{
		Issuer:    issuer,
		Audience:  jwt.ClaimStrings{stateAudience},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	return jwt.NewWithClaims(signingMethod, s).SignedString(t.key)
}

// LoginCodes are single-use codes that hand a completed Google sign-in from
// the backend to the SvelteKit server, which redeems one for a JWT. They live
// only in Postgres.
type LoginCodes struct {
	db db
	// ttl is how long the SvelteKit server has to redeem a code
	// (config.toml auth.login_code_ttl).
	ttl time.Duration
}

// NewLoginCodes returns the login code store backed by pg, issuing codes
// valid for ttl.
func NewLoginCodes(pg *postgres.DB, ttl time.Duration) *LoginCodes {
	return &LoginCodes{db: db{pool: pg.Pool()}, ttl: ttl}
}

// Create makes a new code for userID.
func (c *LoginCodes) Create(ctx context.Context, userID uuid.UUID) (string, error) {
	code, err := randomToken()
	if err != nil {
		return "", err
	}
	return code, c.db.insertCode(ctx, code, userID, c.ttl)
}

// Redeem uses up a code, calling use with its user. The code is used up only
// if use returns nil; otherwise it stays redeemable and use's error is
// returned. ErrInvalidCode, wrapping postgres.ErrNotFound, if the code is
// unknown, expired or already used.
func (c *LoginCodes) Redeem(ctx context.Context, code string, use func(userID uuid.UUID) error) error {
	err := c.db.consumeCode(ctx, code, use)
	if errors.Is(err, postgres.ErrNotFound) {
		return fmt.Errorf("%w: %w", ErrInvalidCode, err)
	}
	return err
}

// DeleteExpired removes codes that were never redeemed.
func (c *LoginCodes) DeleteExpired(ctx context.Context) (int64, error) {
	return c.db.deleteExpiredCodes(ctx)
}

// tokenBytes is the size of random OAuth states and login codes: 256 bits,
// so they can't be guessed. A security parameter, not a tunable.
const tokenBytes = 32

// randomToken returns tokenBytes random bytes, base64url-encoded.
func randomToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
