package tests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/admin"
	"labyrinth/internal/platform/postgres/pgtest"
	"labyrinth/internal/qte"
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

// GrantPowerup is unused here.
func (fakeState) GrantPowerup(context.Context, string, teamstate.Powerup) (teamstate.GrantResult, error) {
	return teamstate.GrantResult{}, nil
}

// QTEStats is unused here.
func (fakeState) QTEStats() map[int64]teamstate.QTEStat { return nil }

// statsState is a fakeState whose QTEStats are fixed.
type statsState struct {
	fakeState
	stats map[int64]teamstate.QTEStat
}

// QTEStats returns the fixed stats.
func (s statsState) QTEStats() map[int64]teamstate.QTEStat { return s.stats }

// fakeQTEs is an admin.QTEs holding a fixed list; a QTE's status is "open"
// once started.
type fakeQTEs struct{ list []qte.QTE }

// List returns the QTEs.
func (f fakeQTEs) List() []qte.QTE { return f.list }

// Status is draft or open.
func (fakeQTEs) Status(q qte.QTE) string {
	if q.StartedAt == nil {
		return qte.StatusDraft
	}
	return qte.StatusOpen
}

// Create is unused here.
func (fakeQTEs) Create(context.Context, uuid.UUID, qte.Draft, bool) (qte.QTE, error) {
	return qte.QTE{}, nil
}

// Start is unused here.
func (fakeQTEs) Start(context.Context, int64) (qte.QTE, error) { return qte.QTE{}, nil }

// End is unused here.
func (fakeQTEs) End(context.Context, int64) (qte.QTE, error) { return qte.QTE{}, nil }

// fakeUsers is an admin.Users recording what it was asked.
type fakeUsers struct {
	limit     int
	setRole   []uuid.UUID
	searchErr error // what Search fails with, if set
}

// Search records the limit.
func (f *fakeUsers) Search(_ context.Context, _ string, limit int) ([]user.User, error) {
	f.limit = limit
	return nil, f.searchErr
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
	fs := fakeState{"222222": {Health: 3, KnockedOut: false, BossesDefeated: 2, CooldownClass: &cooldown, CastCount: 7,
		Powerups: teamstate.PowerupsView{Held: []string{"heart"}, Active: []teamstate.ActivePowerupView{{Kind: "damage_multiplier"}}}}}
	a := admin.New(ft, fs, fakeQTEs{}, &fakeUsers{}, admin.Rules{UserSearchLimit: 5})
	got := a.Teams()
	if len(got) != 2 || got[0].Game != nil || got[1].Game == nil {
		t.Fatalf("overview: %+v", got)
	}
	if g := got[1].Game; g.Health != 3 || g.BossesDefeated != 2 || g.CooldownClass == nil || *g.CooldownClass != "class_4" || g.CastCount != 7 ||
		len(g.Powerups.Held) != 1 || g.Powerups.Held[0] != "heart" || len(g.Powerups.Active) != 1 {
		t.Fatalf("started team's game: %+v", g)
	}
}

// TestQTEsOverview checks the admin's QTE list: newest first as the qte use
// case lists them, each with its status and its answers (by_option {} when
// nobody answered, never null).
func TestQTEsOverview(t *testing.T) {
	now := time.Now()
	fq := fakeQTEs{list: []qte.QTE{{ID: 2, StartedAt: &now, EndsAt: &now}, {ID: 1}}}
	fs := statsState{stats: map[int64]teamstate.QTEStat{2: {Answered: 3, Correct: 1, Granted: 1, ByOption: map[int]int{0: 2, 1: 1}}}}
	got := admin.New(fakeTeams{}, fs, fq, &fakeUsers{}, admin.Rules{}).QTEs()
	if len(got) != 2 || got[0].ID != 2 || got[0].Status != qte.StatusOpen || got[0].Stats.Answered != 3 ||
		got[1].ID != 1 || got[1].Status != qte.StatusDraft || got[1].Stats.ByOption == nil {
		t.Fatalf("overview: %+v", got)
	}
	js := string(pgtest.Must(json.Marshal(got[1])))
	if !strings.Contains(js, `"id":1,`) || !strings.Contains(js, `"status":"draft"`) || !strings.Contains(js, `"by_option":{}`) {
		t.Fatalf("a draft's row: %s", js)
	}
}

// TestSetRoleNotOwn checks that an admin can't change their own role (and
// the user use case isn't asked), but can change someone else's.
func TestSetRoleNotOwn(t *testing.T) {
	fu := &fakeUsers{}
	a := admin.New(fakeTeams{}, fakeState{}, fakeQTEs{}, fu, admin.Rules{UserSearchLimit: 5})
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
	a := admin.New(fakeTeams{}, fakeState{}, fakeQTEs{}, fu, admin.Rules{UserSearchLimit: 7})
	if _, err := a.SearchUsers(context.Background(), "snu"); err != nil || fu.limit != 7 {
		t.Fatalf("search: %v, limit %d", err, fu.limit)
	}
}

// failingQTEs is an admin.QTEs whose writes fail with an error admin
// doesn't map (Postgres down, say).
type failingQTEs struct{ fakeQTEs }

// Start fails.
func (failingQTEs) Start(context.Context, int64) (qte.QTE, error) {
	return qte.QTE{}, errors.New("database down")
}

// TestAPIInternalErrors checks the admin routes' failure paths that need no
// database: an error the error table doesn't map is a logged 500 (a QTE
// start, a user search), and the revive route's team id is checked first.
func TestAPIInternalErrors(t *testing.T) {
	mux := http.NewServeMux()
	a := admin.New(fakeTeams{}, fakeState{}, failingQTEs{}, &fakeUsers{searchErr: errors.New("database down")}, admin.Rules{UserSearchLimit: 5})
	admin.NewAPI(a, pgtest.Quiet).Routes(mux, func(h http.HandlerFunc) http.Handler { return h })
	for _, c := range []struct {
		method, path string
		status       int
		code         string
	}{
		{"POST", "/api/admin/qtes/1/start", http.StatusInternalServerError, ""},
		{"GET", "/api/admin/users?email=x", http.StatusInternalServerError, ""},
		{"POST", "/api/admin/teams/12/revive", http.StatusBadRequest, "invalid_team_id"},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(c.method, c.path, nil))
		var env struct {
			Code string `json:"code"`
		}
		pgtest.Must(0, json.Unmarshal(w.Body.Bytes(), &env))
		if w.Code != c.status || env.Code != c.code {
			t.Errorf("%s %s: %d %q, want %d %q", c.method, c.path, w.Code, env.Code, c.status, c.code)
		}
	}
}
