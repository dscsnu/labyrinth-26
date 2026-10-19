package tests

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// startedTeam forms a team of teams.size through the API, starts it, and returns its
// players (leader first).
func (e *env) startedTeam(prefix string) []player {
	e.t.Helper()
	_, ps := e.fullTeam(prefix)
	for _, p := range ps[1:] {
		expect(e.t, "ready", e.call("PUT", "/api/teams/me/ready", p, map[string]bool{"ready": true}), http.StatusOK, "")
	}
	expect(e.t, "start", e.call("POST", "/api/teams/me/start", ps[0], nil), http.StatusOK, "")
	return ps
}

// TestSelectAndScanRoutes walks the team state routes over HTTP with every
// error code: selecting, the body size limit, scanning a square with distinct
// members, one member scanning several waypoints, rejected rescans, a reported
// wrong scan poisoning the attempt until a reselect, and clearing.
func TestSelectAndScanRoutes(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 1012 is in no pattern, so it's wrong for every shape.
	if _, err := e.db.Pool().Exec(context.Background(),
		"INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('1012', 'Nowhere', 0, 0)"); err != nil {
		t.Fatal(err)
	}
	e = e.restart() // content is read at startup
	lobby := e.newPlayer("lobby")
	e.call("POST", "/api/teams", lobby, map[string]string{"name": "Lobby"})
	ps := e.startedTeam("s")

	sel := func(p player, body any) apiResult {
		return e.call("PUT", "/api/teams/me/state/selection", p, body)
	}
	scan := func(p player, w string) apiResult {
		return e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"waypoint_id": w})
	}

	expect(t, "lobby select", sel(lobby, map[string]string{"shape": "square"}), http.StatusConflict, "not_in_game")
	expect(t, "no team", scan(e.newPlayer("x"), "1001"), http.StatusNotFound, "not_in_team")
	expect(t, "bad body", sel(ps[0], map[string]int{"shape": 1}), http.StatusBadRequest, "invalid_body")
	expect(t, "empty scan body", e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{}), http.StatusBadRequest, "invalid_body")
	expect(t, "unknown shape", sel(ps[0], map[string]string{"shape": "hexagon"}), http.StatusNotFound, "unknown_shape")
	expect(t, "unknown waypoint", scan(ps[0], "9999"), http.StatusNotFound, "unknown_waypoint")
	expect(t, "scan before select", scan(ps[0], "1001"), http.StatusConflict, "no_shape_selected")
	huge := strings.Repeat("x", int(e.cfg.Settings.Server.MaxBodyBytes))
	expect(t, "body over server.max_body_bytes", sel(ps[0], map[string]string{"shape": huge}), http.StatusBadRequest, "invalid_body")

	r := sel(ps[1], map[string]string{"shape": "square"})
	expect(t, "select", r, http.StatusOK, "")
	if r.payload["selection"].(map[string]any)["shape"] != "square" {
		t.Fatalf("selection: %v", r.payload)
	}

	r = scan(ps[0], "1001")
	expect(t, "first scan", r, http.StatusOK, "")
	if r.payload["outcome"] != "scan_added" {
		t.Fatalf("first scan: %v", r.payload)
	}
	expect(t, "rescan", scan(ps[0], "1001"), http.StatusConflict, "waypoint_already_scanned")
	expect(t, "same waypoint", scan(ps[1], "1001"), http.StatusConflict, "waypoint_already_scanned")
	expect(t, "same member, another waypoint", scan(ps[0], "1002"), http.StatusOK, "")
	expect(t, "scan 3", scan(ps[2], "1007"), http.StatusOK, "")
	r = scan(ps[3], "1008")
	expect(t, "scan 4", r, http.StatusOK, "")
	if r.payload["outcome"] != "pattern_completed" {
		t.Fatalf("fourth scan: %v", r.payload)
	}

	r = e.call("GET", "/api/teams/me/state", ps[len(ps)-1], nil)
	expect(t, "get state", r, http.StatusOK, "")
	if shapes := r.payload["shapes"].([]any); len(shapes) != 1 || shapes[0] != "square" || r.payload["selection"] != nil {
		t.Fatalf("state: %v", r.payload)
	}

	expect(t, "select again", sel(ps[0], map[string]string{"shape": "triangle"}), http.StatusOK, "")
	r = e.call("DELETE", "/api/teams/me/state/selection", ps[len(ps)-1], nil)
	expect(t, "clear", r, http.StatusOK, "")
	if r.payload["selection"] != nil || len(r.payload["stack"].([]any)) != 0 {
		t.Fatalf("after clear: %v", r.payload)
	}

	// A wrong scan is pushed and reported (wrong_scan, marked wrong, the
	// attempt poisoned); every later scan is refused until a reselect.
	expect(t, "reselect", sel(ps[0], map[string]string{"shape": "square"}), http.StatusOK, "")
	r = scan(ps[0], "1012")
	expect(t, "wrong scan", r, http.StatusOK, "")
	st := r.payload["state"].(map[string]any)
	if r.payload["outcome"] != "wrong_scan" || st["poisoned"] != true || st["stack"].([]any)[0].(map[string]any)["wrong"] != true {
		t.Fatalf("wrong scan: %v", r.payload)
	}
	expect(t, "poisoned attempt", scan(ps[1], "1001"), http.StatusConflict, "selection_poisoned")
	expect(t, "reselect clears it", sel(ps[0], map[string]string{"shape": "square"}), http.StatusOK, "")
	expect(t, "scan after reselect", scan(ps[1], "1001"), http.StatusOK, "")
}

