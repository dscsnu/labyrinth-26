package tests

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
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
// error code: selecting, the body size limit, scanning a quadrilateral with distinct
// members, one member scanning several waypoints, rejected rescans, a reported
// wrong scan poisoning the attempt until a reselect, and clearing.
func TestSelectAndScanRoutes(t *testing.T) {
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	// 1012 is in no pattern, so it's wrong for every shape.
	if _, err := e.db.Pool().Exec(context.Background(),
		"INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('2099', 'Nowhere', 0, 0)"); err != nil {
		t.Fatal(err)
	}
	// Casts here reuse waypoints: TestQRCooldownOverHTTP covers the QR cooldown.
	e.cfg.Settings.Game.QRCooldown.Duration = 0
	e = e.restart() // content is read at startup
	lobby := e.newPlayer("lobby")
	e.call("POST", "/api/teams", lobby, map[string]string{"name": "Lobby"})
	ps := e.startedTeam("s")

	sel := func(p player, body any) apiResult {
		return e.call("PUT", "/api/teams/me/state/selection", p, body)
	}
	scan := func(p player, w string) apiResult {
		return e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"qr": qrToken(w, time.Now().Unix())})
	}

	expect(t, "lobby select", sel(lobby, map[string]string{"shape": "quadrilateral"}), http.StatusConflict, "not_in_game")
	expect(t, "no team", scan(e.newPlayer("x"), "2001"), http.StatusNotFound, "not_in_team")
	expect(t, "bad body", sel(ps[0], map[string]int{"shape": 1}), http.StatusBadRequest, "invalid_body")
	expect(t, "empty scan body", e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{}), http.StatusBadRequest, "invalid_body")

	// QR verification: a tampered token and a stale one are refused before
	// the waypoint or the team is even looked up.
	fresh := qrToken("2001", time.Now().Unix())
	last := fresh[len(fresh)-1]
	replacement := byte('A')
	if last == 'A' {
		replacement = 'B'
	}
	expect(t, "tampered token", e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"qr": fresh[:len(fresh)-1] + string(replacement)}),
		http.StatusBadRequest, "invalid_qr")
	expect(t, "expired token", e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"qr": qrToken("2001", time.Now().Unix()-100)}),
		http.StatusConflict, "qr_expired")

	expect(t, "unknown shape", sel(ps[0], map[string]string{"shape": "dodecagon"}), http.StatusNotFound, "unknown_shape")
	expect(t, "unknown waypoint", scan(ps[0], "9999"), http.StatusNotFound, "unknown_waypoint")
	expect(t, "scan before select", scan(ps[0], "2001"), http.StatusConflict, "no_shape_selected")
	huge := strings.Repeat("x", int(e.cfg.Settings.Server.MaxBodyBytes))
	expect(t, "body over server.max_body_bytes", sel(ps[0], map[string]string{"shape": huge}), http.StatusBadRequest, "invalid_body")

	expect(t, "select by a non-leader", sel(ps[1], map[string]string{"shape": "quadrilateral"}), http.StatusForbidden, "not_leader")
	expect(t, "clear by a non-leader", e.call("DELETE", "/api/teams/me/state/selection", ps[1], nil), http.StatusForbidden, "not_leader")
	r := sel(ps[0], map[string]string{"shape": "quadrilateral"})
	expect(t, "select", r, http.StatusOK, "")
	if r.payload["selection"].(map[string]any)["shape"] != "quadrilateral" {
		t.Fatalf("selection: %v", r.payload)
	}

	r = scan(ps[0], "2001")
	expect(t, "first scan", r, http.StatusOK, "")
	if r.payload["outcome"] != "scan_added" {
		t.Fatalf("first scan: %v", r.payload)
	}
	expect(t, "rescan", scan(ps[0], "2001"), http.StatusConflict, "waypoint_already_scanned")
	expect(t, "same waypoint", scan(ps[1], "2001"), http.StatusConflict, "waypoint_already_scanned")
	expect(t, "same member, another waypoint", scan(ps[0], "2002"), http.StatusOK, "")
	expect(t, "scan 3", scan(ps[2], "2007"), http.StatusOK, "")
	r = scan(ps[3], "2008")
	expect(t, "scan 4", r, http.StatusOK, "")
	if r.payload["outcome"] != "pattern_completed" {
		t.Fatalf("fourth scan: %v", r.payload)
	}

	r = e.call("GET", "/api/teams/me/state", ps[len(ps)-1], nil)
	expect(t, "get state", r, http.StatusOK, "")
	if shapes := r.payload["shapes"].([]any); len(shapes) != 1 || shapes[0] != "quadrilateral" || r.payload["selection"] != nil {
		t.Fatalf("state: %v", r.payload)
	}

	expect(t, "select again", sel(ps[0], map[string]string{"shape": "triangle"}), http.StatusOK, "")
	r = e.call("DELETE", "/api/teams/me/state/selection", ps[0], nil)
	expect(t, "clear", r, http.StatusOK, "")
	if r.payload["selection"] != nil || len(r.payload["stack"].([]any)) != 0 {
		t.Fatalf("after clear: %v", r.payload)
	}

	// The quadrilateral was the last cast, so its class (class_4) is on cooldown:
	// every quadrilateral is class_4, so the shape can't be selected.
	expect(t, "quadrilateral on cooldown", sel(ps[0], map[string]string{"shape": "quadrilateral"}), http.StatusConflict, "shape_on_cooldown")

	// A wrong scan is pushed and reported (wrong_scan, marked wrong, the
	// attempt poisoned); every later scan is refused until a reselect.
	expect(t, "reselect", sel(ps[0], map[string]string{"shape": "triangle"}), http.StatusOK, "")
	r = scan(ps[0], "2099")
	expect(t, "wrong scan", r, http.StatusOK, "")
	st := r.payload["state"].(map[string]any)
	if r.payload["outcome"] != "wrong_scan" || st["poisoned"] != true || st["stack"].([]any)[0].(map[string]any)["wrong"] != true {
		t.Fatalf("wrong scan: %v", r.payload)
	}
	expect(t, "poisoned attempt", scan(ps[1], "2001"), http.StatusConflict, "selection_poisoned")
	expect(t, "reselect clears it", sel(ps[0], map[string]string{"shape": "triangle"}), http.StatusOK, "")
	expect(t, "scan after reselect", scan(ps[1], "2001"), http.StatusOK, "")
}

