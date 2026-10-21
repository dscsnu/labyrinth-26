package tests

import (
	"runtime"
	"testing"

	"labyrinth/internal/platform/stats"
)

// TestCollect checks that Collect returns a populated, sane snapshot: the
// runtime figures are always set, and on Linux so are the /proc ones.
func TestCollect(t *testing.T) {
	s := stats.Collect()
	if s.GoVersion == "" {
		t.Error("GoVersion is empty")
	}
	if s.Goroutines < 1 {
		t.Errorf("Goroutines = %d, want at least 1", s.Goroutines)
	}
	if s.GOMAXPROCS < 1 {
		t.Errorf("GOMAXPROCS = %d, want at least 1", s.GOMAXPROCS)
	}
	if s.NumCPU < 1 {
		t.Errorf("NumCPU = %d, want at least 1", s.NumCPU)
	}
	if s.Memory.Alloc == 0 || s.Memory.Sys == 0 {
		t.Errorf("runtime memory not reported: %+v", s.Memory)
	}
	if runtime.GOOS != "linux" {
		return
	}
	if s.Memory.RSS == 0 {
		t.Error("RSS = 0 on Linux, want the process's resident memory")
	}
	if s.CPU.ProcessSeconds < 0 {
		t.Errorf("ProcessSeconds = %v, want >= 0", s.CPU.ProcessSeconds)
	}
	if s.CPU.LoadAverage.One < 0 || s.CPU.LoadAverage.Five < 0 || s.CPU.LoadAverage.Fifteen < 0 {
		t.Errorf("negative load average: %+v", s.CPU.LoadAverage)
	}
}
