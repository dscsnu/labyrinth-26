package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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
		return e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"qr": qrToken(w, time.Now().Unix())})
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
	expect(t, "removed after restart", e.call("POST", "/api/teams/me/state/scans", ps[1], map[string]string{"qr": qrToken("1003", time.Now().Unix())}),
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
			scanRes = e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"qr": qrToken("1001", time.Now().Unix())})
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
	expect(t, "scan", e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"qr": qrToken("1001", time.Now().Unix())}),
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

// callList sends a request as p and returns the status and the payload as
// an array (call decodes objects only).
func (e *env) callList(method, path string, p player) (int, []any) {
	e.t.Helper()
	req, _ := http.NewRequest(method, e.backend.URL+path, nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	res := e.do(req)
	defer res.Body.Close()
	var env struct {
		Payload []any `json:"payload"`
	}
	json.NewDecoder(res.Body).Decode(&env)
	return res.StatusCode, env.Payload
}

// castOver selects shape as the leader and scans ids over HTTP, failing
// unless the last scan completes the pattern.
func (e *env) castOver(leader player, shape string, ids []string) apiResult {
	e.t.Helper()
	expect(e.t, "select "+shape, e.call("PUT", "/api/teams/me/state/selection", leader, map[string]string{"shape": shape}), http.StatusOK, "")
	var r apiResult
	for _, w := range ids {
		r = e.call("POST", "/api/teams/me/state/scans", leader, map[string]string{"qr": qrToken(w, time.Now().Unix())})
		expect(e.t, "scan "+w, r, http.StatusOK, "")
	}
	if r.payload["outcome"] != "pattern_completed" {
		e.t.Fatalf("cast %s: %v", shape, r.payload)
	}
	return r
}

// TestAdminTeamsOverview checks GET /api/admin/teams: a lobby team's game is
// null; a started team's has its health, boss, cooldowns and cast count.
func TestAdminTeamsOverview(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	admin := e.newAdmin("admin")
	ps := e.startedTeam("o")
	e.call("POST", "/api/teams", e.newPlayer("lobby"), map[string]string{"name": "Lobby"})
	e.castOver(ps[0], "square", []string{"1001", "1002", "1007", "1008"})

	status, list := e.callList("GET", "/api/admin/teams", admin)
	if status != http.StatusOK || len(list) != 2 {
		t.Fatalf("teams: %d %v", status, list)
	}
	for _, row := range list {
		row := row.(map[string]any)
		team := row["team"].(map[string]any)
		switch team["status"] {
		case "LOBBY":
			if row["game"] != nil {
				t.Fatalf("a lobby team has a game: %v", row)
			}
		case "IN_GAME":
			g := row["game"].(map[string]any)
			if g["health"] != float64(e.cfg.Settings.Game.Health) || g["cast_count"] != float64(1) || g["cooldown_class"] != "class_4" ||
				g["knocked_out"] != false || g["boss"] == nil {
				t.Fatalf("a started team's game: %v", g)
			}
		}
	}
}

// TestAdminDeleteTeam deletes a started team over HTTP: the guard and bad
// IDs first; then every member's stream gets no_team (team_deleted), their
// game routes answer not_in_team, the team is gone from the admin's list,
// a member can create a team at once, and deleting it again is
// team_not_found.
func TestAdminDeleteTeam(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	admin := e.newAdmin("admin")
	ps := e.startedTeam("d")
	teamID := e.call("GET", "/api/teams/me", ps[0], nil).payload["id"].(string)
	expect(t, "player deletes", e.call("DELETE", "/api/admin/teams/"+teamID, ps[0], nil), http.StatusForbidden, "not_admin")
	expect(t, "bad team id", e.call("DELETE", "/api/admin/teams/12", admin, nil), http.StatusBadRequest, "invalid_team_id")

	s := e.openStream(ps[1])
	s.next(t, "connect")
	s.nextState(t, "connect")
	expect(t, "delete", e.call("DELETE", "/api/admin/teams/"+teamID, admin, nil), http.StatusOK, "")
	if ev := s.next(t, "deleted"); ev.name != "no_team" || ev.typ != "team_deleted" {
		t.Fatalf("member's event: %s %s", ev.name, ev.typ)
	}
	expect(t, "state after the delete", e.call("GET", "/api/teams/me/state", ps[0], nil), http.StatusNotFound, "not_in_team")
	if _, list := e.callList("GET", "/api/admin/teams", admin); len(list) != 0 {
		t.Fatalf("teams after the delete: %v", list)
	}
	expect(t, "member creates a team", e.call("POST", "/api/teams", ps[2], map[string]string{"name": "Again"}), http.StatusCreated, "")
	expect(t, "delete again", e.call("DELETE", "/api/admin/teams/"+teamID, admin, nil), http.StatusNotFound, "team_not_found")
}

// TestAdminResetCooldownsOverHTTP casts the square, so its class and its
// waypoints cool down; the reset route clears both (stream type
// cooldowns_reset), so the waypoint scans again; and the route's guard and
// errors.
func TestAdminResetCooldownsOverHTTP(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	if e.cfg.Settings.Game.QRCooldown.Duration <= 0 {
		t.Skip("needs game.qr_cooldown > 0")
	}
	e = e.restart()
	admin := e.newAdmin("admin")
	ps := e.startedTeam("r")
	teamID := e.call("GET", "/api/teams/me", ps[0], nil).payload["id"].(string)
	reset := func(p player, id string) apiResult {
		return e.call("POST", "/api/admin/teams/"+id+"/cooldowns/reset", p, nil)
	}
	e.castOver(ps[0], "square", []string{"1001", "1002", "1007", "1008"})
	expect(t, "select the triangle", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": "triangle"}), http.StatusOK, "")
	expect(t, "a cooled waypoint", e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"qr": qrToken("1001", time.Now().Unix())}),
		http.StatusConflict, "waypoint_on_cooldown")

	expect(t, "player resets", reset(ps[0], teamID), http.StatusForbidden, "not_admin")
	expect(t, "bad team id", reset(admin, "12"), http.StatusBadRequest, "invalid_team_id")
	expect(t, "unknown team", reset(admin, "000000"), http.StatusNotFound, "team_not_found")
	lobby := e.call("POST", "/api/teams", e.newPlayer("lobby"), map[string]string{"name": "Lobby"}).payload["id"].(string)
	expect(t, "lobby team", reset(admin, lobby), http.StatusConflict, "not_in_game")

	s := e.openStream(ps[1])
	s.next(t, "connect")
	s.nextState(t, "connect")
	r := reset(admin, teamID)
	expect(t, "reset", r, http.StatusOK, "")
	if r.payload["cooldown_class"] != nil || len(r.payload["qr_cooldowns"].([]any)) != 0 {
		t.Fatalf("after the reset: %v", r.payload)
	}
	if ev := s.nextState(t, "reset"); ev.typ != "cooldowns_reset" {
		t.Fatalf("reset event: %s", ev.typ)
	}
	expect(t, "the waypoint after the reset", e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"qr": qrToken("1001", time.Now().Unix())}),
		http.StatusOK, "")
}