// TestCastLogRoute checks GET /api/teams/me/casts: 404 without a team, an
// empty list before any cast, then every cast newest first with its class,
// repeats marked: the quadrilateral, a triangle (the cooldown forbids two quadrilaterals in
// a row), the quadrilateral again.
func TestCastLogRoute(t *testing.T) {
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.cfg.Settings.Game.QRCooldown.Duration = 0 // casts reuse waypoints: TestQRCooldownOverHTTP covers it
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
	for _, c := range []struct {
		shape string
		ids   []string
	}{{"quadrilateral", []string{"2001", "2002", "2007", "2008"}}, {"triangle", []string{"2001", "2002", "2003"}}, {"quadrilateral", []string{"2001", "2002", "2007", "2008"}}} {
		expect(t, "select "+c.shape, e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": c.shape}), http.StatusOK, "")
		for _, w := range c.ids {
			expect(t, "scan "+w, e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"qr": qrToken(w, time.Now().Unix())}), http.StatusOK, "")
		}
	}
	got := casts()
	cast := func(i int) map[string]any { return got[i].(map[string]any) }
	if len(got) != 3 || cast(0)["already_completed"] != true || cast(1)["already_completed"] != false || cast(2)["already_completed"] != false ||
		cast(0)["shape"] != "quadrilateral" || cast(0)["class"] != "class_4" || cast(1)["class"] != "class_3" || cast(0)["damage"].(float64) <= 0 {
		t.Fatalf("cast log: %v", got)
	}
}

