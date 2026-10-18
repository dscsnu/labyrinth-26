package tests

// teams.Store-level tests against a real Postgres (TEST_DATABASE_URL), in schema
// teams_test. HTTP behaviour is covered end to end in internal/server.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teams"
	"labyrinth/internal/user"
)

// must returns a teams.Team or panics on error, failing the test.
var must = pgtest.Must[teams.Team]

// minRestartTeam is the smallest teams.size TestSurvivesRestart works with:
// its lobby team has a leader and two joiners. With a smaller size in
// config.toml it skips rather than fails.
const minRestartTeam = 3

// sinkMembers is where BenchmarkForUser stores what it read, so the compiler
// can't discard the reads.
var sinkMembers atomic.Int64

// newUsers signs in n test users and returns their IDs.
func newUsers(t testing.TB, db *postgres.DB, n int) []uuid.UUID {
	t.Helper()
	users := user.New(db)
	ids := make([]uuid.UUID, n)
	for i := range ids {
		u, err := users.SignInWithGoogle(context.Background(), user.GoogleProfile{
			Subject: fmt.Sprintf("s%d", i), Email: fmt.Sprintf("u%d@snu.edu.in", i), Name: fmt.Sprintf("U%d", i),
		})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = u.ID
	}
	return ids
}

// load builds a teams.Store from db with a fresh hub.
func load(t testing.TB, db *postgres.DB) *teams.Store {
	t.Helper()
	s, _ := loadWithHub(t, db)
	return s
}

// loadWithHub is load, also returning the hub the store publishes to.
func loadWithHub(t testing.TB, db *postgres.DB) (*teams.Store, *live.Hub) {
	t.Helper()
	set := configtest.Settings(t)
	hub := live.NewHub(set.Live.SubscriberBuffer)
	return pgtest.Must(teams.Load(context.Background(), db, hub, pgtest.Quiet, teams.Rules{
		Size:          set.Teams.Size,
		NameMaxLength: set.Teams.NameMaxLength,
		WriteTimeout:  set.Storage.WriteTimeout.Duration,
		CodeAttempts:  set.Teams.CodeAttempts,
	})), hub
}