// TestAdminUsersAndRoles checks the user routes: search by email (case
// insensitive), setting a role (and the promoted user reaching an admin
// route at once, with a token that still says PLAYER), and every refusal:
// a player, an admin's own role, an unknown role, a bad or unknown user.
func TestAdminUsersAndRoles(t *testing.T) {
	e := setup(t)
	admin := e.newAdmin("admin")
	p := e.newPlayer("helpme")
	if status, found := e.callList("GET", "/api/admin/users?email=HELPME", admin); status != http.StatusOK ||
		len(found) != 1 || found[0].(map[string]any)["id"] != p.id {
		t.Fatalf("search: %d %v", status, found)
	}
	role := func(as player, id string, body any) apiResult {
		return e.call("PUT", "/api/admin/users/"+id+"/role", as, body)
	}
	expect(t, "player sets a role", role(p, p.id, map[string]string{"role": "ADMIN"}), http.StatusForbidden, "not_admin")
	expect(t, "own role", role(admin, admin.id, map[string]string{"role": "PLAYER"}), http.StatusConflict, "cannot_change_own_role")
	expect(t, "unknown role", role(admin, p.id, map[string]string{"role": "OWNER"}), http.StatusBadRequest, "invalid_role")
	expect(t, "no role", role(admin, p.id, map[string]string{}), http.StatusBadRequest, "invalid_body")
	expect(t, "bad user id", role(admin, "nope", map[string]string{"role": "HELPER"}), http.StatusBadRequest, "invalid_user_id")
	expect(t, "unknown user", role(admin, uuid.NewString(), map[string]string{"role": "HELPER"}), http.StatusNotFound, "user_not_found")

	r := role(admin, p.id, map[string]string{"role": "ADMIN"})
	expect(t, "promote", r, http.StatusOK, "")
	if r.payload["role"] != "ADMIN" {
		t.Fatalf("promoted: %v", r.payload)
	}
	if status, _ := e.callList("GET", "/api/admin/teams", p); status != http.StatusOK {
		t.Fatalf("the promoted user on an admin route: %d", status)
	}
}

