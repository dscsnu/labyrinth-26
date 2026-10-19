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
	"net/http"
	"sync"
	"testing"
	"time"
)

// simSquare is a square in the 2025 seed.
var simSquare = []string{"1001", "1002", "1007", "1008"}

// simClient is one player's phone: a network delay each way, and an event
// stream delivered after the same delay.
type simClient struct {
	e        *env
	p        player
	up, down time.Duration // request and response delays

	mu       sync.Mutex
	stack    []any    // last stack seen
	shape    []string // completed shapes last seen
	poisoned bool     // last poisoned flag seen
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
		for ev := range arrived {
			time.Sleep(time.Until(ev.at.Add(c.down)))
			c.apply(ev.data)
		}
	}()
	return c
}

// apply records a team state the client just received.
func (c *simClient) apply(state map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
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
	body, _ := json.Marshal(map[string]string{"waypoint_id": waypoint})
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

// simTeam starts a team on the 2025 seed, gives every player a client (delays
// from delay(i)), and selects shape.
func simTeam(t *testing.T, shape string, delay func(i int) (up, down time.Duration)) (*env, []*simClient) {
	t.Helper()
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
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
	_, cs := simTeam(t, "square", func(i int) (time.Duration, time.Duration) {
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
			return len(c.stack) == 0 && len(c.shape) == 1 && c.shape[0] == "square"
		})
	}
}

// TestSimRaceSameWaypoint has two players scan the same waypoint at the same
// moment: one is accepted, the other gets waypoint_already_scanned, and every
// screen converges on a stack holding that one scan.
func TestSimRaceSameWaypoint(t *testing.T) {
	_, cs := simTeam(t, "square", func(i int) (time.Duration, time.Duration) {
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
	e, cs := simTeam(t, "square", func(i int) (time.Duration, time.Duration) {
		return time.Duration(i*10) * time.Millisecond, time.Duration(60-i*10) * time.Millisecond
	})
	if status, code, _, err := cs[0].scan("1001"); err != nil || status != http.StatusOK {
		t.Fatalf("first scan: %d %s %v", status, code, err)
	}
	var wg sync.WaitGroup
	var wrongOutcome string
	var other struct {
		status int
		code   string
	}
	wg.Add(2)
	go func() { defer wg.Done(); _, _, wrongOutcome, _ = cs[1].scan("1004") }() // in no square with 1001: wrong either way
	go func() { defer wg.Done(); other.status, other.code, _, _ = cs[2].scan("1002") }()
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

	expect(t, "reselect", e.call("PUT", "/api/teams/me/state/selection", cs[3].p, map[string]string{"shape": "square"}), http.StatusOK, "")
	for _, c := range cs {
		eventually(t, "every phone sees it cleared", func() bool {
			c.mu.Lock()
			defer c.mu.Unlock()
			return !c.poisoned && len(c.stack) == 0
		})
	}
}
