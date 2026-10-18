package tests

// Simulated teams: each player is a client with its own network delay, its
// own event stream, and the frontend's countdown rule (scanGate, mirrored from
// frontend/src/lib/game/scanGate.ts):
//
//   - the countdown starts when the client first sees a non-empty stack: its
//     own 200 for the first scan, or a teammate's scan arriving as a
//     team_state event
//   - it resets whenever the client sees an empty stack (selection,
//     completion)
//   - once game.scan_window has passed, the client sends no more scans
//
// The server allows game.scan_window + game.scan_grace, bottom to top. A
// client's countdown starts after the server accepted the bottom scan, so a
// scan sent before the client's deadline reaches the server at most
// window + (response delay + request delay) after the bottom scan. These tests
// check that any round trip up to the grace is safe, and show what happens
// beyond it.
//
// The window is shrunk to simWindow (+ simGrace) so the tests take seconds.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync"
	"testing"
	"time"
)

const (
	// simWindow is game.scan_window for the simulations.
	simWindow = 2 * time.Second
	// simGrace is game.scan_grace for the simulations.
	simGrace = time.Second
)

// simSquare is a square in the 2025 seed.
var simSquare = []string{"1001", "1002", "1007", "1008"}

// errGateClosed means the client's countdown ran out, so it didn't send.
var errGateClosed = errors.New("countdown over: select again")

// scanGate is the frontend's countdown rule (scanGate.ts), in Go.
type scanGate struct {
	window  time.Duration
	started time.Time // zero: no countdown running
}

// observe updates the gate with a stack size the client just learned of.
func (g *scanGate) observe(stackSize int, now time.Time) {
	switch {
	case stackSize == 0:
		g.started = time.Time{}
	case g.started.IsZero():
		g.started = now
	}
}

// canScan reports whether the client may send a scan now.
func (g *scanGate) canScan(now time.Time) bool {
	return g.started.IsZero() || now.Sub(g.started) < g.window
}

// deadline is when the running countdown ends; zero if none is running.
func (g *scanGate) deadline() time.Time {
	if g.started.IsZero() {
		return time.Time{}
	}
	return g.started.Add(g.window)
}

// simClient is one player's phone: a network delay each way, an event
// stream delivered after the same delay, and a scan gate.
type simClient struct {
	e        *env
	p        player
	up, down time.Duration // request and response delays

	mu    sync.Mutex
	gate  scanGate
	stack []any    // last stack seen
	shape []string // completed shapes last seen
	sent  int      // scan requests actually sent
}

// newSimClient connects p's event stream with delay down, and returns its
// client.
func (e *env) newSimClient(p player, up, down time.Duration) *simClient {
	c := &simClient{e: e, p: p, up: up, down: down, gate: scanGate{window: simWindow}}
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
	c.shape = nil
	for _, s := range state["shapes"].([]any) {
		c.shape = append(c.shape, s.(string))
	}
	c.gate.observe(len(c.stack), time.Now())
}

// scan sends a scan unless the gate is closed, with the client's delays, and
// returns the status, error code and outcome.
func (c *simClient) scan(waypoint string) (status int, code, outcome string, err error) {
	c.mu.Lock()
	if !c.gate.canScan(time.Now()) {
		c.mu.Unlock()
		return 0, "", "", errGateClosed
	}
	c.sent++
	c.mu.Unlock()

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

// deadline is the client's countdown end; zero if none is running.
func (c *simClient) deadline() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.gate.deadline()
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

// simTeam starts a team on the 2025 seed with the simulation's window and
// grace, gives every player a client (delays from delay(i)), and selects
// shape.
func simTeam(t *testing.T, shape string, delay func(i int) (up, down time.Duration)) (*env, []*simClient) {
	t.Helper()
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e.cfg.Settings.Game.ScanWindow.Duration = simWindow
	e.cfg.Settings.Game.ScanGrace.Duration = simGrace
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
// each with its own delay (up to 150 ms each way): exactly one scan completes
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
			return len(c.stack) == 0 && len(c.shape) == 1 && c.shape[0] == "square" && c.gate.started.IsZero()
		})
	}
}

