package tests

import (
	"slices"
	"testing"

	"labyrinth/internal/teamstate"
)

// TestStack checks the stack: room for a pattern's points plus one wrong
// scan, bottom-to-top order, Push leaving the original unchanged, and a push
// onto a full stack panicking (Scan never does it).
func TestStack(t *testing.T) {
	waypoints := func(s teamstate.Stack) []int {
		var out []int
		for i := range s.Len() {
			out = append(out, s.At(i).Waypoint)
		}
		return out
	}

	s := teamstate.NewStack(3) // a 3-point pattern + 1 wrong scan
	if s.Cap != 4 {
		t.Fatalf("cap %d, want 4", s.Cap)
	}
	for w := range 3 {
		s = s.Push(teamstate.StackScan{Waypoint: w})
	}
	before := s
	s = s.Push(teamstate.StackScan{Waypoint: 9, Wrong: true})
	if got := waypoints(s); !slices.Equal(got, []int{0, 1, 2, 9}) || !s.At(3).Wrong {
		t.Fatalf("stack %v", got)
	}
	if got := waypoints(before); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("Push changed the original: %v", got)
	}

	defer func() {
		if recover() == nil {
			t.Fatal("pushing onto a full stack should panic")
		}
	}()
	s.Push(teamstate.StackScan{Waypoint: 10})
}
