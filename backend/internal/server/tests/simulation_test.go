package tests

// Simulated teams: each player is a client with its own network delay each
// way and its own event stream, delivered after that delay. There's no time
// limit on an attempt, so these tests are about concurrency and convergence:
// scans racing over uneven networks, and every phone ending on the same
// team_state.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"sync"
	"testing"
	"time"

	"labyrinth/internal/platform/postgres/pgtest"
)

// simSquare is a quadrilateral in the 2026 seed.
var simSquare = []string{"2001", "2002", "2007", "2008"}

// simClient is one player's phone: a network delay each way, and an event
// stream delivered after the same delay.
type simClient struct {
	e        *env
	p        player
	up, down time.Duration // request and response delays

	mu       sync.Mutex
	stack    []any          // last stack seen
	shape    []string       // completed shapes last seen
	poisoned bool           // last poisoned flag seen
	state    map[string]any // the newest team_state seen, by version
	noTeam   bool           // a no_team event arrived (removed from the team)
}

// newSimClient connects p's event stream with delay down, and returns its
// client.
func (e *env) newSimClient(p player, up, down time.Duration) *simClient {
	c := &simClient{e: e, p: p, up: up, down: down}
	s := e.openStream(p)
	// Stamp each event as it arrives, then deliver it down after that, in
	// order. (Sleeping down per event would add the delays up.)
	type stamped struct {
		data map[string]any
		at   time.Time
	}
	arrived := make(chan stamped, 64)
	go func() {
		defer close(arrived)
		for ev := range s.states {
			arrived <- stamped{ev.data, time.Now()}
		}
	}()
	go func() {
		for ev := range s.events {
			if ev.name == "no_team" {
				c.mu.Lock()
				c.noTeam = true
				c.mu.Unlock()
			}
		}
	}()
	go func() {
		for ev := range arrived {
			time.Sleep(time.Until(ev.at.Add(c.down)))
			c.apply(ev.data)
		}
	}()
	return c
}

// apply records a team state the client just received, if it's newer than
// the one held (by version: a response and its event arrive in either
// order), as frontend/src/lib/types/Live.type.ts isNewer does.
func (c *simClient) apply(state map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.state != nil {
		held, in := c.state["version"].(map[string]any), state["version"].(map[string]any)
		if held["epoch"] == in["epoch"] && in["seq"].(float64) <= held["seq"].(float64) {
			return
		}
	}
	c.state = state
	c.stack, _ = state["stack"].([]any)
	c.poisoned, _ = state["poisoned"].(bool)
	c.shape = nil
	for _, s := range state["shapes"].([]any) {
		c.shape = append(c.shape, s.(string))
	}
}

// scan sends a scan with the client's delays, and returns the status, error
// code and outcome.
func (c *simClient) scan(waypoint string) (status int, code, outcome string, err error) {
	time.Sleep(c.up)
	body, _ := json.Marshal(map[string]string{"qr": qrToken(waypoint, time.Now().Unix())})
	req, _ := http.NewRequest("POST", c.e.backend.URL+"/api/teams/me/state/scans", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+c.p.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, "", "", err
	}
	defer res.Body.Close()
	var env struct {
		Code    string `json:"code"`
		Payload struct {
			Outcome string         `json:"outcome"`
			State   map[string]any `json:"state"`
		} `json:"payload"`
	}
	json.NewDecoder(res.Body).Decode(&env)
	time.Sleep(c.down)
	if res.StatusCode == http.StatusOK {
		c.apply(env.Payload.State)
	}
	return res.StatusCode, env.Code, env.Payload.Outcome, nil
}