// TestGameShapes checks GET /api/game/shapes: it needs a token, lists the
// seed's shapes with their pattern counts, sizes, spell classes (class_n for
// each size n) and damage ranges, carries max_pattern_points and every spell
// class ascending by points (and no timing rules: there's no time limit).
func TestGameShapes(t *testing.T) {
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
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
	sq := byID["quadrilateral"]
	if len(shapes) != 5 || sq["patterns"] != float64(210) || len(sq["points"].([]any)) != 1 || sq["points"].([]any)[0] != float64(4) {
		t.Fatalf("shapes: %v", shapes)
	}
	for id, s := range byID {
		dmg, ok := s["damage"].(map[string]any)
		if !ok || dmg["min"].(float64) <= 0 || dmg["max"].(float64) < dmg["min"].(float64) {
			t.Fatalf("shape %s damage range invalid: %v", id, s["damage"])
		}
		// One class per size, in the same order: a pattern of n points is
		// class_n.
		points, classes := s["points"].([]any), s["classes"].([]any)
		if len(classes) != len(points) {
			t.Fatalf("shape %s: classes %v for points %v", id, classes, points)
		}
		for i, n := range points {
			if want := fmt.Sprintf("class_%d", int(n.(float64))); classes[i] != want {
				t.Fatalf("shape %s: classes %v for points %v, want %s at %d", id, classes, points, want, i)
			}
		}
	}
	// Every spell class, ascending by points.
	sc := r.payload["spell_classes"].([]any)
	for i, c := range sc {
		m := c.(map[string]any)
		if m["points"] != float64(i+1) || m["id"] != fmt.Sprintf("class_%d", i+1) || m["name"] != fmt.Sprintf("Class %d", i+1) {
			t.Fatalf("spell_classes[%d] = %v", i, m)
		}
	}
	if len(sc) == 0 {
		t.Fatal("no spell_classes")
	}
}

