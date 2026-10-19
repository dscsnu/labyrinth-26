package tests

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"

	"labyrinth/internal/user"
)

// player is a signed-in test user.
type player struct {
	id    string
	token string
}

// newPlayer signs in a test user named name and returns their token.
func (e *env) newPlayer(name string) player {
	e.t.Helper()
	u, err := e.users.SignInWithGoogle(context.Background(), user.GoogleProfile{
		Subject: "sub-" + name, Email: name + "@snu.edu.in", Name: name,
	})
	if err != nil {
		e.t.Fatal(err)
	}
	tok, _, err := e.tokens.IssueAccess(u)
	if err != nil {
		e.t.Fatal(err)
	}
	return player{id: u.ID.String(), token: tok}
}

// apiResult is a decoded API response.
type apiResult struct {
	status  int
	code    string
	payload map[string]any
}

// call sends a JSON request as p and decodes the envelope.
func (e *env) call(method, path string, p player, body any) apiResult {
	e.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, e.backend.URL+path, &buf)
	req.Header.Set("Content-Type", "application/json")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	var out struct {
		Code    string         `json:"code"`
		Payload map[string]any `json:"payload"`
	}
	json.NewDecoder(res.Body).Decode(&out)
	return apiResult{status: res.StatusCode, code: out.Code, payload: out.Payload}
}

// expect fails unless got has that status and error code.
func expect(t *testing.T, what string, got apiResult, status int, code string) {
	t.Helper()
	if got.status != status || got.code != code {
		t.Fatalf("%s: got %d %q, want %d %q", what, got.status, got.code, status, code)
	}
}

// members returns the members array of a team payload.
func members(r apiResult) []any {
	m, _ := r.payload["members"].([]any)
	return m
}

// fullTeam creates a team of teams.size: led by players[0], everyone else
// joins.
func (e *env) fullTeam(prefix string) (string, []player) {
	e.t.Helper()
	ps := make([]player, e.cfg.Settings.Teams.Size)
	for i := range ps {
		ps[i] = e.newPlayer(fmt.Sprintf("%s%d", prefix, i))
	}
	r := e.call("POST", "/api/teams", ps[0], map[string]string{"name": prefix})
	expect(e.t, "create", r, http.StatusCreated, "")
	teamID := r.payload["id"].(string)
	for _, p := range ps[1:] {
		expect(e.t, "join", e.call("POST", "/api/teams/join", p, map[string]string{"team_id": teamID}), http.StatusOK, "")
	}
	return teamID, ps
}