// eventually fails unless cond holds within a few seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s: not within 3s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// simTeam starts a team on the 2026 seed (plus 2099, in no pattern: the one
// wrong scan), gives every player a client (delays
// from delay(i)), and selects shape.
func simTeam(t *testing.T, shape string, delay func(i int) (up, down time.Duration)) (*env, []*simClient) {
	t.Helper()
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Every scan of the 2026 checkpoints fits its shape; 2099 (in no
	// pattern) is the one wrong scan.
	if _, err := e.db.Pool().Exec(context.Background(), "INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('2099', 'Nowhere', 0, 0)"); err != nil {
		t.Fatal(err)
	}
	e = e.restart()
	ps := e.startedTeam("sim")
	clients := make([]*simClient, len(ps))
	for i, p := range ps {
		up, down := delay(i)
		clients[i] = e.newSimClient(p, up, down)
	}
	expect(t, "select", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": shape}), http.StatusOK, "")
	return e, clients
}

// TestSimTeamCompletesUnderLatency has four players scan a square at once,
// each with its own delays (up to 150 ms each way): exactly one scan completes
// it, and every player's screen ends with the stack purged and the square
// completed.
func TestSimTeamCompletesUnderLatency(t *testing.T) {
	_, cs := simTeam(t, "quadrilateral", func(i int) (time.Duration, time.Duration) {
		return time.Duration(i*30) * time.Millisecond, time.Duration(150-i*25) * time.Millisecond
	})
	var wg sync.WaitGroup
	outcomes := make([]string, len(simSquare))
	for i, w := range simSquare {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, code, outcome, err := cs[i].scan(w)
			if err != nil || status != http.StatusOK {
				t.Errorf("player %d scanning %s: %d %s %v", i, w, status, code, err)
			}
			outcomes[i] = outcome
		}()
	}
	wg.Wait()
	completed := 0
	for _, o := range outcomes {
		if o == "pattern_completed" {
			completed++
		}
	}
	if completed != 1 {
		t.Fatalf("outcomes %v: want exactly one pattern_completed", outcomes)
	}
	for _, c := range cs {
		eventually(t, "player sees the purge and the square", func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return len(c.stack) == 0 && len(c.shape) == 1 && c.shape[0] == "quadrilateral"
		})
	}
}

// TestSimRaceSameWaypoint has two players scan the same waypoint at the same
// moment: one is accepted, the other gets waypoint_already_scanned, and every
// screen converges on a stack holding that one scan.
func TestSimRaceSameWaypoint(t *testing.T) {
	_, cs := simTeam(t, "quadrilateral", func(i int) (time.Duration, time.Duration) {
		return time.Duration(i*5) * time.Millisecond, 10 * time.Millisecond
	})
	type result struct {
		status int
		code   string
	}
	results := make([]result, 2)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func() {
			defer wg.Done()
			status, code, _, _ := cs[i].scan(simSquare[0])
			results[i] = result{status, code}
		}()
	}
	wg.Wait()
	ok, rejected := 0, 0
	for _, r := range results {
		switch {
		case r.status == http.StatusOK:
			ok++
		case r.status == http.StatusConflict && r.code == "waypoint_already_scanned":
			rejected++
		}
	}
	if ok != 1 || rejected != 1 {
		t.Fatalf("results %+v: want one 200 and one waypoint_already_scanned", results)
	}
	for _, c := range cs {
		eventually(t, "screens converge", func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return len(c.stack) == 1
		})
	}
}

// TestSimWrongScanReachesEveryone has one player scan a waypoint that can't
// be part of the square: they're told (wrong_scan), every phone sees the
// attempt poisoned, a teammate's scan racing it is either accepted before it
// or refused after it (selection_poisoned), and a reselect clears it for all.
func TestSimWrongScanReachesEveryone(t *testing.T) {
	e, cs := simTeam(t, "quadrilateral", func(i int) (time.Duration, time.Duration) {
		return time.Duration(i*10) * time.Millisecond, time.Duration(60-i*10) * time.Millisecond
	})
	if status, code, _, err := cs[0].scan("2001"); err != nil || status != http.StatusOK {
		t.Fatalf("first scan: %d %s %v", status, code, err)
	}
	var wg sync.WaitGroup
	var wrongOutcome string
	var other struct {
		status int
		code   string
	}
	wg.Add(2)
	go func() { defer wg.Done(); _, _, wrongOutcome, _ = cs[1].scan("2099") }() // in no pattern: wrong either way
	go func() { defer wg.Done(); other.status, other.code, _, _ = cs[2].scan("2002") }()
	wg.Wait()
	if wrongOutcome != "wrong_scan" {
		t.Fatalf("the wrong scan's outcome: %q", wrongOutcome)
	}
	if other.status != http.StatusOK && other.code != "selection_poisoned" {
		t.Fatalf("the racing scan: %d %s", other.status, other.code)
	}
	for _, c := range cs {
		eventually(t, "every phone sees the poisoned attempt", func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return c.poisoned
		})
	}

	expect(t, "reselect", e.call("PUT", "/api/teams/me/state/selection", cs[0].p, map[string]string{"shape": "quadrilateral"}), http.StatusOK, "")
	for _, c := range cs {
		eventually(t, "every phone sees it cleared", func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return !c.poisoned && len(c.stack) == 0
		})
	}
}