// TestLeaderboardAndFinish gives every boss 1 HP: one team casts until the
// last falls (square and triangle in turn, for the spell cooldown; no QR
// cooldown). It becomes FINISHED (team event finished) and its scans are
// not_in_game. GET /api/leaderboard puts it first, marked finished, with
// the other team's own entry as me; streams open with the leaderboard
// snapshot, and a refresh broadcasts the change as updated.
func TestLeaderboardAndFinish(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := e.db.Pool().Exec(context.Background(), "UPDATE boss SET hp = 1"); err != nil {
		t.Fatal(err)
	}
	e.cfg.Settings.Game.QRCooldown.Duration = 0
	e = e.restart()
	winners := e.startedTeam("w")
	others := e.startedTeam("x")
	watcher := e.openStream(e.newPlayer("watcher"))
	if ev := <-watcher.boards; ev.typ != "snapshot" {
		t.Fatalf("a stream opens with the leaderboard snapshot, got %s", ev.typ)
	}
	team := e.openStream(winners[1])
	team.next(t, "connect")

	for i := range e.cfg.Settings.Game.Bosses {
		if i%2 == 0 {
			e.castOver(winners[0], "square", []string{"1001", "1002", "1007", "1008"})
		} else {
			e.castOver(winners[0], "triangle", []string{"1001", "1002", "1003"})
		}
	}
	eventually(t, "the team is finished", func() bool {
		return e.call("GET", "/api/teams/me", winners[0], nil).payload["status"] == "FINISHED"
	})
	for {
		ev := team.next(t, "finished event")
		if ev.typ == "finished" {
			break
		}
	}
	expect(t, "select after finishing", e.call("PUT", "/api/teams/me/state/selection", winners[0], map[string]string{"shape": "square"}),
		http.StatusConflict, "not_in_game")

	r := e.call("GET", "/api/leaderboard", others[0], nil)
	expect(t, "leaderboard", r, http.StatusOK, "")
	top := r.payload["top"].([]any)
	first := top[0].(map[string]any)
	me := r.payload["me"].(map[string]any)
	if r.payload["teams"] != float64(2) || first["finished"] != true || first["bosses_defeated"] != float64(e.cfg.Settings.Game.Bosses) ||
		me["rank"] != float64(2) || me["bosses_defeated"] != float64(0) {
		t.Fatalf("leaderboard: %v", r.payload)
	}
	if !e.app.Leaderboard.Refresh() {
		t.Fatal("the refresh after the game didn't broadcast")
	}
	select {
	case ev := <-watcher.boards:
		if ev.typ != "updated" || len(ev.data["top"].([]any)) == 0 {
			t.Fatalf("broadcast: %s %v", ev.typ, ev.data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no leaderboard broadcast")
	}
}