// TestTeamLifecycle walks a team from creation to start, checking every rule
// and error code on the way.
func TestTeamLifecycle(t *testing.T) {
	e := setup(t)
	leader := e.newPlayer("leader")

	expect(t, "unauthenticated", e.call("GET", "/api/teams/me", player{}, nil), http.StatusUnauthorized, "missing_token")
	expect(t, "no team yet", e.call("GET", "/api/teams/me", leader, nil), http.StatusNotFound, "not_in_team")

	r := e.call("POST", "/api/teams", leader, map[string]string{"name": "  Evil Exes  "})
	expect(t, "create", r, http.StatusCreated, "")
	teamID := r.payload["id"].(string)
	if r.payload["name"] != "Evil Exes" || r.payload["leader_id"] != leader.id ||
		r.payload["status"] != "LOBBY" || len(members(r)) != 1 || len(teamID) != 6 {
		t.Fatalf("unexpected new team %v", r.payload)
	}
	expect(t, "create while in team", e.call("POST", "/api/teams", leader, map[string]string{"name": "x"}), http.StatusConflict, "already_in_team")

	join := func(p player) apiResult {
		return e.call("POST", "/api/teams/join", p, map[string]string{"team_id": teamID})
	}
	ps := []player{leader}
	for i := 1; i < e.cfg.Settings.Teams.Size; i++ {
		p := e.newPlayer(fmt.Sprintf("m%d", i))
		r = join(p)
		expect(t, "join", r, http.StatusOK, "")
		ps = append(ps, p)
	}
	if len(members(r)) != e.cfg.Settings.Teams.Size {
		t.Fatalf("want %d members, got %d", e.cfg.Settings.Teams.Size, len(members(r)))
	}

	extra := e.newPlayer("extra")
	expect(t, "join full team", join(extra), http.StatusConflict, "team_full")
	expect(t, "join twice", join(ps[1]), http.StatusConflict, "already_in_team")
	expect(t, "join unknown team", e.call("POST", "/api/teams/join", extra, map[string]string{"team_id": "000000"}), http.StatusNotFound, "team_not_found")
	expect(t, "join bad id", e.call("POST", "/api/teams/join", extra, map[string]string{"team_id": "12ab"}), http.StatusBadRequest, "invalid_team_id")

	ready := func(p player, v bool) apiResult {
		return e.call("PUT", "/api/teams/me/ready", p, map[string]bool{"ready": v})
	}
	start := func(p player) apiResult { return e.call("POST", "/api/teams/me/start", p, nil) }

	expect(t, "leader readies", ready(leader, true), http.StatusConflict, "leader_cannot_ready")
	expect(t, "ready without body", e.call("PUT", "/api/teams/me/ready", ps[1], map[string]any{}), http.StatusBadRequest, "invalid_body")
	expect(t, "member starts", start(ps[1]), http.StatusForbidden, "not_leader")
	expect(t, "start, nobody ready", start(leader), http.StatusConflict, "not_all_ready")

	for _, p := range ps[1:] {
		expect(t, "ready", ready(p, true), http.StatusOK, "")
	}
	expect(t, "unready", ready(ps[1], false), http.StatusOK, "")
	expect(t, "start, one unready", start(leader), http.StatusConflict, "not_all_ready")
	expect(t, "ready again", ready(ps[1], true), http.StatusOK, "")

	last := ps[len(ps)-1]
	expect(t, "member leaves", e.call("POST", "/api/teams/leave", last, nil), http.StatusOK, "")
	expect(t, "rejoin", join(last), http.StatusOK, "")
	expect(t, "start, rejoiner unready", start(leader), http.StatusConflict, "not_all_ready")
	expect(t, "rejoiner readies", ready(last, true), http.StatusOK, "")

	r = start(leader)
	expect(t, "start", r, http.StatusOK, "")
	if r.payload["status"] != "IN_GAME" || r.payload["started_at"] == nil {
		t.Fatalf("not started: %v", r.payload)
	}

	expect(t, "start twice", start(leader), http.StatusConflict, "team_locked")
	expect(t, "leave in game", e.call("POST", "/api/teams/leave", ps[2], nil), http.StatusConflict, "team_locked")
	expect(t, "unready in game", ready(ps[2], false), http.StatusConflict, "team_locked")
	expect(t, "join in game", join(extra), http.StatusConflict, "team_locked")

	r = e.call("GET", "/api/teams/me", ps[3], nil)
	expect(t, "member reads team", r, http.StatusOK, "")
	if r.payload["id"] != teamID || r.payload["status"] != "IN_GAME" {
		t.Fatalf("GET /api/teams/me: %v", r.payload)
	}
	for _, m := range members(r) {
		if _, hasEmail := m.(map[string]any)["email"]; hasEmail {
			t.Fatal("team members expose email")
		}
	}
}

