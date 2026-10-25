package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/user"
)

// addWaypoints inserts waypoints with these IDs, the checkpoints helpers can
// be assigned (the schema starts with none).
func addWaypoints(t *testing.T, db *postgres.DB, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := db.Pool().Exec(context.Background(),
			"INSERT INTO waypoint (id, name, latitude, longitude) VALUES ($1, $1, 0, 0)", id); err != nil {
			t.Fatal(err)
		}
	}
}

// signIn creates a user with this email and sets their role.
func signIn(t *testing.T, users *user.Users, email string, role user.Role) user.User {
	t.Helper()
	ctx := context.Background()
	u := pgtest.Must(users.SignInWithGoogle(ctx, user.GoogleProfile{Subject: email, Email: email, Name: email}))
	if role != user.RolePlayer {
		u = pgtest.Must(users.SetRole(ctx, u.ID, role))
	}
	return u
}

// TestByEmail checks the exact, case-insensitive lookup: any case finds the
// user, a substring doesn't, and an unknown email is ErrUserNotFound
// wrapping postgres.ErrNotFound.
func TestByEmail(t *testing.T) {
	users, _ := open(t)
	ctx := context.Background()
	u := signIn(t, users, "Asha@snu.edu.in", user.RolePlayer)
	for _, email := range []string{"Asha@snu.edu.in", "asha@SNU.EDU.IN"} {
		if got, err := users.ByEmail(ctx, email); err != nil || got.ID != u.ID {
			t.Fatalf("%s: %+v %v", email, got, err)
		}
	}
	for _, email := range []string{"asha", "nobody@snu.edu.in"} {
		if _, err := users.ByEmail(ctx, email); !errors.Is(err, user.ErrUserNotFound) || !errors.Is(err, postgres.ErrNotFound) {
			t.Fatalf("%s: %v", email, err)
		}
	}
}

// TestAssign checks a helper's checkpoint: set, moved and cleared (nil);
// Helpers lists only HELPERs, by email, with their checkpoints; a player and
// an admin are ErrNotHelper and an unknown user ErrUserNotFound, with nothing
// changed.
func TestAssign(t *testing.T) {
	users, db := open(t)
	ctx := context.Background()
	addWaypoints(t, db, "2001", "2002")
	b := signIn(t, users, "b@snu.edu.in", user.RoleHelper)
	a := signIn(t, users, "a@snu.edu.in", user.RoleHelper)
	p := signIn(t, users, "p@snu.edu.in", user.RolePlayer)
	admin := signIn(t, users, "admin@snu.edu.in", user.RoleAdmin)
	w1, w2 := "2001", "2002"

	if u := pgtest.Must(users.Assign(ctx, b.ID, &w1)); u.WaypointID == nil || *u.WaypointID != "2001" {
		t.Fatalf("assigned: %+v", u)
	}
	if u := pgtest.Must(users.Assign(ctx, b.ID, &w2)); *u.WaypointID != "2002" {
		t.Fatalf("moved: %+v", u)
	}
	pgtest.Must(users.Assign(ctx, a.ID, &w2)) // two helpers may share a checkpoint
	hs := pgtest.Must(users.Helpers(ctx))
	if len(hs) != 2 || hs[0].ID != a.ID || hs[1].ID != b.ID || *hs[0].WaypointID != "2002" || *hs[1].WaypointID != "2002" {
		t.Fatalf("helpers: %+v", hs)
	}
	if u := pgtest.Must(users.Assign(ctx, b.ID, nil)); u.WaypointID != nil {
		t.Fatalf("cleared: %+v", u)
	}

	for _, id := range []uuid.UUID{p.ID, admin.ID} {
		if _, err := users.Assign(ctx, id, &w1); !errors.Is(err, user.ErrNotHelper) {
			t.Fatalf("not a helper: %v", err)
		}
		if u := pgtest.Must(users.Get(ctx, id)); u.WaypointID != nil {
			t.Fatalf("a non-helper got a checkpoint: %+v", u)
		}
	}
	if _, err := users.Assign(ctx, uuid.New(), &w1); !errors.Is(err, user.ErrUserNotFound) {
		t.Fatalf("unknown user: %v", err)
	}
}

// TestDemotingClearsCheckpoint checks SetRole to PLAYER clears a helper's
// checkpoint in the same write, and promoting to ADMIN keeps it.
func TestDemotingClearsCheckpoint(t *testing.T) {
	users, db := open(t)
	ctx := context.Background()
	addWaypoints(t, db, "2001")
	h := signIn(t, users, "h@snu.edu.in", user.RoleHelper)
	w := "2001"
	pgtest.Must(users.Assign(ctx, h.ID, &w))
	if u := pgtest.Must(users.SetRole(ctx, h.ID, user.RoleAdmin)); u.WaypointID == nil {
		t.Fatalf("promoted to admin: %+v", u)
	}
	if u := pgtest.Must(users.SetRole(ctx, h.ID, user.RolePlayer)); u.WaypointID != nil || u.Role != user.RolePlayer {
		t.Fatalf("demoted to player: %+v", u)
	}
}

// TestCheckpointConstraints checks the database backstops (migration 0016):
// a PLAYER can't hold a checkpoint (userprofile_waypoint_staff), an unknown
// waypoint is refused (the foreign key), and deleting a waypoint unassigns
// its helpers.
func TestCheckpointConstraints(t *testing.T) {
	users, db := open(t)
	ctx := context.Background()
	addWaypoints(t, db, "2001")
	p := signIn(t, users, "p@snu.edu.in", user.RolePlayer)
	h := signIn(t, users, "h@snu.edu.in", user.RoleHelper)
	if _, err := db.Pool().Exec(ctx, "UPDATE userprofile SET waypoint_id = '2001' WHERE id = $1", p.ID); err == nil {
		t.Fatal("a player was given a checkpoint")
	}
	unknown := "2999"
	if _, err := users.Assign(ctx, h.ID, &unknown); err == nil {
		t.Fatal("assigned an unknown waypoint")
	}
	w := "2001"
	pgtest.Must(users.Assign(ctx, h.ID, &w))
	if _, err := db.Pool().Exec(ctx, "DELETE FROM waypoint WHERE id = '2001'"); err != nil {
		t.Fatal(err)
	}
	if u := pgtest.Must(users.Get(ctx, h.ID)); u.WaypointID != nil {
		t.Fatalf("checkpoint after its waypoint was deleted: %+v", u)
	}
}
