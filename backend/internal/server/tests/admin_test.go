package tests

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"
)

// newAdmin signs in a player and makes them ADMIN in the database. Their token
// still says PLAYER: admin routes must read the role from the database.
func (e *env) newAdmin(name string) player {
	e.t.Helper()
	p := e.newPlayer(name)
	e.setRole(p, "ADMIN")
	return p
}

// setRole sets p's role in the database.
func (e *env) setRole(p player, role string) {
	e.t.Helper()
	if _, err := e.db.Pool().Exec(context.Background(), "UPDATE userprofile SET role = $2 WHERE id = $1", p.id, role); err != nil {
		e.t.Fatal(err)
	}
}

// removePath is the admin route removing userID from teamID, naming newLeader
// if it isn't empty.
func removePath(teamID, userID, newLeader string) string {
	p := fmt.Sprintf("/api/admin/teams/%s/members/%s", teamID, userID)
	if newLeader != "" {
		p += "?new_leader=" + newLeader
	}
	return p
}

// TestAdminGuard checks that admin routes need a token and the ADMIN role as
// stored now: a player is refused, an admin whose token says PLAYER is let
// in, and demoting them takes effect on the next request.
func TestAdminGuard(t *testing.T) {
	e := setup(t)
	p := e.newPlayer("p")
	expect(t, "no token", e.call("GET", "/api/admin/teams", player{}, nil), http.StatusUnauthorized, "missing_token")
	expect(t, "player", e.call("GET", "/api/admin/teams", p, nil), http.StatusForbidden, "not_admin")
	expect(t, "player removes", e.call("DELETE", removePath("123456", p.id, ""), p, nil), http.StatusForbidden, "not_admin")

	e.setRole(p, "ADMIN") // the token still says PLAYER
	expect(t, "promoted", e.call("GET", "/api/admin/teams", p, nil), http.StatusOK, "")
	e.setRole(p, "PLAYER")
	expect(t, "demoted", e.call("GET", "/api/admin/teams", p, nil), http.StatusForbidden, "not_admin")
}

// TestAdminRemoveFromLobby walks every rule and error code of removing a
// member from a lobby team: bad IDs, unknown team, not a member, the leader
// needing a valid new leader, a non-leader not taking one, the events each
// side gets, the list route, and the last member being refused. The removed
// player is free to create a team.
func TestAdminRemoveFromLobby(t *testing.T) {
	e := setup(t)
	admin := e.newAdmin("admin")
	teamID, ps := e.fullTeam("l")
	outsider := e.newPlayer("outsider")
	remove := func(user, newLeader string) apiResult {
		return e.call("DELETE", removePath(teamID, user, newLeader), admin, nil)
	}

	expect(t, "bad team id", e.call("DELETE", removePath("12", ps[1].id, ""), admin, nil), http.StatusBadRequest, "invalid_team_id")
	expect(t, "bad user id", remove("nope", ""), http.StatusBadRequest, "invalid_user_id")
	expect(t, "bad new leader", remove(ps[0].id, "nope"), http.StatusBadRequest, "invalid_user_id")
	expect(t, "unknown team", e.call("DELETE", removePath("000000", ps[1].id, ""), admin, nil), http.StatusNotFound, "team_not_found")
	expect(t, "not a member", remove(outsider.id, ""), http.StatusNotFound, "not_a_member")
	expect(t, "leader, no new leader", remove(ps[0].id, ""), http.StatusConflict, "new_leader_required")
	expect(t, "leader replaces itself", remove(ps[0].id, ps[0].id), http.StatusConflict, "invalid_new_leader")
	expect(t, "new leader outside", remove(ps[0].id, outsider.id), http.StatusConflict, "invalid_new_leader")
	expect(t, "non-leader with new leader", remove(ps[1].id, ps[2].id), http.StatusConflict, "invalid_new_leader")

	removed := e.openStream(ps[1])
	expectTeam(t, "removed, on connect", removed.next(t, "connect"), len(ps))
	stay := e.openStream(ps[2])
	expectTeam(t, "member, on connect", stay.next(t, "connect"), len(ps))

	r := remove(ps[1].id, "")
	expect(t, "remove a member", r, http.StatusOK, "")
	if len(members(r)) != len(ps)-1 || r.payload["leader_id"] != ps[0].id {
		t.Fatalf("after removal: %v", r.payload)
	}
	expectNoTeam(t, "removed player", removed.next(t, "removal"))
	expectTeam(t, "member sees it", stay.next(t, "removal"), len(ps)-1)
	expect(t, "removed has no team", e.call("GET", "/api/teams/me", ps[1], nil), http.StatusNotFound, "not_in_team")

	r = remove(ps[0].id, ps[3].id)
	expect(t, "remove the leader", r, http.StatusOK, "")
	if r.payload["leader_id"] != ps[3].id || len(members(r)) != len(ps)-2 {
		t.Fatalf("after removing the leader: %v", r.payload)
	}
	expect(t, "new leader can start", e.call("POST", "/api/teams/me/start", ps[3], nil), http.StatusConflict, "not_all_ready")

	list := e.call("GET", "/api/admin/teams", admin, nil)
	expect(t, "list", list, http.StatusOK, "")

	for _, p := range ps[4:] {
		expect(t, "remove the rest", remove(p.id, ""), http.StatusOK, "")
	}
	expect(t, "remove another", remove(ps[2].id, ""), http.StatusOK, "")
	expect(t, "last member", remove(ps[3].id, ""), http.StatusConflict, "last_member")
	expect(t, "last member, as leader", remove(ps[3].id, ps[2].id), http.StatusConflict, "last_member")

	expect(t, "removed player creates a team", e.call("POST", "/api/teams", ps[1], map[string]string{"name": "Again"}), http.StatusCreated, "")
}

