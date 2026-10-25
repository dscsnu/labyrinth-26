package tests

import (
	"encoding/json"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/activity"
	"labyrinth/internal/platform/live"
	"labyrinth/internal/teams"
	"labyrinth/internal/teamstate"
)

// fakeTeams is an activity.Teams holding one team.
type fakeTeams struct{ team teams.Team }

// ByID returns the team if the ID matches.
func (f fakeTeams) ByID(id string) (teams.Team, bool) { return f.team, id == f.team.ID }

// at is the feed's fixed clock.
var at = time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)

// fixture is a feed over one team of two, Asha and Ravi, with an admin's
// stream watching.
type fixture struct {
	feed       *activity.Feed
	asha, ravi uuid.UUID
	watch      *live.Subscription
	hub        *live.Hub
}

// newFixture returns a fixture whose queue holds batchMax and recent list
// recent.
func newFixture(t *testing.T, batchMax, recent int) *fixture {
	t.Helper()
	asha, ravi := uuid.New(), uuid.New()
	ft := fakeTeams{team: teams.Team{ID: "482913", Name: "Scanners", Members: []teams.Member{{ID: asha, Name: "Asha"}, {ID: ravi, Name: "Ravi"}}}}
	feed := activity.New(ft, slog.New(slog.NewTextHandler(io.Discard, nil)), activity.Rules{
		PublishEvery: time.Second, BatchMax: batchMax, Recent: recent, Now: func() time.Time { return at },
	})
	hub := live.NewHub(1, 16)
	watch := hub.Subscribe(uuid.New())
	stop := feed.Watch(watch)
	t.Cleanup(stop)
	return &fixture{feed: feed, asha: asha, ravi: ravi, watch: watch, hub: hub}
}

// batch reads the next event on the watching stream as a Batch, failing if
// there's none.
func (f *fixture) batch(t *testing.T) (string, activity.Batch) {
	t.Helper()
	select {
	case ev := <-f.watch.C:
		var env struct {
			Type    string         `json:"type"`
			Message activity.Batch `json:"message"`
		}
		if err := json.Unmarshal(ev.Data(), &env); err != nil || ev.Name() != "activity" {
			t.Fatalf("event %s: %s (%v)", ev.Name(), ev.Data(), err)
		}
		return env.Type, env.Message
	default:
		t.Fatal("no event")
	}
	return "", activity.Batch{}
}

// ip returns a pointer to n.
func ip(n int) *int { return &n }

// TestEntriesByType checks each kind of change becomes the right entry:
// who acted (by name), the shape, waypoint, damage, boss, health, the
// powerup granted then activated (named from what the team held), and a
// quiz answer; a change by the server has no "by"; snapshot and resync are
// ignored.
func TestEntriesByType(t *testing.T) {
	f := newFixture(t, 50, 50)
	rec := func(typ live.EventType, v teamstate.View) { f.feed.Record("482913", typ, v) }
	rec(teamstate.ShapeSelected, teamstate.View{Selection: &teamstate.SelectionView{Shape: "triangle", SelectedBy: f.asha}})
	rec(teamstate.ScanAdded, teamstate.View{Stack: []teamstate.StackScanView{{WaypointID: "2001", UserID: f.asha}, {WaypointID: "2003", UserID: f.ravi}}})
	rec(teamstate.SpellCast, teamstate.View{LastCast: &teamstate.CastView{Shape: "triangle", Dealt: 68, Boss: ip(1), CastBy: f.ravi}})
	rec(teamstate.BossAttacked, teamstate.View{Health: 7, Boss: &teamstate.BossView{Serial: 2}})
	rec(teamstate.PowerupGranted, teamstate.View{Powerups: teamstate.PowerupsView{Held: []string{"heart"}}})
	rec(teamstate.PowerupActivated, teamstate.View{Powerups: teamstate.PowerupsView{Held: []string{}}})
	rec(teamstate.QTEAnswered, teamstate.View{QTE: &teamstate.QTEAnswerView{Correct: true, AnsweredBy: f.asha}})
	rec(live.Snapshot, teamstate.View{})
	rec(live.Resync, teamstate.View{})
	if !f.feed.Flush() {
		t.Fatal("nothing sent")
	}
	typ, b := f.batch(t)
	if typ != "batch" || b.Dropped != 0 || len(b.Entries) != 7 {
		t.Fatalf("%s: %+v", typ, b)
	}
	e := b.Entries
	name := func(i int) string {
		if e[i].By == nil {
			return ""
		}
		return *e[i].By
	}
	switch {
	case e[0].Type != "shape_selected" || e[0].Shape != "triangle" || name(0) != "Asha" || e[0].Team != "Scanners" || !e[0].At.Equal(at):
		t.Fatalf("selection: %+v", e[0])
	case e[1].Type != "scan_added" || e[1].Waypoint != "2003" || name(1) != "Ravi":
		t.Fatalf("scan: %+v", e[1])
	case e[2].Shape != "triangle" || *e[2].Dealt != 68 || *e[2].Boss != 1 || name(2) != "Ravi":
		t.Fatalf("cast: %+v", e[2])
	case *e[3].Health != 7 || *e[3].Boss != 2 || e[3].By != nil:
		t.Fatalf("attack: %+v", e[3])
	case e[4].Powerup != "heart" || e[5].Powerup != "heart":
		t.Fatalf("powerups: %+v %+v", e[4], e[5])
	case !*e[6].Correct || name(6) != "Asha":
		t.Fatalf("quiz: %+v", e[6])
	}
}

