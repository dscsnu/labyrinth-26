package tests

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"labyrinth/internal/auth"
)

// sseEvent is one parsed server-sent event.
type sseEvent struct {
	name string
	data map[string]any
}

// stream is an open event stream being read in the background. Events are
// routed by name: hello, team_state, and the rest (team, no_team).
type stream struct {
	hellos chan sseEvent // hello
	events chan sseEvent // team / no_team
	states chan sseEvent // team_state
	cancel context.CancelFunc

	mu    sync.Mutex
	names []string // every event's name, in arrival order; guarded by mu
}

// seen returns the names of every event received so far, in order, across
// all channels.
func (s *stream) seen() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.names)
}

// openStream connects p to the event stream with its Bearer token and parses
// events in the background.
func (e *env) openStream(p player) *stream {
	e.t.Helper()
	return e.openStreamAt("/api/events", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.token) })
}

// openStreamAt connects to the event stream at path, letting auth add the
// credentials, and parses events in the background.
func (e *env) openStreamAt(path string, auth func(*http.Request)) *stream {
	e.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	e.t.Cleanup(cancel)

	req, _ := http.NewRequestWithContext(ctx, "GET", e.backend.URL+path, nil)
	auth(req)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/event-stream" {
		e.t.Fatalf("stream: %d %s", res.StatusCode, res.Header.Get("Content-Type"))
	}

	s := &stream{hellos: make(chan sseEvent, 64), events: make(chan sseEvent, 64), states: make(chan sseEvent, 64), cancel: cancel}
	go func() {
		defer close(s.hellos)
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
				s.mu.Lock()
				s.names = append(s.names, name)
				s.mu.Unlock()
				ev := sseEvent{name: name, data: data}
				switch name {
				case "hello":
					s.hellos <- ev
				case "team_state":
					s.states <- ev
				default:
					s.events <- ev
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

	for _, path := range []string{"/api/events", "/api/teams/me/events"} {
		req, _ := http.NewRequest("GET", e.backend.URL+path, nil)
		if res := e.do(req); res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s, no token: %d", path, res.StatusCode)
		}
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

// nextState returns the next team_state event, failing after 3 s.
func (s *stream) nextState(t *testing.T, what string) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.states:
		if !ok {
			t.Fatalf("%s: stream closed", what)
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: no team_state within 3s", what)
	}
	return sseEvent{}
}

// TestStartPublishesTeamState checks that when the leader starts, every
// member's open stream gets the IN_GAME team and then the team's empty
// team_state, in that order, without reconnecting.
func TestStartPublishesTeamState(t *testing.T) {
	e := setup(t)
	teamID, ps := e.fullTeam("start")
	for _, p := range ps[1:] {
		expect(t, "ready", e.call("PUT", "/api/teams/me/ready", p, map[string]bool{"ready": true}), http.StatusOK, "")
	}

	streams := make([]*stream, len(ps))
	for i, p := range ps {
		streams[i] = e.openStream(p)
		expectTeam(t, "initial team", streams[i].next(t, "initial team"), len(ps))
		streams[i].nextState(t, "initial team_state")
	}

	expect(t, "start", e.call("POST", "/api/teams/me/start", ps[0], nil), http.StatusOK, "")
	for i, s := range streams {
		if team := expectTeam(t, "started team", s.next(t, "started team"), len(ps)); team["status"] != "IN_GAME" {
			t.Fatalf("member %d: team status %v, want IN_GAME", i, team["status"])
		}
		st := s.nextState(t, "team_state after start")
		if st.data["team_id"] != teamID || len(st.data["waypoints"].([]any)) != 0 || len(st.data["stack"].([]any)) != 0 || st.data["selection"] != nil {
			t.Fatalf("member %d: team_state after start %v, want empty", i, st.data)
		}
		want := []string{"hello", "team", "team_state", "team", "team_state"}
		if got := s.seen(); !slices.Equal(got, want) {
			t.Fatalf("member %d: events %v, want %v", i, got, want)
		}
	}
}

// TestStreamStartsWithHello checks that every stream opens with hello, whose
// server_time is the server's clock, before the user's own snapshot.
func TestStreamStartsWithHello(t *testing.T) {
	e := setup(t)
	before := time.Now()
	s := e.openStream(e.newPlayer("p"))
	expectNoTeam(t, "initial", s.next(t, "initial"))
	h := <-s.hellos
	at, err := time.Parse(time.RFC3339Nano, h.data["server_time"].(string))
	if err != nil || at.Before(before.Add(-time.Second)) || at.After(time.Now().Add(time.Second)) {
		t.Fatalf("hello server_time %v (%v), want about now", h.data["server_time"], err)
	}
	if got := s.seen(); !slices.Equal(got, []string{"hello", "no_team"}) {
		t.Fatalf("events %v, want [hello no_team]", got)
	}
}

// TestStreamCookieAndAlias checks that /api/events accepts the token cookie
// a browser's EventSource sends, that no other route does, and that the
// deprecated /api/teams/me/events alias serves the same stream with Bearer.
func TestStreamCookieAndAlias(t *testing.T) {
	e := setup(t)
	p := e.newPlayer("p")
	cookie := &http.Cookie{Name: auth.TokenCookie, Value: p.token}

	s := e.openStreamAt("/api/events", func(r *http.Request) { r.AddCookie(cookie) })
	expectNoTeam(t, "cookie stream", s.next(t, "cookie stream"))

	req, _ := http.NewRequest("GET", e.backend.URL+"/api/teams/me", nil)
	req.AddCookie(cookie)
	if res := e.do(req); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/teams/me with only the cookie: %d, want 401", res.StatusCode)
	}
	req, _ = http.NewRequest("GET", e.backend.URL+"/api/teams/me/events", nil)
	req.AddCookie(cookie)
	if res := e.do(req); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("alias with only the cookie: %d, want 401", res.StatusCode)
	}
	req, _ = http.NewRequest("GET", e.backend.URL+"/api/events", nil)
	req.AddCookie(&http.Cookie{Name: auth.TokenCookie, Value: "not-a-token"})
	if res := e.do(req); res.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad cookie: %d, want 401", res.StatusCode)
	}

	alias := e.openStreamAt("/api/teams/me/events", func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.token) })
	expectNoTeam(t, "alias stream", alias.next(t, "alias stream"))
	if got := alias.seen(); !slices.Equal(got, []string{"hello", "no_team"}) {
		t.Fatalf("alias events %v, want [hello no_team]", got)
	}
}

