package main

import (
	"fmt"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// recorder collects latency samples and errors per operation.
type recorder struct {
	mu      sync.Mutex
	samples map[string][]time.Duration
	errors  map[string]map[string]int // op → "status code" → count
}

// newRecorder returns an empty recorder.
func newRecorder() *recorder {
	return &recorder{samples: map[string][]time.Duration{}, errors: map[string]map[string]int{}}
}

// observe records one successful operation's latency.
func (r *recorder) observe(op string, d time.Duration) {
	r.mu.Lock()
	r.samples[op] = append(r.samples[op], d)
	r.mu.Unlock()
}

// fail records one failed operation and why.
func (r *recorder) fail(op, reason string) {
	r.mu.Lock()
	if r.errors[op] == nil {
		r.errors[op] = map[string]int{}
	}
	r.errors[op][reason]++
	r.mu.Unlock()
}

// opStats summarises one operation.
type opStats struct {
	op                 string
	count, errors      int
	p50, p95, p99, max time.Duration
}

// stats summarises every operation, sorted by name.
func (r *recorder) stats() []opStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	ops := map[string]bool{}
	for op := range r.samples {
		ops[op] = true
	}
	for op := range r.errors {
		ops[op] = true
	}
	var out []opStats
	for op := range ops {
		s := opStats{op: op, count: len(r.samples[op])}
		for _, n := range r.errors[op] {
			s.errors += n
		}
		if s.count > 0 {
			d := slices.Clone(r.samples[op])
			slices.Sort(d)
			pct := func(p float64) time.Duration { return d[min(len(d)-1, int(p*float64(len(d))))] }
			s.p50, s.p95, s.p99, s.max = pct(0.50), pct(0.95), pct(0.99), d[len(d)-1]
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].op < out[j].op })
	return out
}

// total returns overall request count, errors and p99 across HTTP operations
// (SSE delivery samples are excluded: they aren't requests).
func (r *recorder) total() (count, errors int, p99 time.Duration) {
	r.mu.Lock()
	var all []time.Duration
	for op, d := range r.samples {
		if !strings.HasPrefix(op, "sse") {
			all = append(all, d...)
		}
	}
	for op, m := range r.errors {
		if !strings.HasPrefix(op, "sse") {
			for _, n := range m {
				errors += n
			}
		}
	}
	r.mu.Unlock()
	if len(all) == 0 {
		return 0, errors, 0
	}
	slices.Sort(all)
	return len(all), errors, all[min(len(all)-1, int(0.99*float64(len(all))))]
}

// print writes the scenario's table and the backend's resource use.
func (r *recorder) print(title string, elapsed time.Duration, res resourceStats) {
	fmt.Printf("\n== %s (%s)\n", title, elapsed.Round(time.Millisecond))
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "operation\tcount\tper sec\tp50\tp95\tp99\tmax\terrors\t")
	for _, s := range r.stats() {
		fmt.Fprintf(w, "%s\t%d\t%.0f\t%s\t%s\t%s\t%s\t%d\t\n", s.op, s.count,
			float64(s.count)/elapsed.Seconds(), ms(s.p50), ms(s.p95), ms(s.p99), ms(s.max), s.errors)
	}
	w.Flush()
	r.mu.Lock()
	for op, m := range r.errors {
		for reason, n := range m {
			fmt.Printf("   error  %-28s %-30s ×%d\n", op, reason, n)
		}
	}
	r.mu.Unlock()
	if res.samples == 0 {
		fmt.Println("   backend: phase shorter than one resource sample (1s)")
		return
	}
	fmt.Printf("   backend: cpu avg %.0f%% max %.0f%% (100%% = 1 core, %d cores), rss max %.0f MB, os threads max %d\n",
		res.cpuAvg, res.cpuMax, res.cores, res.rssMaxMB, res.threadsMax)
}

// ms formats d in milliseconds, or "-" for zero.
func ms(d time.Duration) string {
	if d == 0 {
		return "-"
	}
	return strconv.FormatFloat(float64(d.Microseconds())/1000, 'f', 2, 64) + "ms"
}