// TestSimLastScanAtDeadlineWithinGrace has the last player on a slow network
// (300 ms each way, a 600 ms round trip, under the 1 s grace) send at the very
// end of their own countdown, which started when the first scan reached them
// over the event stream. The server still accepts it and the square completes.
func TestSimLastScanAtDeadlineWithinGrace(t *testing.T) {
	slow := 300 * time.Millisecond
	_, cs := simTeam(t, "square", func(i int) (time.Duration, time.Duration) {
		if i == 3 {
			return slow, slow
		}
		return 0, 0
	})
	for i, w := range simSquare[:3] {
		if status, code, _, err := cs[i].scan(w); err != nil || status != http.StatusOK {
			t.Fatalf("scan %s: %d %s %v", w, status, code, err)
		}
	}
	last := cs[3]
	eventually(t, "slow player's countdown starts", func() bool { return !last.deadline().IsZero() })
	time.Sleep(time.Until(last.deadline().Add(-20 * time.Millisecond)))

	status, code, outcome, err := last.scan(simSquare[3])
	if err != nil || status != http.StatusOK || outcome != "pattern_completed" {
		t.Fatalf("last scan at the deadline: %d %s %q %v", status, code, outcome, err)
	}
}

// TestSimLatencyBeyondGraceLosesBottomScan shows the limit: with a 1.4 s round
// trip (over the 1 s grace), a scan sent at the end of the slow player's
// countdown arrives after the first scan went stale. The server drops that
// scan instead of completing, so the team must rescan it or select again.
func TestSimLatencyBeyondGraceLosesBottomScan(t *testing.T) {
	slow := 700 * time.Millisecond
	_, cs := simTeam(t, "square", func(i int) (time.Duration, time.Duration) {
		if i == 3 {
			return slow, slow
		}
		return 0, 0
	})
	for i, w := range simSquare[:3] {
		cs[i].scan(w)
	}
	last := cs[3]
	eventually(t, "slow player's countdown starts", func() bool { return !last.deadline().IsZero() })
	time.Sleep(time.Until(last.deadline().Add(-20 * time.Millisecond)))

	status, _, outcome, err := last.scan(simSquare[3])
	if err != nil || status != http.StatusOK || outcome != "scan_added" {
		t.Fatalf("late scan: %d %q %v; want 200 scan_added (bottom scan dropped)", status, outcome, err)
	}
}

// TestSimGateClosesThenReselectOpens checks that once a player's countdown
// runs out their client sends nothing, and that selecting again resets every
// player's countdown so scanning works.
func TestSimGateClosesThenReselectOpens(t *testing.T) {
	e, cs := simTeam(t, "square", func(int) (time.Duration, time.Duration) { return 20 * time.Millisecond, 20 * time.Millisecond })
	cs[0].scan(simSquare[0])
	eventually(t, "every countdown starts", func() bool {
		for _, c := range cs {
			if c.deadline().IsZero() {
				return false
			}
		}
		return true
	})
	time.Sleep(time.Until(cs[1].deadline()) + 10*time.Millisecond)

	if _, _, _, err := cs[1].scan(simSquare[1]); !errors.Is(err, errGateClosed) {
		t.Fatalf("after the countdown: %v, want errGateClosed", err)
	}
	if cs[1].sent != 0 {
		t.Fatalf("a closed gate sent %d requests", cs[1].sent)
	}

	expect(t, "reselect", e.call("PUT", "/api/teams/me/state/selection", cs[2].p, map[string]string{"shape": "square"}), http.StatusOK, "")
	eventually(t, "every countdown resets", func() bool {
		for _, c := range cs {
			if !c.deadline().IsZero() {
				return false
			}
		}
		return true
	})
	for i, w := range simSquare {
		status, code, _, err := cs[i].scan(w)
		if err != nil || status != http.StatusOK {
			t.Fatalf("after reselect, scan %s: %d %s %v", w, status, code, err)
		}
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
