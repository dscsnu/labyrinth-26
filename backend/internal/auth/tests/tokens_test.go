package tests

import (
	"crypto/ed25519"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	"labyrinth/internal/auth"
	"labyrinth/internal/user"
)

// newKey returns a fresh Ed25519 key.
func newKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, key, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// testUser returns a player with a random ID.
func testUser() user.User {
	return user.User{ID: uuid.New(), Email: "a@snu.edu.in", Name: "A", Role: user.RolePlayer}
}

// TestSafeNext checks that only same-site paths survive as redirect targets.
func TestSafeNext(t *testing.T) {
	cases := map[string]string{
		"":                    "/",
		"/":                   "/",
		"/team":               "/team",
		"/team?tab=1#x":       "/team?tab=1#x",
		"//evil.com":          "/",
		"//evil.com/path":     "/",
		"/\\evil.com":         "/",
		"https://evil.com":    "/",
		"evil.com":            "/",
		"javascript:alert(1)": "/",
		"/ok\r\nSet-Cookie:":  "/",
	}
	for in, want := range cases {
		if got := auth.SafeNext(in); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestAccessTokenRoundTrip checks that an issued token verifies with the same
// claims.
func TestAccessTokenRoundTrip(t *testing.T) {
	tok := auth.NewTokens(newKey(t), time.Hour)
	u := testUser()

	raw, exp, err := tok.IssueAccess(u)
	if err != nil {
		t.Fatal(err)
	}
	if time.Until(exp) < 59*time.Minute {
		t.Fatalf("expiry too soon: %v", exp)
	}

	claims, err := tok.VerifyAccess(raw)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := claims.UserID()
	if id != u.ID || claims.Email != u.Email || claims.Role != u.Role {
		t.Fatalf("claims mismatch: %+v", claims)
	}
}

// TestAccessTokenRejections checks that forged, expired and unsigned tokens
// are rejected. State and access tokens being refused in each other's place is
// tested end to end in internal/server/tests
// (TestStateAndAccessTokensNotInterchangeable).
func TestAccessTokenRejections(t *testing.T) {
	key := newKey(t)
	tok := auth.NewTokens(key, time.Hour)
	u := testUser()
	good, _, _ := tok.IssueAccess(u)

	t.Run("wrong key", func(t *testing.T) {
		other := auth.NewTokens(newKey(t), time.Hour)
		if _, err := other.VerifyAccess(good); err == nil {
			t.Fatal("accepted token signed with another key")
		}
	})

	t.Run("expired", func(t *testing.T) {
		expired, _, _ := auth.NewTokens(key, -time.Minute).IssueAccess(u)
		if _, err := tok.VerifyAccess(expired); err == nil {
			t.Fatal("accepted expired token")
		}
	})

	t.Run("alg none", func(t *testing.T) {
		unsigned := jwt.NewWithClaims(jwt.SigningMethodNone, auth.AccessClaims{
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: auth.Issuer, Audience: jwt.ClaimStrings{auth.AccessAudience},
				Subject: u.ID.String(), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		})
		raw, _ := unsigned.SignedString(jwt.UnsafeAllowNoneSignatureType)
		if _, err := tok.VerifyAccess(raw); err == nil {
			t.Fatal("accepted alg=none token")
		}
	})

	t.Run("HS256 with public key as secret", func(t *testing.T) {
		forged := jwt.NewWithClaims(jwt.SigningMethodHS256, auth.AccessClaims{
			Role: user.RoleAdmin,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer: auth.Issuer, Audience: jwt.ClaimStrings{auth.AccessAudience},
				Subject: u.ID.String(), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
			},
		})
		raw, _ := forged.SignedString([]byte(key.Public().(ed25519.PublicKey)))
		if _, err := tok.VerifyAccess(raw); err == nil {
			t.Fatal("accepted HS256 token keyed with the public key")
		}
	})
}