// TestAdminRemoveFromStartedTeam removes a member from an IN_GAME team: their
// stream gets no_team and no more team_state, their game writes get
// not_in_team, the rest play on (the removed player's scan stays in the
// stack), the removed player can join a lobby team, and a restart keeps it.
func TestAdminRemoveFromStartedTeam(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	admin := e.newAdmin("admin")
	ps := e.startedTeam("g")
	teamID := e.call("GET", "/api/teams/me", ps[0], nil).payload["id"].(string)
	scan := func(p player, w string) apiResult {
		return e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"waypoint_id": w})
	}

	expect(t, "select", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": "square"}), http.StatusOK, "")
	expect(t, "removed player's scan, before", scan(ps[1], "1001"), http.StatusOK, "")

	removed := e.openStream(ps[1])
	removed.next(t, "connect")
	removed.nextState(t, "connect")
	stay := e.openStream(ps[2])
	stay.next(t, "connect")
	stay.nextState(t, "connect")

	expect(t, "remove", e.call("DELETE", removePath(teamID, ps[1].id, ""), admin, nil), http.StatusOK, "")
	expectNoTeam(t, "removed player", removed.next(t, "removal"))
	expectTeam(t, "member sees it", stay.next(t, "removal"), len(ps)-1)

	expect(t, "removed scans", scan(ps[1], "1002"), http.StatusNotFound, "not_in_team")
	expect(t, "removed selects", e.call("PUT", "/api/teams/me/state/selection", ps[1], map[string]string{"shape": "square"}),
		http.StatusNotFound, "not_in_team")
	expect(t, "removed reads state", e.call("GET", "/api/teams/me/state", ps[1], nil), http.StatusNotFound, "not_in_team")

	// The team plays on; the removed player's scan still counts.
	expect(t, "scan 2", scan(ps[2], "1002"), http.StatusOK, "")
	expect(t, "scan 3", scan(ps[3], "1007"), http.StatusOK, "")
	r := scan(ps[0], "1008")
	if r.payload["outcome"] != "pattern_completed" {
		t.Fatalf("completion with the removed player's scan: %v", r.payload)
	}
	for range 3 {
		stay.nextState(t, "member gets each scan")
	}
	select {
	case ev := <-removed.states:
		t.Fatalf("removed player got a team_state: %v", ev.data)
	case <-time.After(200 * time.Millisecond):
	}

	lobby := e.call("POST", "/api/teams", e.newPlayer("lobby"), map[string]string{"name": "Lobby"})
	expect(t, "lobby team", lobby, http.StatusCreated, "")
	expect(t, "removed player joins a lobby team", e.call("POST", "/api/teams/join", ps[1], map[string]string{"team_id": lobby.payload["id"].(string)}),
		http.StatusOK, "")

	e = e.restart()
	if got := members(e.call("GET", "/api/teams/me", ps[0], nil)); len(got) != len(ps)-1 {
		t.Fatalf("after restart: %d members", len(got))
	}
	expect(t, "removed after restart", e.call("POST", "/api/teams/me/state/scans", ps[1], map[string]string{"waypoint_id": "1003"}),
		http.StatusConflict, "not_in_game")
}

