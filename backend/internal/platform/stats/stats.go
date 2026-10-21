package stats

import (
	"bufio"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// clkTck is the clock ticks per second Linux reports process CPU time in
// (USER_HZ). It's the standard user clock, not a tunable.
const clkTck = 100

// LoadAverage is the host's one-, five- and fifteen-minute load averages,
// from /proc/loadavg. All three are zero on platforms without it (or if it
// can't be read).
type LoadAverage struct {
	One     float64 `json:"1"`
	Five    float64 `json:"5"`
	Fifteen float64 `json:"15"`
}

// CPU is the process's total CPU time so far and the host's load average.
type CPU struct {
	// ProcessSeconds is the process's user plus system CPU time in seconds.
	ProcessSeconds float64 `json:"process_seconds"`
	// LoadAverage is the whole host's load, not the process's share.
	LoadAverage LoadAverage `json:"load_average"`
}

// Memory is the Go heap as the runtime sees it, plus the process's resident
// set size. Every field is in bytes.
type Memory struct {
	// Alloc is bytes allocated and still in use by the runtime.
	Alloc uint64 `json:"alloc"`
	// TotalAlloc is bytes allocated over the process's life, freed or not.
	TotalAlloc uint64 `json:"total_alloc"`
	// Sys is bytes the runtime has obtained from the OS.
	Sys uint64 `json:"sys"`
	// HeapAlloc is Alloc within the heap; HeapObjects the live object count.
	HeapAlloc   uint64 `json:"heap_alloc"`
	HeapObjects uint64 `json:"heap_objects"`
	// NumGC is the number of completed garbage collections.
	NumGC uint32 `json:"num_gc"`
	// GCPauseTotal is the total time spent paused for GC, in nanoseconds.
	GCPauseTotal uint64 `json:"gc_pause_total_ns"`
	// RSS is the process's resident set size from /proc (0 where unknown).
	RSS uint64 `json:"rss"`
}

// Snapshot is everything the health route reports about this process: the
// runtime's own view and, on Linux, the OS's view from /proc.
type Snapshot struct {
	GoVersion  string `json:"go_version"`
	Goroutines int    `json:"goroutines"`
	GOMAXPROCS int    `json:"gomaxprocs"`
	NumCPU     int    `json:"num_cpu"`
	CPU        CPU    `json:"cpu"`
	Memory     Memory `json:"memory"`
}

// Collect returns the current process and runtime snapshot.
func Collect() Snapshot {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Snapshot{
		GoVersion:  runtime.Version(),
		Goroutines: runtime.NumGoroutine(),
		GOMAXPROCS: runtime.GOMAXPROCS(0),
		NumCPU:     runtime.NumCPU(),
		CPU: CPU{
			ProcessSeconds: processSeconds(),
			LoadAverage:    loadAverage(),
		},
		Memory: Memory{
			Alloc:        m.Alloc,
			TotalAlloc:   m.TotalAlloc,
			Sys:          m.Sys,
			HeapAlloc:    m.HeapAlloc,
			HeapObjects:  m.HeapObjects,
			NumGC:        m.NumGC,
			GCPauseTotal: m.PauseTotalNs,
			RSS:          rssBytes(),
		},
	}
}

// processSeconds returns the process's user plus system CPU time in seconds,
// from /proc/self/stat. Zero on non-Linux or if it can't be read.
func processSeconds() float64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	b, err := os.ReadFile("/proc/self/stat")
	if err != nil {
		return 0
	}
	// The fields after the ")" that closes the process name; utime and stime
	// are the 14th and 15th fields overall, so the 12th and 13th here.
	rest := string(b[strings.LastIndexByte(string(b), ')')+2:])
	f := strings.Fields(rest)
	if len(f) < 13 {
		return 0
	}
	ut, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	return float64(ut+st) / clkTck
}

// rssBytes returns the process's resident set size in bytes, from
// /proc/self/status. Zero on non-Linux or if it can't be read.
func rssBytes() uint64 {
	if runtime.GOOS != "linux" {
		return 0
	}
	f, err := os.Open("/proc/self/status")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		if k != "VmRSS" {
			continue
		}
		kb, _ := strconv.ParseFloat(strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB")), 64)
		return uint64(kb * 1024)
	}
	return 0
}

// loadAverage returns the host's 1-, 5- and 15-minute load averages, from
// /proc/loadavg. All zero on non-Linux or if it can't be read.
func loadAverage() LoadAverage {
	if runtime.GOOS != "linux" {
		return LoadAverage{}
	}
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return LoadAverage{}
	}
	f := strings.Fields(string(b))
	if len(f) < 3 {
		return LoadAverage{}
	}
	one, _ := strconv.ParseFloat(f[0], 64)
	five, _ := strconv.ParseFloat(f[1], 64)
	fifteen, _ := strconv.ParseFloat(f[2], 64)
	return LoadAverage{One: one, Five: five, Fifteen: fifteen}
}
