package tests

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"labyrinth/internal/admin"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
	"labyrinth/internal/user"
)

// fakeTeams is an admin.Teams holding a fixed list of teams.
type fakeTeams struct{ list []teams.Team }

// All returns the teams.
func (f fakeTeams) All() []teams.Team { return f.list }

// ByID finds a team.
func (f fakeTeams) ByID(id string) (teams.Team, bool) {
	for _, t := range f.list {
		if t.ID == id {
			return t, true
		}
	}
	return teams.Team{}, false
}

// RemoveMember is unused here.
func (fakeTeams) RemoveMember(context.Context, string, uuid.UUID, *uuid.UUID) (teams.Team, error) {
	return teams.Team{}, nil
}

// Delete is unused here.
func (fakeTeams) Delete(context.Context, string) error { return nil }

// fakeState is an admin.TeamState answering TeamView from a map.
type fakeState map[string]teamstate.View

// TeamView returns the team's view.
func (f fakeState) TeamView(id string) teamstate.View { return f[id] }

// Revive is unused here.
func (fakeState) Revive(context.Context, string) (teamstate.View, error) {
	return teamstate.View{}, nil
}

// ResetCooldowns is unused here.
func (fakeState) ResetCooldowns(context.Context, string) (teamstate.View, error) {
	return teamstate.View{}, nil
}

// fakeUsers is an admin.Users recording what it was asked.
type fakeUsers struct {
	limit   int
	setRole []uuid.UUID
}

// Search records the limit.
func (f *fakeUsers) Search(_ context.Context, _ string, limit int) ([]user.User, error) {
	f.limit = limit
	return nil, nil
}

// SetRole records the user.
func (f *fakeUsers) SetRole(_ context.Context, id uuid.UUID, role user.Role) (user.User, error) {
	f.setRole = append(f.setRole, id)
	return user.User{ID: id, Role: role}, nil
}

// TestTeamsOverview checks the admin's teams list: a lobby team has no game
// (null), a started one has its game at a glance, taken from its team view.
func TestTeamsOverview(t *testing.T) {
	cooldown := "class_4"
	ft := fakeTeams{list: []teams.Team{{ID: "111111", Status: teams.Lobby}, {ID: "222222", Status: teams.InGame}}}
	fs := fakeState{"222222": {Health: 3, KnockedOut: false, BossesDefeated: 2, CooldownClass: &cooldown, CastCount: 7}}
	a := admin.New(ft, fs, &fakeUsers{}, admin.Rules{UserSearchLimit: 5})
	got := a.Teams()
	if len(got) != 2 || got[0].Game != nil || got[1].Game == nil {
		t.Fatalf("overview: %+v", got)
	}
	if g := got[1].Game; g.Health != 3 || g.BossesDefeated != 2 || g.CooldownClass == nil || *g.CooldownClass != "class_4" || g.CastCount != 7 {
		t.Fatalf("started team's game: %+v", g)
	}
}

// TestSetRoleNotOwn checks that an admin can't change their own role (and
// the user use case isn't asked), but can change someone else's.
func TestSetRoleNotOwn(t *testing.T) {
	fu := &fakeUsers{}
	a := admin.New(fakeTeams{}, fakeState{}, fu, admin.Rules{UserSearchLimit: 5})
	me, other := uuid.New(), uuid.New()
	if _, err := a.SetRole(context.Background(), me, me, user.RolePlayer); !errors.Is(err, admin.ErrCannotChangeOwnRole) {
		t.Fatalf("own role: %v", err)
	}
	if len(fu.setRole) != 0 {
		t.Fatal("the user use case was asked to change the admin's own role")
	}
	if u, err := a.SetRole(context.Background(), me, other, user.RoleHelper); err != nil || u.Role != user.RoleHelper {
		t.Fatalf("someone else's role: %v %+v", err, u)
	}
}

// TestSearchLimit checks that a user search passes admin.user_search_limit.
func TestSearchLimit(t *testing.T) {
	fu := &fakeUsers{}
	a := admin.New(fakeTeams{}, fakeState{}, fu, admin.Rules{UserSearchLimit: 7})
	if _, err := a.SearchUsers(context.Background(), "snu"); err != nil || fu.limit != 7 {
		t.Fatalf("search: %v, limit %d", err, fu.limit)
	}
}