// call sends a request with the client's delays and returns the status,
// error code and payload; a payload holding team state (the state itself,
// or a scan's "state") is applied.
func (c *simClient) call(method, path string, body any) (status int, code string, payload map[string]any) {
	time.Sleep(c.up)
	var buf bytes.Buffer
	if body != nil {
		json.NewEncoder(&buf).Encode(body)
	}
	req, _ := http.NewRequest(method, c.e.backend.URL+path, &buf)
	req.Header.Set("Authorization", "Bearer "+c.p.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0, err.Error(), nil
	}
	defer res.Body.Close()
	var env struct {
		Code    string         `json:"code"`
		Payload map[string]any `json:"payload"`
	}
	json.NewDecoder(res.Body).Decode(&env)
	time.Sleep(c.down)
	if res.StatusCode == http.StatusOK && env.Payload != nil {
		if st, ok := env.Payload["state"].(map[string]any); ok {
			c.apply(st)
		} else if _, ok := env.Payload["version"]; ok {
			c.apply(env.Payload)
		}
	}
	return res.StatusCode, env.Code, env.Payload
}

// teamOfSize forms and starts a team of n players over HTTP, leader first.
func (e *env) teamOfSize(prefix string, n int) []player {
	e.t.Helper()
	ps := make([]player, n)
	for i := range ps {
		ps[i] = e.newPlayer(fmt.Sprintf("%s-%d", prefix, i))
	}
	r := e.call("POST", "/api/teams", ps[0], map[string]string{"name": prefix})
	expect(e.t, "create", r, http.StatusCreated, "")
	for _, p := range ps[1:] {
		expect(e.t, "join", e.call("POST", "/api/teams/join", p, map[string]string{"team_id": r.payload["id"].(string)}), http.StatusOK, "")
		expect(e.t, "ready", e.call("PUT", "/api/teams/me/ready", p, map[string]bool{"ready": true}), http.StatusOK, "")
	}
	expect(e.t, "start", e.call("POST", "/api/teams/me/start", ps[0], nil), http.StatusOK, "")
	return ps
}

// simPattern is one seed pattern: its shape and waypoint IDs.
type simPattern struct {
	shape     string
	waypoints []string
}

// httpStrategies are the ways an HTTP-simulated team plays; team i plays
// httpStrategies[i % len] at size 1 + (i / len) % teams.size.
var httpStrategies = []string{"steady", "solo", "sloppy", "racer", "retrier", "quitter", "shrinking"}