// TestGameBosses checks GET /api/game/bosses: it needs a token, is empty
// before content is loaded, then lists the seed's game.bosses bosses in
// serial order; and a cast over HTTP shows up in team_state as damage to
// boss 1, with what the cast dealt in last_cast.
func TestGameBosses(t *testing.T) {
	e := setup(t)
	expect(t, "no token", e.call("GET", "/api/game/bosses", player{}, nil), http.StatusUnauthorized, "missing_token")
	list := func() []any {
		req, _ := http.NewRequest("GET", e.backend.URL+"/api/game/bosses", nil)
		req.Header.Set("Authorization", "Bearer "+e.newPlayer("viewer").token)
		res := e.do(req)
		var env struct {
			Payload []any `json:"payload"`
		}
		if err := json.NewDecoder(res.Body).Decode(&env); err != nil || res.StatusCode != http.StatusOK {
			t.Fatalf("bosses: %d %v", res.StatusCode, err)
		}
		return env.Payload
	}
	if got := list(); len(got) != 0 {
		t.Fatalf("before seeding: %v", got)
	}
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart() // content is read at startup
	got := list()
	if len(got) != e.cfg.Settings.Game.Bosses {
		t.Fatalf("%d bosses, want game.bosses = %d", len(got), e.cfg.Settings.Game.Bosses)
	}
	for i, b := range got {
		b := b.(map[string]any)
		if b["serial"] != float64(i+1) || b["name"] == "" || b["asset"] == "" || b["hp"].(float64) <= 0 {
			t.Fatalf("boss %d: %v", i, b)
		}
	}

	ps := e.startedTeam("b")
	r := e.call("GET", "/api/teams/me/state", ps[0], nil)
	if boss := r.payload["boss"].(map[string]any); boss["serial"] != float64(1) || boss["damage"] != float64(0) ||
		r.payload["bosses_defeated"] != float64(0) || r.payload["damage_multiplier"] != float64(1) {
		t.Fatalf("before a cast: %v", r.payload)
	}
	expect(t, "select", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": "quadrilateral"}), http.StatusOK, "")
	for _, w := range []string{"2001", "2002", "2007", "2008"} {
		r = e.call("POST", "/api/teams/me/state/scans", ps[0], map[string]string{"qr": qrToken(w, time.Now().Unix())})
	}
	st := r.payload["state"].(map[string]any)
	lc := st["last_cast"].(map[string]any)
	// Whether the quadrilateral defeats boss 1 depends on its seeded HP.
	defeated := lc["dealt"].(float64) >= got[0].(map[string]any)["hp"].(float64)
	wantBoss, wantDamage, wantDefeated := float64(1), lc["dealt"], float64(0)
	if defeated {
		wantBoss, wantDamage, wantDefeated = 2, float64(0), 1
	}
	if lc["boss"] != float64(1) || lc["dealt"] != lc["damage"] || lc["defeated_boss"] != defeated ||
		st["boss"].(map[string]any)["serial"] != wantBoss || st["boss"].(map[string]any)["damage"] != wantDamage ||
		st["bosses_defeated"] != wantDefeated {
		t.Fatalf("after a cast: %v", st)
	}
}

// TestSpellCooldownOverHTTP checks the spell cooldown end to end, with two
// started teams: team A casts the quadrilateral over HTTP; its stream's spell_cast
// event and GET state carry cooldown_class class_4 and cooldown_shapes; its
// leader reselecting the quadrilateral is 409 shape_on_cooldown, with no event; team
// B, which hasn't cast, still selects the quadrilateral (the cooldown is per team);
// a restart keeps A's cooldown; A's triangle cast moves it to class_3.
func TestSpellCooldownOverHTTP(t *testing.T) {
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.cfg.Settings.Game.QRCooldown.Duration = 0 // casts reuse waypoints: TestQRCooldownOverHTTP covers it
	e = e.restart()
	a, b := e.startedTeam("cda"), e.startedTeam("cdb")
	sel := func(p player, shape string) apiResult {
		return e.call("PUT", "/api/teams/me/state/selection", p, map[string]string{"shape": shape})
	}
	castAs := func(p player, shape string, ids []string) map[string]any {
		expect(t, "select "+shape, sel(p, shape), http.StatusOK, "")
		var r apiResult
		for _, w := range ids {
			r = e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"qr": qrToken(w, time.Now().Unix())})
			expect(t, "scan "+w, r, http.StatusOK, "")
		}
		if r.payload["outcome"] != "pattern_completed" {
			t.Fatalf("%s: outcome %v", shape, r.payload["outcome"])
		}
		return r.payload["state"].(map[string]any)
	}
	cooldown := func(st map[string]any) (any, []any) { return st["cooldown_class"], st["cooldown_shapes"].([]any) }
	same := func(got []any, want ...string) bool {
		if len(got) != len(want) {
			return false
		}
		for i := range want {
			if got[i] != want[i] {
				return false
			}
		}
		return true
	}

	s := e.openStream(a[1])
	s.nextState(t, "snapshot")
	if c, shapes := cooldown(e.call("GET", "/api/teams/me/state", a[0], nil).payload); c != nil || len(shapes) != 0 {
		t.Fatalf("before any cast: cooldown %v, shapes %v", c, shapes)
	}
	st := castAs(a[0], "quadrilateral", []string{"2001", "2002", "2007", "2008"})
	if c, shapes := cooldown(st); c != "class_4" || !same(shapes, "quadrilateral") {
		t.Fatalf("A's cast response: cooldown %v, shapes %v", c, shapes)
	}
	var ev sseEvent
	for ev.typ != "spell_cast" && ev.typ != "boss_defeated" {
		ev = s.nextState(t, "the cast's event")
	}
	if c, shapes := cooldown(ev.data); c != "class_4" || !same(shapes, "quadrilateral") {
		t.Fatalf("A's cast event (%s): cooldown %v, shapes %v", ev.typ, c, shapes)
	}

	expect(t, "A reselects the quadrilateral", sel(a[0], "quadrilateral"), http.StatusConflict, "shape_on_cooldown")
	select {
	case ev := <-s.states:
		t.Fatalf("a refused selection published %s", ev.typ)
	case <-time.After(100 * time.Millisecond):
	}
	expect(t, "B selects the quadrilateral (its own cooldown is empty)", sel(b[0], "quadrilateral"), http.StatusOK, "")
	if c, _ := cooldown(e.call("GET", "/api/teams/me/state", b[0], nil).payload); c != nil {
		t.Fatalf("B's cooldown %v after A's cast", c)
	}

	e = e.restart()
	if c, shapes := cooldown(e.call("GET", "/api/teams/me/state", a[0], nil).payload); c != "class_4" || !same(shapes, "quadrilateral") {
		t.Fatalf("A after a restart: cooldown %v, shapes %v", c, shapes)
	}
	expect(t, "A reselects the quadrilateral after a restart", sel(a[0], "quadrilateral"), http.StatusConflict, "shape_on_cooldown")
	st = castAs(a[0], "triangle", []string{"2001", "2002", "2003"})
	if c, shapes := cooldown(st); c != "class_3" || !same(shapes, "triangle") {
		t.Fatalf("A after a triangle: cooldown %v, shapes %v", c, shapes)
	}
	castAs(a[0], "quadrilateral", []string{"2001", "2002", "2007", "2008"})
}