// TestAdminRemoveRacesScan races a member's scan against an admin removing
// that member, for every non-leader: the removal always succeeds, and the
// scan is either accepted (it ran first, and stays in the stack) or refused
// with not_in_team (it ran after), never accepted after the removal. Which
// one wins isn't controlled (over HTTP the scan usually does); the refused
// path is checked deterministically in TestAdminRemoveFromStartedTeam.
func TestAdminRemoveRacesScan(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	admin := e.newAdmin("admin")
	ps := e.startedTeam("r")
	teamID := e.call("GET", "/api/teams/me", ps[0], nil).payload["id"].(string)
	accepted := 0
	for i, p := range ps[1:] {
		expect(t, "select", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": "square"}), http.StatusOK, "")
		var scanRes, removeRes apiResult
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			scanRes = e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"waypoint_id": "1001"})
		}()
		go func() { defer wg.Done(); removeRes = e.call("DELETE", removePath(teamID, p.id, ""), admin, nil) }()
		wg.Wait()
		expect(t, "removal", removeRes, http.StatusOK, "")
		stack := e.call("GET", "/api/teams/me/state", ps[0], nil).payload["stack"].([]any)
		switch {
		case scanRes.status == http.StatusOK && len(stack) == 1:
			accepted++
		case scanRes.status == http.StatusNotFound && scanRes.code == "not_in_team" && len(stack) == 0:
		default:
			t.Fatalf("round %d: scan %d %s with stack %v", i, scanRes.status, scanRes.code, stack)
		}
	}
	t.Logf("%d of %d scans ran before their removal", accepted, len(ps)-1)
}

// TestAdminRevive knocks a started team out (a stored phase with 0 health,
// applied at restart) and checks the HTTP side: health and knocked_out in
// team_state, selecting and scanning refused with knocked_out, the revive
// route's guard and errors, and a revive restoring game.health, a fresh
// attack clock, and play.
func TestAdminRevive(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	admin := e.newAdmin("admin")
	ps := e.startedTeam("k")
	teamID := e.call("GET", "/api/teams/me", ps[0], nil).payload["id"].(string)
	revive := func(p player, id string) apiResult {
		return e.call("POST", "/api/admin/teams/"+id+"/revive", p, nil)
	}

	r := e.call("GET", "/api/teams/me/state", ps[0], nil)
	if r.payload["health"] != float64(e.cfg.Settings.Game.Health) || r.payload["knocked_out"] != false ||
		r.payload["boss"].(map[string]any)["next_attack_at"] == nil {
		t.Fatalf("a started team: %v", r.payload)
	}

	if _, err := e.db.Pool().Exec(context.Background(),
		"INSERT INTO team_boss (team_id, phase_health, phase_started_at) VALUES ($1, 0, now())", teamID); err != nil {
		t.Fatal(err)
	}
	e = e.restart() // team state is read at startup
	r = e.call("GET", "/api/teams/me/state", ps[1], nil)
	if r.payload["health"] != float64(0) || r.payload["knocked_out"] != true || r.payload["boss"].(map[string]any)["next_attack_at"] != nil {
		t.Fatalf("knocked out: %v", r.payload)
	}
	expect(t, "select", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": "square"}),
		http.StatusConflict, "knocked_out")
	expect(t, "scan", e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"waypoint_id": "1001"}),
		http.StatusConflict, "knocked_out")

	expect(t, "player revives", revive(ps[0], teamID), http.StatusForbidden, "not_admin")
	expect(t, "unknown team", revive(admin, "000000"), http.StatusNotFound, "team_not_found")
	lobby := e.call("POST", "/api/teams", e.newPlayer("lobby"), map[string]string{"name": "Lobby"}).payload["id"].(string)
	expect(t, "lobby team", revive(admin, lobby), http.StatusConflict, "not_in_game")

	s := e.openStream(ps[2])
	s.next(t, "connect")
	s.nextState(t, "connect")
	r = revive(admin, teamID)
	expect(t, "revive", r, http.StatusOK, "")
	if r.payload["health"] != float64(e.cfg.Settings.Game.Health) || r.payload["knocked_out"] != false ||
		r.payload["boss"].(map[string]any)["attacks"] != float64(0) {
		t.Fatalf("revived: %v", r.payload)
	}
	if ev := s.nextState(t, "revive"); ev.typ != "revived" || ev.data["health"] != float64(e.cfg.Settings.Game.Health) {
		t.Fatalf("revive event: %s %v", ev.typ, ev.data)
	}
	expect(t, "select after revive", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": "square"}),
		http.StatusOK, "")
}
