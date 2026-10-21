package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/user"
)

// TestSetRole checks SetRole: each valid role is stored and returned (Get
// sees it at once), an unknown role is invalid_role with nothing changed,
// and an unknown user is user_not_found. A later sign-in keeps the role.
func TestSetRole(t *testing.T) {
	users, _ := open(t)
	ctx := context.Background()
	u := pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s", Email: "a@snu.edu.in", Name: "A"}))
	for _, role := range []user.Role{user.RoleHelper, user.RoleAdmin, user.RolePlayer, user.RoleAdmin} {
		if got := pgtest.Must(users.SetRole(ctx, u.ID, role)); got.Role != role || got.ID != u.ID {
			t.Fatalf("set %s: %+v", role, got)
		}
		if got := pgtest.Must(users.Get(ctx, u.ID)); got.Role != role {
			t.Fatalf("Get after setting %s: %s", role, got.Role)
		}
	}
	if _, err := users.SetRole(ctx, u.ID, "OWNER"); !errors.Is(err, user.ErrInvalidRole) {
		t.Fatalf("unknown role: %v", err)
	}
	if got := pgtest.Must(users.Get(ctx, u.ID)); got.Role != user.RoleAdmin {
		t.Fatalf("a refused role changed it: %s", got.Role)
	}
	if _, err := users.SetRole(ctx, uuid.New(), user.RoleHelper); !errors.Is(err, user.ErrUserNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
	if again := pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: "s", Email: "a@snu.edu.in", Name: "A"})); again.Role != user.RoleAdmin {
		t.Fatalf("a sign-in reset the role: %s", again.Role)
	}
}

// TestSearch checks Search: emails containing the query, case insensitive,
// by email, at most limit of them; an empty query matches everyone; % and _
// in the query are matched literally, not as wildcards.
func TestSearch(t *testing.T) {
	users, _ := open(t)
	ctx := context.Background()
	for i, email := range []string{"zed@snu.edu.in", "amy@snu.edu.in", "bob@gmail.com", "a_b@snu.edu.in", "50%off@snu.edu.in"} {
		pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: string(rune('a' + i)), Email: email, Name: email}))
	}
	emails := func(q string, limit int) []string {
		var out []string
		for _, u := range pgtest.Must(users.Search(ctx, q, limit)) {
			out = append(out, u.Email)
		}
		return out
	}
	cases := []struct {
		q     string
		limit int
		want  []string
	}{
		{"SNU", 10, []string{"50%off@snu.edu.in", "a_b@snu.edu.in", "amy@snu.edu.in", "zed@snu.edu.in"}},
		{"snu", 2, []string{"50%off@snu.edu.in", "a_b@snu.edu.in"}},
		{"", 10, []string{"50%off@snu.edu.in", "a_b@snu.edu.in", "amy@snu.edu.in", "bob@gmail.com", "zed@snu.edu.in"}},
		{"_", 10, []string{"a_b@snu.edu.in"}},    // not "any one character"
		{"%", 10, []string{"50%off@snu.edu.in"}}, // not "anything"
		{"nobody", 10, nil},
	}
	for _, c := range cases {
		got := emails(c.q, c.limit)
		if len(got) != len(c.want) {
			t.Fatalf("search %q (limit %d): %v, want %v", c.q, c.limit, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("search %q (limit %d): %v, want %v", c.q, c.limit, got, c.want)
			}
		}
	}
}
