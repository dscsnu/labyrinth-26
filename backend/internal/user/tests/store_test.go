package tests

// Tests against a real Postgres (TEST_DATABASE_URL), in schema user_test.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/google/uuid"

	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/user"
)

// open returns the user store on a fresh schema.
func open(t *testing.T) (*user.Users, *postgres.DB) {
	t.Helper()
	db := pgtest.Open(t, pgtest.URL(t, "user_test"))
	return user.New(db), db
}

// TestSignInCreatesThenRefreshes checks first sign-in creates a PLAYER, and a
// later one with the same subject updates name and email but keeps the ID and
// role.
func TestSignInCreatesThenRefreshes(t *testing.T) {
	users, db := open(t)
	ctx := context.Background()
	first := pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s", Email: "a@snu.edu.in", Name: "A"}))
	if first.Role != "PLAYER" || first.AvatarURL != nil {
		t.Fatalf("first sign-in: %+v", first)
	}
	if _, err := db.Pool().Exec(ctx, "UPDATE userprofile SET role = 'ADMIN'"); err != nil {
		t.Fatal(err)
	}
	again := pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s", Email: "b@snu.edu.in", Name: "B"}))
	if again.ID != first.ID || again.Email != "b@snu.edu.in" || again.Name != "B" || again.Role != "ADMIN" {
		t.Fatalf("second sign-in: %+v", again)
	}
}

// TestSignInWithoutPictureKeepsAvatar checks an empty picture doesn't erase
// the stored avatar, and a new one replaces it.
func TestSignInWithoutPictureKeepsAvatar(t *testing.T) {
	users, _ := open(t)
	ctx := context.Background()
	p := user.GoogleProfile{Subject: "s", Email: "a@snu.edu.in", Name: "A", AvatarURL: "https://x/1.png"}
	pgtest.Must(users.SignInWithGoogle(ctx, p))
	p.AvatarURL = ""
	if u := pgtest.Must(users.SignInWithGoogle(ctx, p)); u.AvatarURL == nil || *u.AvatarURL != "https://x/1.png" {
		t.Fatalf("avatar after empty picture: %v", u.AvatarURL)
	}
	p.AvatarURL = "https://x/2.png"
	if u := pgtest.Must(users.SignInWithGoogle(ctx, p)); *u.AvatarURL != "https://x/2.png" {
		t.Fatalf("avatar after new picture: %v", *u.AvatarURL)
	}
}

// TestEmailInUse checks that a second Google account with an email already
// taken gets user.ErrEmailInUse, for a new user and for an email change.
func TestEmailInUse(t *testing.T) {
	users, _ := open(t)
	ctx := context.Background()
	pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s1", Email: "a@snu.edu.in", Name: "A"}))
	pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s2", Email: "b@snu.edu.in", Name: "B"}))
	if _, err := users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s3", Email: "a@snu.edu.in", Name: "C"}); !errors.Is(err, user.ErrEmailInUse) {
		t.Fatalf("new user with a taken email: %v", err)
	}
	if _, err := users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s2", Email: "a@snu.edu.in", Name: "B"}); !errors.Is(err, user.ErrEmailInUse) {
		t.Fatalf("existing user changing to a taken email: %v", err)
	}
}

// TestGetUnknownUser checks Get maps a missing user to user.ErrUserNotFound, which
// still matches postgres.ErrNotFound.
func TestGetUnknownUser(t *testing.T) {
	users, _ := open(t)
	_, err := users.Get(context.Background(), uuid.New())
	if !errors.Is(err, user.ErrUserNotFound) || !errors.Is(err, postgres.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// TestConcurrentFirstSignIns checks that one Google account signing in from
// several places at once gets one user.
func TestConcurrentFirstSignIns(t *testing.T) {
	users, db := open(t)
	const tries = 8
	ids := make([]uuid.UUID, tries)
	var wg sync.WaitGroup
	for i := range tries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			u, err := users.SignInWithGoogle(context.Background(), user.GoogleProfile{Subject: "s", Email: "a@snu.edu.in", Name: fmt.Sprint(i)})
			if err != nil {
				t.Error(err)
			}
			ids[i] = u.ID
		}()
	}
	wg.Wait()
	var n int
	if err := db.Pool().QueryRow(context.Background(), "SELECT count(*) FROM userprofile").Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d users (%v), want 1", n, err)
	}
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("different IDs: %v", ids)
		}
	}
}