// snapshot serialises everything s holds, for comparing across restarts:
// every team (All), and for every member, the team ForUser finds (the
// user → team index).
func snapshot(t *testing.T, s *teams.Store) string {
	t.Helper()
	all := s.All()
	userTeam := map[string]string{}
	for _, team := range all {
		for _, m := range team.Members {
			found, err := s.ForUser(m.ID)
			if err != nil {
				t.Fatalf("member %s of %s: %v", m.ID, team.ID, err)
			}
			userTeam[m.ID.String()] = found.ID
		}
	}
	b, err := json.Marshal(map[string]any{"teams": all, "userTeam": userTeam})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestSurvivesRestart checks that a started team, a lobby whose leader left,
// and a deleted team all reload from Postgres exactly as they were in memory.
// It uses teams.size + 4 users: a full team, three for the lobby, one for the
// deleted team.
func TestSurvivesRestart(t *testing.T) {
	dbURL := pgtest.URL(t, "teams_test")
	db := pgtest.Open(t, dbURL)
	ctx := context.Background()
	s := load(t, db)
	size := s.Size() // teams.size from config.toml
	if size < minRestartTeam {
		t.Skipf("needs teams.size >= %d", minRestartTeam)
	}
	users := newUsers(t, db, size+4)

	full := must(s.Create(ctx, users[0], "Full"))
	for _, u := range users[1:size] {
		must(s.Join(ctx, u, full.ID))
	}
	for _, u := range users[1:size] {
		must(s.SetReady(ctx, u, true))
	}
	must(s.Start(ctx, users[0]))

	leader, second, third, loner := users[size], users[size+1], users[size+2], users[size+3]
	lobby := must(s.Create(ctx, leader, "Lobby"))
	must(s.Join(ctx, second, lobby.ID))
	must(s.Join(ctx, third, lobby.ID))
	must(s.SetReady(ctx, third, true))
	if err := s.Leave(ctx, leader); err != nil { // leader leaves; second takes over
		t.Fatal(err)
	}

	must(s.Create(ctx, loner, "Gone"))
	if err := s.Leave(ctx, loner); err != nil { // last member out deletes the team
		t.Fatal(err)
	}

	before := snapshot(t, s)
	if after := snapshot(t, load(t, pgtest.Open(t, dbURL))); after != before {
		t.Fatalf("state changed across restart\nbefore: %s\nafter:  %s", before, after)
	}
}

// TestReadsDoNotTouchDatabase checks that reads keep working with the
// database closed, and that a failed write leaves memory unchanged.
func TestReadsDoNotTouchDatabase(t *testing.T) {
	db := pgtest.Open(t, pgtest.URL(t, "teams_test"))
	ctx := context.Background()
	users := newUsers(t, db, 2)
	s := load(t, db)
	team := must(s.Create(ctx, users[0], "T"))
	must(s.Join(ctx, users[1], team.ID))

	db.Close()

	got, err := s.ForUser(users[1])
	if err != nil || got.ID != team.ID || len(got.Members) != 2 {
		t.Fatalf("read after DB close: %+v, %v", got, err)
	}
	if _, err := s.SetReady(ctx, users[1], true); err == nil {
		t.Fatal("write with the database closed should fail")
	}
	if got, _ := s.ForUser(users[1]); got.Members[1].IsReady {
		t.Fatal("failed write must not change memory")
	}
}

// TestFailedWriteReconciles checks that when memory and database disagree
// (someone edited the database by hand), a failed write reloads the team from
// the database.
func TestFailedWriteReconciles(t *testing.T) {
	dbURL := pgtest.URL(t, "teams_test")
	db := pgtest.Open(t, dbURL)
	ctx := context.Background()
	users := newUsers(t, db, 3)
	s := load(t, db)
	team := must(s.Create(ctx, users[0], "T"))
	must(s.Join(ctx, users[1], team.ID))
	must(s.Join(ctx, users[2], team.ID))

	conn := pgtest.Must(pgx.Connect(ctx, dbURL))
	if _, err := conn.Exec(ctx, "DELETE FROM teammember WHERE user_id = $1", users[1]); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)

	if _, err := s.SetReady(ctx, users[1], true); err == nil {
		t.Fatal("write for a member deleted in the DB should fail")
	}
	got := must(s.ForUser(users[0]))
	if len(got.Members) != 2 {
		t.Fatalf("memory not reconciled: %d members, want 2", len(got.Members))
	}
	if _, err := s.ForUser(users[1]); !errors.Is(err, teams.ErrNotInTeam) {
		t.Fatalf("removed member still indexed: %v", err)
	}
	must(s.SetReady(ctx, users[2], true))
}

// BenchmarkForUser measures concurrent in-memory team reads: one team of
// teams.size members, read by GOMAXPROCS goroutines at once.
func BenchmarkForUser(b *testing.B) {
	db := pgtest.Open(b, pgtest.URL(b, "teams_test"))
	ctx := context.Background()
	size := configtest.Settings(b).Teams.Size
	users := newUsers(b, db, size)
	s := load(b, db)
	team := pgtest.Must(s.Create(ctx, users[0], "Bench"))
	for _, u := range users[1:] {
		pgtest.Must(s.Join(ctx, u, team.ID))
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i, members := 0, 0
		for pb.Next() {
			t, err := s.ForUser(users[i%size])
			if err != nil {
				b.Fatal(err)
			}
			members += len(t.Members)
			i++
		}
		sinkMembers.Add(int64(members))
	})
}

// TestRefreshProfile checks that a member's new name and avatar reach their
// team in memory and are published, that an unchanged profile publishes
// nothing, and that a user with no team is ignored.
func TestRefreshProfile(t *testing.T) {
	db := pgtest.Open(t, pgtest.URL(t, "teams_test"))
	ids := newUsers(t, db, 2)
	s, hub := loadWithHub(t, db)
	team := must(s.Create(context.Background(), ids[0], "Profiles"))
	sub := hub.Subscribe(ids[0])
	defer sub.Close()

	avatar := "https://x/new.png"
	s.RefreshProfile(ids[0], "Renamed", &avatar)
	got, _ := s.ByID(team.ID)
	if m := got.Members[0]; m.Name != "Renamed" || m.AvatarURL == nil || *m.AvatarURL != avatar {
		t.Fatalf("member after refresh: %+v", m)
	}
	if ev := <-sub.C; ev.Name != "team" {
		t.Fatalf("event %q", ev.Name)
	}

	same := avatar
	s.RefreshProfile(ids[0], "Renamed", &same)
	s.RefreshProfile(ids[1], "Nobody", nil) // not in a team
	select {
	case ev := <-sub.C:
		t.Fatalf("no-op refresh published %s", ev.Name)
	default:
	}
}