// TestSim100Teams plays simTeams teams of every size at once over real HTTP
// and event streams, each player on their own delays (0–30 ms each way),
// each team by a strategy: casting with members taking turns, one member
// scanning everything, a wrong scan then a refused scan then a reselect, two
// members racing for a point, resent scans, giving up, and an admin removing
// a member mid-game. Every request's status and code must be the expected
// one; at the end every phone must hold its team's state (version and cast
// count), and every removed player must have had no_team. Each team picks
// its next pattern of another size than its last cast's (the spell cooldown
// refuses a spell of the last cast's class).
func TestSim100Teams(t *testing.T) {
	const teams, rounds = 100, 6
	e := setup(t)
	ctx := context.Background()
	if err := e.db.Seed2026(ctx); err != nil {
		t.Fatal(err)
	}
	// 2099 is in no pattern: wrong for every shape. Bosses never attack, and
	// QR codes don't cool down (back-to-back rounds reuse waypoints): this is
	// about requests and streams, not time.
	if _, err := e.db.Pool().Exec(ctx, "INSERT INTO waypoint (id, name, latitude, longitude) VALUES ('2099', 'Nowhere', 0, 0)"); err != nil {
		t.Fatal(err)
	}
	e.cfg.Settings.Game.BossAttack.Interval.Duration = 10 * 365 * 24 * time.Hour // never, for this test
	e.cfg.Settings.Game.QRCooldown.Duration = 0
	e = e.restart()
	admin := e.newAdmin("sim-admin")

	var patterns []simPattern
	rows := pgtest.Must(e.db.Pool().Query(ctx, `
		SELECT p.shape_id, array_agg(pw.waypoint_id ORDER BY pw.waypoint_id)
		FROM pattern p JOIN pattern_waypoint pw ON pw.pattern_id = p.id
		GROUP BY p.id HAVING count(*) BETWEEN 2 AND 4 ORDER BY p.id -- 5+ points are locked until boss 5`))
	for rows.Next() {
		var p simPattern
		if err := rows.Scan(&p.shape, &p.waypoints); err != nil {
			t.Fatal(err)
		}
		patterns = append(patterns, p)
	}

	type simTeam struct {
		strategy string
		id       string
		clients  []*simClient
		removed  int // index of the removed member, or -1
		casts    int
		// cooldown is the waypoint count of the team's last cast: its
		// spell class (class_n) is on cooldown, so the next pattern is
		// picked of another size. 0 before the first cast.
		cooldown int
	}
	sims := make([]*simTeam, teams)
	for i := range sims {
		strategy := httpStrategies[i%len(httpStrategies)]
		ps := e.teamOfSize(fmt.Sprintf("h%d", i), 1+(i/len(httpStrategies))%e.cfg.Settings.Teams.Size)
		st := &simTeam{strategy: strategy, removed: -1,
			id: e.call("GET", "/api/teams/me", ps[0], nil).payload["id"].(string)}
		rng := rand.New(rand.NewPCG(uint64(i), 7))
		for _, p := range ps {
			st.clients = append(st.clients, e.newSimClient(p,
				time.Duration(rng.IntN(30))*time.Millisecond, time.Duration(rng.IntN(30))*time.Millisecond))
		}
		sims[i] = st
	}

	var wg sync.WaitGroup
	for i, st := range sims {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(i), 8))
			fail := func(format string, args ...any) {
				t.Errorf("team %d (%s, %d members): %s", i, st.strategy, len(st.clients), fmt.Sprintf(format, args...))
			}
			active := func() []*simClient {
				var out []*simClient
				for m, c := range st.clients {
					if m != st.removed {
						out = append(out, c)
					}
				}
				return out
			}
			want := func(what string, status int, code string, wantStatus int, wantCode string) bool {
				if status != wantStatus || code != wantCode {
					fail("%s: %d %s, want %d %s", what, status, code, wantStatus, wantCode)
					return false
				}
				return true
			}
			// cast selects p's shape and scans its points, by who(k).
			cast := func(p simPattern, who func(k int) *simClient, resend bool) {
				status, code, _ := who(0).call("PUT", "/api/teams/me/state/selection", map[string]string{"shape": p.shape})
				if !want("select", status, code, http.StatusOK, "") {
					return
				}
				for k, w := range p.waypoints {
					status, code, outcome, _ := who(k).scan(w)
					if !want("scan "+w, status, code, http.StatusOK, "") {
						return
					}
					if k == len(p.waypoints)-1 && outcome != "pattern_completed" {
						fail("the last scan's outcome is %q", outcome)
					}
					if resend {
						status, code, _, _ := who(k + 1).scan(w)
						wantCode := "waypoint_already_scanned"
						if k == len(p.waypoints)-1 {
							wantCode = "no_shape_selected"
						}
						want("resent "+w, status, code, http.StatusConflict, wantCode)
					}
				}
				st.casts++
				st.cooldown = len(p.waypoints)
			}
			turns := func(k int) *simClient { a := active(); return a[k%len(a)] }

			for round := range rounds {
				p := patterns[rng.IntN(len(patterns))]
				for len(p.waypoints) == st.cooldown { // its class is on cooldown
					p = patterns[rng.IntN(len(patterns))]
				}
				switch st.strategy {
				case "steady":
					cast(p, turns, false)
				case "solo":
					cast(p, func(int) *simClient { return st.clients[0] }, false)
				case "retrier":
					cast(p, turns, true)
				case "quitter":
					if round%2 == 0 {
						cast(p, turns, false)
						continue
					}
					status, code, _ := turns(0).call("PUT", "/api/teams/me/state/selection", map[string]string{"shape": p.shape})
					want("select", status, code, http.StatusOK, "")
					status, code, _, _ = turns(1).scan(p.waypoints[0])
					want("scan", status, code, http.StatusOK, "")
					status, code, payload := turns(0).call("DELETE", "/api/teams/me/state/selection", nil)
					if want("clear", status, code, http.StatusOK, "") && (payload["selection"] != nil || len(payload["stack"].([]any)) != 0) {
						fail("after clearing: %v", payload)
					}
				case "sloppy":
					status, code, _ := turns(0).call("PUT", "/api/teams/me/state/selection", map[string]string{"shape": p.shape})
					want("select", status, code, http.StatusOK, "")
					status, code, _, _ = turns(0).scan(p.waypoints[0])
					want("first scan", status, code, http.StatusOK, "")
					status, code, outcome, _ := turns(1).scan("2099")
					if want("wrong scan", status, code, http.StatusOK, "") && outcome != "wrong_scan" {
						fail("the wrong scan's outcome is %q", outcome)
					}
					status, code, _, _ = turns(2).scan(p.waypoints[1])
					want("scan on a poisoned attempt", status, code, http.StatusConflict, "selection_poisoned")
					cast(p, turns, false) // reselecting purges it
				case "racer":
					status, code, _ := turns(0).call("PUT", "/api/teams/me/state/selection", map[string]string{"shape": p.shape})
					if !want("select", status, code, http.StatusOK, "") {
						continue
					}
					results := make([]string, 2)
					var race sync.WaitGroup
					for r := range 2 {
						race.Add(1)
						go func() {
							defer race.Done()
							status, code, _, _ := turns(r).scan(p.waypoints[0])
							results[r] = fmt.Sprintf("%d %s", status, code)
						}()
					}
					race.Wait()
					if !((results[0] == "200 " && results[1] == "409 waypoint_already_scanned") ||
						(results[1] == "200 " && results[0] == "409 waypoint_already_scanned")) {
						fail("race on %s: %v", p.waypoints[0], results)
						continue
					}
					for k, w := range p.waypoints[1:] {
						status, code, _, _ := turns(k + 1).scan(w)
						want("scan "+w+" after the race", status, code, http.StatusOK, "")
					}
					st.casts++
					st.cooldown = len(p.waypoints)
				case "shrinking":
					if round == rounds/2 && len(st.clients) > 1 {
						st.removed = len(st.clients) - 1
						r := e.call("DELETE", fmt.Sprintf("/api/admin/teams/%s/members/%s", st.id, st.clients[st.removed].p.id), admin, nil)
						want("admin removal", r.status, r.code, http.StatusOK, "")
						status, code, _, _ := st.clients[st.removed].scan("2001")
						want("the removed member's scan", status, code, http.StatusNotFound, "not_in_team")
					}
					cast(p, turns, false)
				}
			}
		}()
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}

	for i, st := range sims {
		truth := e.call("GET", "/api/teams/me/state", st.clients[0].p, nil)
		if st.removed == 0 {
			truth = e.call("GET", "/api/teams/me/state", st.clients[1].p, nil)
		}
		if truth.payload["cast_count"] != float64(st.casts) {
			t.Fatalf("team %d (%s): cast_count %v, want %d", i, st.strategy, truth.payload["cast_count"], st.casts)
		}
		for m, c := range st.clients {
			if m == st.removed {
				eventually(t, fmt.Sprintf("team %d: the removed player gets no_team", i), func() bool {
					c.mu.Lock()
					defer c.mu.Unlock()
					return c.noTeam
				})
				continue
			}
			eventually(t, fmt.Sprintf("team %d (%s): player %d converges", i, st.strategy, m), func() bool {
				c.mu.Lock()
				defer c.mu.Unlock()
				return c.state != nil && fmt.Sprint(c.state["version"]) == fmt.Sprint(truth.payload["version"]) &&
					c.state["cast_count"] == truth.payload["cast_count"]
			})
		}
	}
}
