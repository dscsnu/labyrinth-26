package tests

// Login code tests against a real Postgres (TEST_DATABASE_URL), in schema
// auth_test.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/auth"
	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/user"
)

// codesFixture returns a login code store with config.toml's TTL, a user and
// one fresh code for them.
func codesFixture(t *testing.T) (*auth.LoginCodes, *postgres.DB, uuid.UUID, string) {
	t.Helper()
	ctx := context.Background()
	db := pgtest.Open(t, pgtest.URL(t, "auth_test"))
	u := pgtest.Must(user.New(db).SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s", Email: "a@snu.edu.in", Name: "A"}))
	codes := auth.NewLoginCodes(db, configtest.Settings(t).Auth.LoginCodeTTL.Duration)
	return codes, db, u.ID, pgtest.Must(codes.Create(ctx, u.ID))
}

// TestFailedRedemptionKeepsCode checks that when use fails, the code isn't
// used up, and the next redemption succeeds and uses it up.
func TestFailedRedemptionKeepsCode(t *testing.T) {
	codes, _, userID, code := codesFixture(t)
	ctx := context.Background()
	boom := errors.New("boom")

	if err := codes.Redeem(ctx, code, func(uuid.UUID) error { return boom }); !errors.Is(err, boom) {
		t.Fatalf("failing use: %v", err)
	}
	var got uuid.UUID
	if err := codes.Redeem(ctx, code, func(id uuid.UUID) error { got = id; return nil }); err != nil || got != userID {
		t.Fatalf("retry: %v, user %v", err, got)
	}
	if err := codes.Redeem(ctx, code, func(uuid.UUID) error { return nil }); !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("third redemption: %v, want ErrNotFound", err)
	}
}

// TestConcurrentRedemptionOnce checks that one code redeemed by many requests
// at once succeeds exactly once.
func TestConcurrentRedemptionOnce(t *testing.T) {
	codes, _, _, code := codesFixture(t)
	const tries = 8
	errs := make([]error, tries)
	var wg sync.WaitGroup
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = codes.Redeem(context.Background(), code, func(uuid.UUID) error { return nil })
		}()
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, postgres.ErrNotFound):
			t.Fatalf("unexpected %v", err)
		}
	}
	if ok != 1 {
		t.Fatalf("%d redemptions succeeded, want 1", ok)
	}
}

// TestCodeExpiryUsesDatabaseClock checks that a code's expiry is Postgres's
// now() plus auth.login_code_ttl.
func TestCodeExpiryUsesDatabaseClock(t *testing.T) {
	_, db, _, _ := codesFixture(t)
	ttl := configtest.Settings(t).Auth.LoginCodeTTL.Duration
	var left time.Duration
	if err := db.Pool().QueryRow(context.Background(),
		"SELECT expires_at - now() FROM auth_code").Scan(&left); err != nil {
		t.Fatal(err)
	}
	if left <= ttl-5*time.Second || left > ttl {
		t.Fatalf("code expires in %v, want just under %v", left, ttl)
	}
}
