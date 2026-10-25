package tests

import (
	"sync"
	"testing"

	"labyrinth/internal/platform/live"
	"labyrinth/internal/teamstate"
)

// TestOnPublishSeesEveryPublish checks Rules.OnPublish (the admin live
// view's hook): it's called once for each team_state the members get, with
// that publish's cause and the state sent: here a selection, three scans
// and the completing scan's cast.
func TestOnPublishSeesEveryPublish(t *testing.T) {
	var mu sync.Mutex
	var got []live.EventType
	var last teamstate.View
	f := started(t, fixtureOptions{rules: func(r *teamstate.Rules) {
		r.OnPublish = func(teamID string, typ live.EventType, v teamstate.View) {
			mu.Lock()
			defer mu.Unlock()
			got, last = append(got, typ), v
		}
	}})
	mu.Lock()
	got = nil // the start's publish, before this test's actions
	mu.Unlock()
	res := f.castQuad(t, 0)

	mu.Lock()
	defer mu.Unlock()
	cast := teamstate.SpellCast
	if res.State.LastCast != nil && res.State.LastCast.DefeatedBoss {
		cast = teamstate.BossDefeated // the seed's boss 1 falls to one quadrilateral
	}
	want := []live.EventType{teamstate.ShapeSelected, teamstate.ScanAdded, teamstate.ScanAdded, teamstate.ScanAdded, cast}
	if len(got) != len(want) {
		t.Fatalf("publishes %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("publish %d: %s, want %s (all: %v)", i, got[i], want[i], got)
		}
	}
	if last.LastCast == nil || last.LastCast.Shape != "quadrilateral" || last.Version != res.State.Version {
		t.Fatalf("the cast's state: %+v", last.LastCast)
	}
}
