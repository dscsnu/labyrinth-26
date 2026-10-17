package auth

import (
	"crypto/ed25519"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"labyrinth/internal/user"
)

const (
	// issuer is the iss claim on every token this backend signs.
	issuer = "labyrinth-backend"
	// accessAudience is the aud claim of access tokens.
	accessAudience = "labyrinth-api"
	// stateAudience is the aud claim of OAuth state cookies, so neither kind of
	// token is accepted as the other.
	stateAudience = "labyrinth-oauth-state"
)

// signingMethod is the only algorithm tokens are signed or accepted with.
var signingMethod = jwt.SigningMethodEdDSA

// Tokens signs (write.go) and verifies (read.go) JWTs with Ed25519. The
// SvelteKit server holds only the public key, so it can verify but never mint.
type Tokens struct {
	key ed25519.PrivateKey
	pub ed25519.PublicKey
	ttl time.Duration
}

// NewTokens returns Tokens that sign with key and issue access tokens valid for
// ttl.
func NewTokens(key ed25519.PrivateKey, ttl time.Duration) *Tokens {
	return &Tokens{key: key, pub: key.Public().(ed25519.PublicKey), ttl: ttl}
}

// AccessClaims are an access token's contents: a snapshot from sign-in, so a
// role change applies once the user signs in again.
type AccessClaims struct {
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	AvatarURL *string   `json:"avatar_url"`
	Role      user.Role `json:"role"`
	jwt.RegisteredClaims
}

// UserID parses the token's subject as a user ID.
func (c AccessClaims) UserID() (uuid.UUID, error) {
	return uuid.Parse(c.Subject)
}

// VerifyAccess checks an access token's signature, algorithm, issuer,
// audience and expiry, and returns its claims.
func (t *Tokens) VerifyAccess(raw string) (AccessClaims, error) {
	var claims AccessClaims
	_, err := jwt.ParseWithClaims(raw, &claims, t.keyFunc,
		jwt.WithValidMethods([]string{signingMethod.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(accessAudience),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return AccessClaims{}, err
	}
	if _, err := claims.UserID(); err != nil {
		return AccessClaims{}, errors.New("token subject is not a user id")
	}
	return claims, nil
}

// oauthState travels in a short-lived cookie between /auth/google/login and
// /auth/google/callback, signed so it can't be forged or edited.
type oauthState struct {
	State    string `json:"state"`
	Verifier string `json:"verifier"`
	Next     string `json:"next"`
	jwt.RegisteredClaims
}

// verifyState checks an OAuth state cookie the same way VerifyAccess checks
// an access token, and returns its contents.
func (t *Tokens) verifyState(raw string) (oauthState, error) {
	var s oauthState
	_, err := jwt.ParseWithClaims(raw, &s, t.keyFunc,
		jwt.WithValidMethods([]string{signingMethod.Alg()}),
		jwt.WithIssuer(issuer),
		jwt.WithAudience(stateAudience),
		jwt.WithExpirationRequired(),
	)
	return s, err
}

// keyFunc gives the JWT parser the public key to verify with.
func (t *Tokens) keyFunc(*jwt.Token) (any, error) {
	return t.pub, nil
}