// TestLeaderLeaving checks that leadership passes to the longest-standing
// member and that the last one out deletes the team.
func TestLeaderLeaving(t *testing.T) {
	e := setup(t)
	a, b, c := e.newPlayer("a"), e.newPlayer("b"), e.newPlayer("c")

	r := e.call("POST", "/api/teams", a, map[string]string{"name": "T"})
	teamID := r.payload["id"].(string)
	expect(t, "b joins", e.call("POST", "/api/teams/join", b, map[string]string{"team_id": teamID}), http.StatusOK, "")
	expect(t, "c joins", e.call("POST", "/api/teams/join", c, map[string]string{"team_id": teamID}), http.StatusOK, "")
	expect(t, "b readies", e.call("PUT", "/api/teams/me/ready", b, map[string]bool{"ready": true}), http.StatusOK, "")

	expect(t, "leader leaves", e.call("POST", "/api/teams/leave", a, nil), http.StatusOK, "")
	r = e.call("GET", "/api/teams/me", c, nil)
	if r.payload["leader_id"] != b.id {
		t.Fatalf("leadership should pass to longest-standing member b, got %v", r.payload["leader_id"])
	}
	expect(t, "new leader can't ready", e.call("PUT", "/api/teams/me/ready", b, map[string]bool{"ready": true}), http.StatusConflict, "leader_cannot_ready")

	expect(t, "b leaves", e.call("POST", "/api/teams/leave", b, nil), http.StatusOK, "")
	expect(t, "c leaves", e.call("POST", "/api/teams/leave", c, nil), http.StatusOK, "")
	expect(t, "leave again", e.call("POST", "/api/teams/leave", c, nil), http.StatusNotFound, "not_in_team")
	expect(t, "empty team is deleted", e.call("POST", "/api/teams/join", a, map[string]string{"team_id": teamID}), http.StatusNotFound, "team_not_found")
}

// TestTeamNameValidation checks the team name rules, with the length limit
// from config.toml teams.name_max_length (counted in characters, so a
// multi-byte name at the limit is fine).
func TestTeamNameValidation(t *testing.T) {
	e := setup(t)
	max := e.cfg.Settings.Teams.NameMaxLength
	cases := map[string]int{
		"":                          http.StatusBadRequest,
		"   ":                       http.StatusBadRequest,
		strings.Repeat("x", max+1):  http.StatusBadRequest,
		"bad\nname":                 http.StatusBadRequest,
		strings.Repeat("é", max):    http.StatusCreated,
		"Scott Pilgrim's Fan Club!": http.StatusCreated,
	}
	i := 0
	for name, want := range cases {
		i++
		r := e.call("POST", "/api/teams", e.newPlayer(fmt.Sprintf("n%d", i)), map[string]string{"name": name})
		if r.status != want {
			t.Errorf("name %q: got %d, want %d", name, r.status, want)
		}
	}
}

// TestConcurrentJoinsRespectTeamSize races twice as many players as there's
// room for to join a team with 1 member: exactly teams.size-1 may get in.
func TestConcurrentJoinsRespectTeamSize(t *testing.T) {
	e := setup(t)
	leader := e.newPlayer("leader")
	teamID := e.call("POST", "/api/teams", leader, map[string]string{"name": "Race"}).payload["id"].(string)

	joiners := make([]player, 2*(e.cfg.Settings.Teams.Size-1))
	for i := range joiners {
		joiners[i] = e.newPlayer(fmt.Sprintf("j%d", i))
	}

	var wg sync.WaitGroup
	results := make([]apiResult, len(joiners))
	for i, p := range joiners {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = e.call("POST", "/api/teams/join", p, map[string]string{"team_id": teamID})
		}()
	}
	wg.Wait()

	ok, full := 0, 0
	for _, r := range results {
		switch {
		case r.status == http.StatusOK:
			ok++
		case r.code == "team_full":
			full++
		default:
			t.Fatalf("unexpected result %+v", r)
		}
	}
	if ok != e.cfg.Settings.Teams.Size-1 || full != len(joiners)-ok {
		t.Fatalf("%d joined, %d rejected as full; want %d joined", ok, full, e.cfg.Settings.Teams.Size-1)
	}
	if n := len(members(e.call("GET", "/api/teams/me", leader, nil))); n != e.cfg.Settings.Teams.Size {
		t.Fatalf("team has %d members, want %d", n, e.cfg.Settings.Teams.Size)
	}
}

