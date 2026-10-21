package tests

import (
	"context"
	"errors"
	"strings"
	"testing"

	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/teams"
)

// TestFinish checks Finish: a lobby team is left alone (nothing to finish),
// a started one becomes FINISHED (published as finished, stored, so a
// restart keeps it), a repeat changes nothing, an unknown team is
// team_not_found, and a FINISHED team's members still can't join another.
func TestFinish(t *testing.T) {
	db := pgtest.Open(t, pgtest.URL(t, "teams_test"))
	ctx := context.Background()
	s, hub := loadWithHub(t, db)
	users := newUsers(t, db, 2)
	team := must(s.Create(ctx, users[0], "Finishers"))
	noErr(t, s.Finish(ctx, team.ID))
	if got, _ := s.ByID(team.ID); got.Status != teams.Lobby {
		t.Fatalf("finishing a lobby team: %s", got.Status)
	}
	must(s.Start(ctx, users[0]))
	sub := hub.Subscribe(users[0])
	defer sub.Close()

	noErr(t, s.Finish(ctx, team.ID))
	got, _ := s.ByID(team.ID)
	ev := <-sub.C
	if got.Status != teams.Finished || ev.Type() != teams.TeamFinished || !strings.Contains(string(ev.Data()), `"FINISHED"`) {
		t.Fatalf("after Finish: %s, event %s %s", got.Status, ev.Type(), ev.Data())
	}
	seq := got.Version.Seq
	noErr(t, s.Finish(ctx, team.ID))
	if again, _ := s.ByID(team.ID); again.Version.Seq != seq || len(sub.C) != 0 {
		t.Fatalf("a repeat Finish changed the team: seq %d → %d", seq, again.Version.Seq)
	}
	if err := s.Finish(ctx, "000000"); !errors.Is(err, teams.ErrTeamNotFound) {
		t.Fatalf("unknown team: %v", err)
	}
	if _, err := s.Create(ctx, users[0], "Again"); !errors.Is(err, teams.ErrAlreadyInTeam) {
		t.Fatalf("a finished team's member creating a team: %v", err)
	}
	if restarted, _ := load(t, db).ByID(team.ID); restarted.Status != teams.Finished {
		t.Fatalf("after a restart: %s", restarted.Status)
	}
}

// TestDelete checks Delete: every member gets no_team (team_deleted) and is
// teamless, the team and its rows are gone from Postgres (a restart doesn't
// bring it back), OnDelete is called once with the team's ID, a member can
// create a team at once, and an unknown team is team_not_found.
func TestDelete(t *testing.T) {
	db := pgtest.Open(t, pgtest.URL(t, "teams_test"))
	ctx := context.Background()
	set := configtest.Settings(t)
	if set.Teams.Size < 2 {
		t.Skip("needs teams.size >= 2")
	}
	hub := live.NewHub(set.Live.HubShards, set.Live.SubscriberBuffer)
	var deleted []string
	s := pgtest.Must(teams.Load(ctx, db, hub, pgtest.Quiet, teams.Rules{
		Size: set.Teams.Size, NameMaxLength: set.Teams.NameMaxLength,
		WriteTimeout: set.Storage.WriteTimeout.Duration, CodeAttempts: set.Teams.CodeAttempts,
		Epoch: epochs.Add(1), OnDelete: func(id string) { deleted = append(deleted, id) },
	}))
	users := newUsers(t, db, 2)
	team := must(s.Create(ctx, users[0], "Doomed"))
	must(s.Join(ctx, users[1], team.ID))
	subs := []*live.Subscription{hub.Subscribe(users[0]), hub.Subscribe(users[1])}
	for _, sub := range subs {
		defer sub.Close()
	}

	noErr(t, s.Delete(ctx, team.ID))
	for i, sub := range subs {
		if ev := <-sub.C; ev.Name() != "no_team" || ev.Type() != teams.NoTeamDeleted {
			t.Fatalf("member %d: %s %s, want no_team team_deleted", i, ev.Name(), ev.Type())
		}
		if _, err := s.ForUser(users[i]); !errors.Is(err, teams.ErrNotInTeam) {
			t.Fatalf("member %d still in a team: %v", i, err)
		}
	}
	if _, ok := s.ByID(team.ID); ok || len(deleted) != 1 || deleted[0] != team.ID {
		t.Fatalf("after Delete: still there %v, OnDelete calls %v", ok, deleted)
	}
	if _, ok := load(t, db).ByID(team.ID); ok {
		t.Fatal("a restart brought the deleted team back")
	}
	must(s.Create(ctx, users[1], "Phoenix"))
	if err := s.Delete(ctx, team.ID); !errors.Is(err, teams.ErrTeamNotFound) {
		t.Fatalf("deleting it again: %v", err)
	}
}

// noErr fails the test on err.
func noErr(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
