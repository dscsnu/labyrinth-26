package tests

import (
	"context"
	"net/http"
	"testing"
	"time"
)

// nextAdmin returns the next activity or admin_leaderboard event, failing
// after 3 s.
func (s *stream) nextAdmin(t *testing.T, what string) sseEvent {
	t.Helper()
	select {
	case ev, ok := <-s.admin:
		if !ok {
			t.Fatalf("%s: stream closed", what)
		}
		return ev
	case <-time.After(3 * time.Second):
		t.Fatalf("%s: no admin event within 3s", what)
	}
	return sseEvent{}
}

// noAdmin fails if s gets an activity or admin_leaderboard event within
// 300 ms.
func (s *stream) noAdmin(t *testing.T, what string) {
	t.Helper()
	select {
	case ev := <-s.admin:
		t.Fatalf("%s: got %s:%s", what, ev.name, ev.typ)
	case <-time.After(300 * time.Millisecond):
	}
}

// TestAdminLiveView checks the admin live view over HTTP and real streams:
// an admin's ?admin=1 stream opens with the activity and full-leaderboard
// snapshots, then gets one activity batch per flush with the team's
// selection, scans and cast (who did each, with the cast's damage), and
// the full ranking when it changes; an admin's plain stream, and a
// player's with or without ?admin=1, get none of it.
func TestAdminLiveView(t *testing.T) {
	e := setup(t)
	if err := e.db.Seed2026(context.Background()); err != nil {
		t.Fatal(err)
	}
	e = e.restart() // content is read at startup
	admin := e.newAdmin("admin")
	ps := e.startedTeam("lv")
	e.startedTeam("lw")
	bearer := func(p player) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+p.token) }
	}

	live := e.openStreamAt("/api/events?admin=1", bearer(admin))
	plain := e.openStream(admin)
	player := e.openStreamAt("/api/events?admin=1", bearer(ps[1]))
	if ev := live.nextAdmin(t, "activity snapshot"); ev.name != "activity" || ev.typ != "snapshot" {
		t.Fatalf("first admin event: %s:%s %v", ev.name, ev.typ, ev.data)
	}
	ev := live.nextAdmin(t, "leaderboard snapshot")
	if ev.name != "admin_leaderboard" || ev.typ != "snapshot" || len(ev.data["teams"].([]any)) != 2 {
		t.Fatalf("full leaderboard snapshot: %s:%s %v", ev.name, ev.typ, ev.data)
	}

	e.castOver(ps[0], "quadrilateral", []string{"2001", "2002", "2007", "2008"})
	if !e.app.Activity.Flush() {
		t.Fatal("the flush after a cast sent nothing")
	}
	ev = live.nextAdmin(t, "activity batch")
	if ev.name != "activity" || ev.typ != "batch" || ev.data["dropped"] != float64(0) {
		t.Fatalf("batch: %s:%s %v", ev.name, ev.typ, ev.data)
	}
	// Both teams' starts are in it too (game_started), queued since then.
	var entries []any
	starts := 0
	for _, x := range ev.data["entries"].([]any) {
		if x.(map[string]any)["type"] == "game_started" {
			starts++
			continue
		}
		entries = append(entries, x)
	}
	if starts != 2 || len(entries) != 5 {
		t.Fatalf("batch entries (%d starts): %v", starts, ev.data["entries"])
	}
	first, last := entries[0].(map[string]any), entries[4].(map[string]any)
	if first["type"] != "shape_selected" || first["shape"] != "quadrilateral" || first["by"] != "lv0" || first["team_id"] == nil {
		t.Fatalf("selection entry: %v", first)
	}
	if scan := entries[1].(map[string]any); scan["type"] != "scan_added" || scan["waypoint"] != "2001" || scan["by"] != "lv0" {
		t.Fatalf("scan entry: %v", scan)
	}
	if (last["type"] != "spell_cast" && last["type"] != "boss_defeated") || last["shape"] != "quadrilateral" || last["dealt"] == nil {
		t.Fatalf("cast entry: %v", last)
	}
	if e.app.Activity.Flush() {
		t.Fatal("a flush with nothing new sent a batch")
	}

	e.app.Leaderboard.Refresh()
	ev = live.nextAdmin(t, "full leaderboard update")
	teams := ev.data["teams"].([]any)
	if ev.name != "admin_leaderboard" || ev.typ != "updated" || len(teams) != 2 || teams[0].(map[string]any)["team_id"] != first["team_id"] {
		t.Fatalf("full leaderboard: %s:%s %v", ev.name, ev.typ, ev.data)
	}

	plain.noAdmin(t, "an admin's plain stream")
	player.noAdmin(t, "a player's ?admin=1 stream")
	expect(t, "players still play", e.call("GET", "/api/teams/me/state", ps[1], nil), http.StatusOK, "")
}