// TestConcurrentStartAndLeave races the leader's start against a member
// leaving: either the start wins (the leave is refused, team_locked) or the
// leave wins and the smaller team starts; the outcome is always consistent.
func TestConcurrentStartAndLeave(t *testing.T) {
	e := setup(t)
	for round := range 10 {
		_, ps := e.fullTeam(fmt.Sprintf("r%d-", round))
		for _, p := range ps[1:] {
			e.call("PUT", "/api/teams/me/ready", p, map[string]bool{"ready": true})
		}

		var wg sync.WaitGroup
		var start, leave apiResult
		wg.Add(2)
		go func() { defer wg.Done(); start = e.call("POST", "/api/teams/me/start", ps[0], nil) }()
		go func() { defer wg.Done(); leave = e.call("POST", "/api/teams/leave", ps[len(ps)-1], nil) }()
		wg.Wait()

		team := e.call("GET", "/api/teams/me", ps[0], nil)
		switch {
		case start.status == http.StatusOK && leave.code == "team_locked":
			if team.payload["status"] != "IN_GAME" || len(members(team)) != e.cfg.Settings.Teams.Size {
				t.Fatalf("round %d: started but team is %v", round, team.payload)
			}
		case leave.status == http.StatusOK && start.status == http.StatusOK:
			// The member left first; any size can start, so the rest started.
			if team.payload["status"] != "IN_GAME" || len(members(team)) != e.cfg.Settings.Teams.Size-1 {
				t.Fatalf("round %d: member left, then started, but team is %v", round, team.payload)
			}
		default:
			t.Fatalf("round %d: inconsistent outcome start=%+v leave=%+v", round, start, leave)
		}
	}
}

// TestUnroutedRequestsUseEnvelope checks that an unknown path and a wrong
// method get JSON envelopes with stable codes, not plain text.
func TestUnroutedRequestsUseEnvelope(t *testing.T) {
	e := setup(t)
	p := e.newPlayer("u")
	expect(t, "unknown path", e.call("GET", "/api/nope", p, nil), http.StatusNotFound, "not_found")
	expect(t, "unknown path, no token", e.call("GET", "/api/nope", player{}, nil), http.StatusNotFound, "not_found")
	expect(t, "wrong method", e.call("DELETE", "/api/teams", p, nil), http.StatusMethodNotAllowed, "method_not_allowed")
}

// TestStartWithAnySize checks that a team of any size can start: the leader
// alone, or a partly filled team once every non-leader is ready.
func TestStartWithAnySize(t *testing.T) {
	e := setup(t)
	solo := e.newPlayer("solo")
	expect(t, "create", e.call("POST", "/api/teams", solo, map[string]string{"name": "Solo"}), http.StatusCreated, "")
	r := e.call("POST", "/api/teams/me/start", solo, nil)
	expect(t, "leader alone starts", r, http.StatusOK, "")
	if r.payload["status"] != "IN_GAME" || len(members(r)) != 1 {
		t.Fatalf("solo start: %v", r.payload)
	}

	if e.cfg.Settings.Teams.Size < 3 {
		t.Skip("the partial-team case needs teams.size >= 3")
	}
	leader, a, b := e.newPlayer("l"), e.newPlayer("a"), e.newPlayer("b")
	created := e.call("POST", "/api/teams", leader, map[string]string{"name": "Three"})
	id := created.payload["id"].(string)
	for _, p := range []player{a, b} {
		expect(t, "join", e.call("POST", "/api/teams/join", p, map[string]string{"team_id": id}), http.StatusOK, "")
	}
	expect(t, "b not ready", e.call("POST", "/api/teams/me/start", leader, nil), http.StatusConflict, "not_all_ready")
	for _, p := range []player{a, b} {
		expect(t, "ready", e.call("PUT", "/api/teams/me/ready", p, map[string]bool{"ready": true}), http.StatusOK, "")
	}
	r = e.call("POST", "/api/teams/me/start", leader, nil)
	expect(t, "three of teams.size start", r, http.StatusOK, "")
	if len(members(r)) != 3 {
		t.Fatalf("partial start: %v", r.payload)
	}
}