// TestCastLogRoute checks GET /api/teams/me/casts: 404 without a team, an
// empty list before any cast, then every cast newest first, repeats marked.
func TestCastLogRoute(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	expect(t, "no team", e.call("GET", "/api/teams/me/casts", e.newPlayer("x"), nil), http.StatusNotFound, "not_in_team")

	ps := e.startedTeam("c")
	casts := func() []any {
		req, _ := http.NewRequest("GET", e.backend.URL+"/api/teams/me/casts", nil)
		req.Header.Set("Authorization", "Bearer "+ps[0].token)
		res := e.do(req)
		var env struct {
			Payload []any `json:"payload"`
		}
		if err := json.NewDecoder(res.Body).Decode(&env); err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("casts: %d %v", res.StatusCode, err)
		}
		return env.Payload
	}
	if got := casts(); len(got) != 0 {
		t.Fatalf("before any cast: %v", got)
	}
	for range 2 {
		expect(t, "select", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": "square"}), http.StatusOK, "")
		for _, w := range []string{"1001", "1002", "1007", "1008"} {
			expect(t, "scan "+w, e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"waypoint_id": w}), http.StatusOK, "")
		}
	}
	got := casts()
	if len(got) != 2 || got[0].(map[string]any)["already_completed"] != true || got[1].(map[string]any)["already_completed"] != false ||
		got[0].(map[string]any)["shape"] != "square" || got[0].(map[string]any)["damage"].(float64) <= 0 {
		t.Fatalf("cast log: %v", got)
	}
}

// TestGameShapes checks GET /api/game/shapes: it needs a token, lists the
// seed's shapes with their pattern counts, sizes and damage ranges, and
// carries max_pattern_points (and no timing rules: there's no time limit).
func TestGameShapes(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	expect(t, "no token", e.call("GET", "/api/game/shapes", player{}, nil), http.StatusUnauthorized, "missing_token")

	r := e.call("GET", "/api/game/shapes", e.newPlayer("viewer"), nil)
	expect(t, "shapes", r, http.StatusOK, "")
	game := e.cfg.Settings.Game
	if _, ok := r.payload["scan_window_ms"]; ok || r.payload["max_pattern_points"] != float64(game.MaxPatternPoints) {
		t.Fatalf("rules: %v", r.payload)
	}
	shapes := r.payload["shapes"].([]any)
	byID := map[string]map[string]any{}
	for _, s := range shapes {
		m := s.(map[string]any)
		byID[m["id"].(string)] = m
	}
	sq := byID["square"]
	if len(shapes) != 5 || sq["patterns"] != float64(18) || len(sq["points"].([]any)) != 1 || sq["points"].([]any)[0] != float64(4) {
		t.Fatalf("shapes: %v", shapes)
	}
	for id, s := range byID {
		dmg, ok := s["damage"].(map[string]any)
		if !ok || dmg["min"].(float64) <= 0 || dmg["max"].(float64) < dmg["min"].(float64) {
			t.Fatalf("shape %s damage range invalid: %v", id, s["damage"])
		}
	}
}