// TestBatchingIsBounded checks the queue: a flush with nothing new sends
// nothing; past activity.batch_max the oldest entries are dropped and
// counted, the newest kept in order; the recent list keeps the last
// activity.recent flushed, which a snapshot carries.
func TestBatchingIsBounded(t *testing.T) {
	f := newFixture(t, 3, 4)
	if f.feed.Flush() {
		t.Fatal("an empty flush sent a batch")
	}
	for i := range 5 {
		f.feed.Record("482913", teamstate.ScanAdded, teamstate.View{Stack: []teamstate.StackScanView{{WaypointID: string(rune('0'+i)) + "000", UserID: f.asha}}})
	}
	f.feed.Flush()
	_, b := f.batch(t)
	if b.Dropped != 2 || len(b.Entries) != 3 || b.Entries[0].Waypoint != "2000" || b.Entries[2].Waypoint != "4000" {
		t.Fatalf("capped batch: %+v", b)
	}
	f.feed.Record("482913", teamstate.Revived, teamstate.View{Health: 10})
	f.feed.Record("482913", teamstate.Revived, teamstate.View{Health: 10})
	f.feed.Flush()
	f.batch(t)
	snap := f.feed.Snapshot()
	if len(snap.Entries) != 4 || snap.Entries[0].Waypoint != "3000" || snap.Entries[3].Type != "revived" || snap.Dropped != 0 {
		t.Fatalf("recent: %+v", snap)
	}
}

// TestOnlyWatchersReceive checks that batches and SendAdmins reach the
// watching stream, not the same admin's other stream; after stop nothing
// arrives (the queue still empties).
func TestOnlyWatchersReceive(t *testing.T) {
	f := newFixture(t, 10, 10)
	other := f.hub.Subscribe(uuid.New())
	f.feed.SendAdmins(live.MustNewEvent("admin_leaderboard", "updated", map[string]any{}))
	if ev := <-f.watch.C; ev.Name() != "admin_leaderboard" {
		t.Fatalf("watcher got %s", ev.Name())
	}
	select {
	case ev := <-other.C:
		t.Fatalf("a stream not watching got %s", ev.Name())
	default:
	}
	if f.feed.Watching() != 1 {
		t.Fatalf("watching %d", f.feed.Watching())
	}

	f2 := newFixture(t, 10, 10)
	stop := f2.feed.Watch(f2.watch) // the same stream again, then stopped twice over
	stop()
	if f2.feed.Watching() != 0 {
		t.Fatalf("watching after stop: %d", f2.feed.Watching())
	}
	f2.feed.Record("482913", teamstate.Revived, teamstate.View{Health: 10})
	if f2.feed.Flush() {
		t.Fatal("a flush with nobody watching sent")
	}
}
