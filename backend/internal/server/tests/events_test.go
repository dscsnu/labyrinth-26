package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"
)

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	name string
	data map[string]any
}

// stream is an open event stream being read in the background.
type stream struct {
	events chan sseEvent // team / no_team
	states chan sseEvent // team_state
	cancel context.CancelFunc
}

// openStream connects p to the team event stream and parses events in the
// background.
func (e *env) openStream(p player) *stream {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)

	req, _ := http.NewRequestWithContext(ctx, "GET", e.backend.URL+"/api/teams/me/events", nil)
	req.Header.Set("Authorization", "Bearer "+p.token)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		e.t.Fatalf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	s := &stream{events: make(chan sseEvent, 64), states: make(chan sseEvent, 64), cancel: cancel}
	go func() {
		defer close(s.events)
		defer close(s.states)
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		var name string
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				var data map[string]any
				json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &data)
				if name == "team_state" {
					s.states <- sseEvent{name: name, data: data}
				} else {
					s.events <- sseEvent{name: name, data: data}
				}
			}
		}
	}()
	return s
}

// next returns the next team or no_team event, failing after 3 s.
func (s *stream) next(t *testing.T, what string) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.events:
		if !ok {
			t.Fatalf("%s: stream closed", what)
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: no event within 3s", what)
	}
	return sseEvent{}
}

// expectTeam fails unless ev is a team event with that many members, and
// returns its data.
func expectTeam(t *testing.T, what string, ev sseEvent, members int) map[string]any {
	t.Helper()
	if ev.name != "team" {
		t.Fatalf("%s: got %q event, want team", what, ev.name)
	}
	if got := len(ev.data["members"].([]any)); got != members {
		t.Fatalf("%s: %d members, want %d", what, got, members)
	}
	return ev.data
}

// expectNoTeam fails unless ev is a no_team event.
func expectNoTeam(t *testing.T, what string, ev sseEvent) {
	t.Helper()
	if ev.name != "no_team" {
		t.Fatalf("%s: got %q event, want no_team", what, ev.name)
	}
}

// memberReady reports whether member id is ready in team.
func memberReady(team map[string]any, id string) bool {
	for _, m := range team["members"].([]any) {
		if m := m.(map[string]any); m["id"] == id {
			return m["is_ready"].(bool)
		}
	}
	return false
}

// TestTeamEventStream checks that teammates see joins, readies, a leadership
// handover and deletion live, and that a stream follows its user to a new
// team.
func TestTeamEventStream(t *testing.T) {
	e := setup(t)
	a, b := e.newPlayer("a"), e.newPlayer("b")

	teamID := e.call("POST", "/api/teams", a, map[string]string{"name": "Live"}).payload["id"].(string)

	sa, sb := e.openStream(a), e.openStream(b)
	expectTeam(t, "a's initial state", sa.next(t, "a initial"), 1)
	expectNoTeam(t, "b's initial state", sb.next(t, "b initial"))

	e.call("POST", "/api/teams/join", b, map[string]string{"team_id": teamID})
	expectTeam(t, "a sees b join", sa.next(t, "a: join"), 2)
	expectTeam(t, "b sees own join", sb.next(t, "b: join"), 2)

	e.call("PUT", "/api/teams/me/ready", b, map[string]bool{"ready": true})
	if team := expectTeam(t, "a sees ready", sa.next(t, "a: ready"), 2); !memberReady(team, b.id) {
		t.Fatal("a's event doesn't show b ready")
	}
	sb.next(t, "b: ready")

	e.call("POST", "/api/teams/leave", a, nil)
	expectNoTeam(t, "a after leaving", sa.next(t, "a: leave"))
	if team := expectTeam(t, "b after leader left", sb.next(t, "b: leader left"), 1); team["leader_id"] != b.id {
		t.Fatalf("b should be leader now, got %v", team["leader_id"])
	}

	e.call("POST", "/api/teams/leave", b, nil)
	expectNoTeam(t, "b after team deleted", sb.next(t, "b: team deleted"))

	// The stream follows the user into a new team.
	e.call("POST", "/api/teams", b, map[string]string{"name": "Second"})
	if team := expectTeam(t, "b's new team", sb.next(t, "b: new team"), 1); team["name"] != "Second" {
		t.Fatalf("got team %v", team["name"])
	}

	// a's stream is still open and unaffected by b's new team.
	select {
	case ev := <-sa.events:
		t.Fatalf("a got unrelated event %v", ev)
	case <-time.After(100 * time.Millisecond):
	}
}

// TestEventStreamAuthAndCleanup checks that the stream needs a token and that
// a disconnected stream is cleaned up.
func TestEventStreamAuthAndCleanup(t *testing.T) {
	e := setup(t)

	req, _ := http.NewRequest("GET", e.backend.URL+"/api/teams/me/events", nil)
	if res := e.do(req); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: %d", res.StatusCode)
	}

	streams := func() float64 {
		res := e.get("/healthz")
		var body struct {
			Payload struct {
				Streams float64 `json:"streams"`
			} `json:"payload"`
		}
		json.NewDecoder(res.Body).Decode(&body)
		return body.Payload.Streams
	}

	s := e.openStream(e.newPlayer("p"))
	s.next(t, "initial")
	if n := streams(); n != 1 {
		t.Fatalf("streams = %v, want 1", n)
	}

	s.cancel()
	deadline := time.Now().Add(2 * time.Second)
	for streams() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("disconnected stream was not cleaned up")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestStreamIncludesTeamState checks that a user in a team gets its
// team_state right after the team on connect, and GET /api/teams/me/state.
func TestStreamIncludesTeamState(t *testing.T) {
	e := setup(t)
	a := e.newPlayer("a")
	teamID := e.call("POST", "/api/teams", a, map[string]string{"name": "State"}).payload["id"].(string)

	s := e.openStream(a)
	expectTeam(t, "initial team", s.next(t, "team"), 1)
	select {
	case ev := <-s.states:
		if ev.data["team_id"] != teamID || len(ev.data["waypoints"].([]any)) != 0 {
			t.Fatalf("initial team_state: %v", ev.data)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("no initial team_state")
	}

	r := e.call("GET", "/api/teams/me/state", a, nil)
	expect(t, "GET state", r, http.StatusOK, "")
	if r.payload["team_id"] != teamID {
		t.Fatalf("GET /api/teams/me/state: %v", r.payload)
	}
	expect(t, "GET state, no team", e.call("GET", "/api/teams/me/state", e.newPlayer("b"), nil), http.StatusNotFound, "not_in_team")
}
