package tests

import (
	"slices"
	"testing"
	"time"

	"labyrinth/internal/teamstate"
)

// TestStack checks the stack: bottom-to-top order, dropping the bottom when
// full, Push leaving the original unchanged, Live leaving out stale scans, and
// a wrong scan never going stale.
func TestStack(t *testing.T) {
	base := time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)
	waypoints := func(scans []teamstate.StackScan) []int {
		var out []int
		for _, sc := range scans {
			out = append(out, sc.Waypoint)
		}
		return out
	}
	all := func(s teamstate.Stack) []teamstate.StackScan {
		var out []teamstate.StackScan
		for i := range s.Len() {
			out = append(out, s.At(i))
		}
		return out
	}

	s := teamstate.NewStack(3)
	for w := range 3 {
		s = s.Push(teamstate.StackScan{Waypoint: w, At: base.Add(time.Duration(w) * 10 * time.Second)})
	}
	if got := waypoints(all(s)); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("full stack %v", got)
	}
	before := s
	s = s.Push(teamstate.StackScan{Waypoint: 3, At: base.Add(30 * time.Second)})
	if got := waypoints(all(s)); !slices.Equal(got, []int{1, 2, 3}) || s.Len() != 3 {
		t.Fatalf("after overflow %v", got)
	}
	if got := waypoints(all(before)); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("Push changed the original: %v", got)
	}

	// At 45 s with a 25 s limit: scans at 10 s (35 s old) are stale, 20 s (25 s
	// old, exactly the limit) and 30 s are live.
	if got := waypoints(s.Live(base.Add(45*time.Second), 25*time.Second)); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("live %v", got)
	}
	if got := s.Live(base.Add(time.Hour), time.Second); len(got) != 0 {
		t.Fatalf("everything should be stale: %v", got)
	}

	// A wrong scan stays live however old it is.
	wrong := teamstate.NewStack(3).Push(teamstate.StackScan{Waypoint: 7, At: base, Wrong: true})
	if got := waypoints(wrong.Live(base.Add(time.Hour), time.Second)); !slices.Equal(got, []int{7}) {
		t.Fatalf("wrong scan should stay live: %v", got)
	}
}