// TestQRCooldownOverHTTP checks the QR cooldown end to end, with two started
// teams and config.toml's game.qr_cooldown: team A casts the quadrilateral over
// HTTP; the cast's response and stream event carry its 4 waypoints in
// qr_cooldowns, until cast_at + qr_cooldown; with the triangle selected, A
// scanning 2001 (in both) is 409 waypoint_on_cooldown, with no event, while
// 2003 scans; team B scans 2001 freely (the cooldown is per team); a restart
// keeps A's qr_cooldowns, rebuilt from the cast log.
func TestQRCooldownOverHTTP(t *testing.T) {
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	cd := e.cfg.Settings.Game.QRCooldown.Duration
	if cd < time.Minute {
		t.Skipf("test assumes game.qr_cooldown >= 1m, longer than it runs (got %v)", cd)
	}
	quad := []string{"2001", "2002", "2007", "2008"}
	a, b := e.startedTeam("qra"), e.startedTeam("qrb")
	sel := func(p player, shape string) {
		expect(t, "select "+shape, e.call("PUT", "/api/teams/me/state/selection", p, map[string]string{"shape": shape}), http.StatusOK, "")
	}
	scan := func(p player, w string) apiResult {
		return e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"qr": qrToken(w, time.Now().Unix())})
	}
	// cooling returns st's qr_cooldowns waypoint IDs, failing unless every
	// until is cast_at + qr_cooldown.
	cooling := func(what string, st map[string]any) []string {
		castAt, err := time.Parse(time.RFC3339Nano, st["last_cast"].(map[string]any)["cast_at"].(string))
		if err != nil {
			t.Fatal(err)
		}
		var ids []string
		for _, c := range st["qr_cooldowns"].([]any) {
			c := c.(map[string]any)
			until, err := time.Parse(time.RFC3339Nano, c["until"].(string))
			if err != nil || !until.Equal(castAt.Add(cd)) {
				t.Fatalf("%s: %v until %v, want cast_at %v + %v", what, c["waypoint_id"], c["until"], castAt, cd)
			}
			ids = append(ids, c["waypoint_id"].(string))
		}
		return ids
	}

	s := e.openStream(a[1])
	if ev := s.nextState(t, "snapshot"); len(ev.data["qr_cooldowns"].([]any)) != 0 {
		t.Fatalf("before any cast: qr_cooldowns %v", ev.data["qr_cooldowns"])
	}
	sel(a[0], "quadrilateral")
	var r apiResult
	for _, w := range quad {
		r = scan(a[0], w)
		expect(t, "A scans "+w, r, http.StatusOK, "")
	}
	if got := cooling("A's cast response", r.payload["state"].(map[string]any)); !slices.Equal(got, quad) {
		t.Fatalf("A's cast response: qr_cooldowns %v, want %v", got, quad)
	}
	var ev sseEvent
	for ev.typ != "spell_cast" && ev.typ != "boss_defeated" {
		ev = s.nextState(t, "the cast's event")
	}
	if got := cooling("A's cast event", ev.data); !slices.Equal(got, quad) {
		t.Fatalf("A's cast event (%s): qr_cooldowns %v, want %v", ev.typ, got, quad)
	}

	sel(a[0], "triangle")
	s.nextState(t, "select")
	expect(t, "A scans 2001 on cooldown", scan(a[1], "2001"), http.StatusConflict, "waypoint_on_cooldown")
	select {
	case ev := <-s.states:
		t.Fatalf("a refused scan published %s", ev.typ)
	case <-time.After(100 * time.Millisecond):
	}
	expect(t, "A scans 2003, not in the quadrilateral", scan(a[1], "2003"), http.StatusOK, "")
	sel(b[0], "quadrilateral")
	expect(t, "B scans 2001 (its own cooldowns are empty)", scan(b[0], "2001"), http.StatusOK, "")

	e = e.restart()
	if got := cooling("A after a restart", e.call("GET", "/api/teams/me/state", a[0], nil).payload); !slices.Equal(got, quad) {
		t.Fatalf("A after a restart: qr_cooldowns %v, want %v", got, quad)
	}
	expect(t, "A scans 2002 after a restart", scan(a[0], "2002"), http.StatusConflict, "waypoint_on_cooldown")
}