// TestCastPublishesDamageOverEventStream checks that after a completing scan,
// team_state.last_cast arrives over the event stream with its positive damage value.
func TestCastPublishesDamageOverEventStream(t *testing.T) {
	e := setup(t)
	if err := e.db.SeedDev2025(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart()

	_, ps := e.fullTeam("cast-damage")
	for _, p := range ps[1:] {
		expect(t, "ready", e.call("PUT", "/api/teams/me/ready", p, map[string]bool{"ready": true}), http.StatusOK, "")
	}
	expect(t, "start", e.call("POST", "/api/teams/me/start", ps[0], nil), http.StatusOK, "")

	s := e.openStream(ps[0])
	expectTeam(t, "initial team", s.next(t, "initial team"), len(ps))
	s.nextState(t, "initial team_state")

	expect(t, "select collinear", e.call("PUT", "/api/teams/me/state/selection", ps[0], map[string]string{"shape": "collinear"}), http.StatusOK, "")
	s.nextState(t, "selection state")

	// Pattern 1 has waypoints 1001, 1003, 1004.
	scan := func(p player, waypoint string) {
		r := e.call("POST", "/api/teams/me/state/scans", p, map[string]string{"waypoint": waypoint})
		expect(t, "scan "+waypoint, r, http.StatusOK, "")
	}
	scan(ps[0], "1001")
	s.nextState(t, "first scan")
	scan(ps[1], "1003")
	s.nextState(t, "second scan")
	scan(ps[2], "1004") // completes pattern 1

	completedState := s.nextState(t, "cast completed state")
	lastCast, ok := completedState.data["last_cast"].(map[string]any)
	if !ok || lastCast == nil {
		t.Fatalf("expected last_cast in team_state, got %v", completedState.data["last_cast"])
	}
	damage, ok := lastCast["damage"].(float64)
	if !ok || damage <= 0 {
		t.Fatalf("expected positive damage in last_cast, got %v", lastCast["damage"])
	}
	if int(damage) != 65 {
		t.Fatalf("expected damage 65 for pattern 1, got %v", damage)
	}
}
