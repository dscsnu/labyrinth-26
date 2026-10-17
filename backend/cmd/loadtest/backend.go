package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
)

// Backend process limits.
const (
	// healthTimeout is how long the backend may take to answer /healthz.
	healthTimeout = 30 * time.Second
	// stopTimeout is how long the backend may take to shut down before it's
	// killed.
	stopTimeout = 15 * time.Second
	// fanoutTimeout is how long the SSE scenario waits for every stream to
	// see its team start.
	fanoutTimeout = 15 * time.Second
)

// benchDB derives the benchmark database URL from the dev DATABASE_URL by
// swapping the database name, and creates that database if needed.
func benchDB(ctx context.Context, devURL, name string) (string, error) {
	if !strings.Contains(name, "bench") {
		return "", fmt.Errorf("refusing database %q: the name must contain \"bench\"", name)
	}
	u, err := url.Parse(devURL)
	if err != nil {
		return "", err
	}
	admin, err := pgx.Connect(ctx, devURL)
	if err != nil {
		return "", fmt.Errorf("connect to dev database: %w", err)
	}
	defer admin.Close(ctx)
	var exists bool
	if err := admin.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)", name).Scan(&exists); err != nil {
		return "", err
	}
	if !exists {
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
			return "", fmt.Errorf("create %s: %w", name, err)
		}
	}
	u.Path = "/" + name
	return u.String(), nil
}

// resetDB wipes the benchmark database. Only call it while no backend uses it.
func resetDB(ctx context.Context, dbURL string) error {
	conn, err := pgx.Connect(ctx, dbURL)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	_, err = conn.Exec(ctx, "DROP SCHEMA public CASCADE; CREATE SCHEMA public;")
	return err
}

// backend is a backend process under test.
type backend struct {
	cmd     *exec.Cmd
	url     string
	sampler *sampler
	exited  chan struct{} // closed when the process exits
}

// buildBackend compiles the backend in dir to a temporary binary.
func buildBackend(dir string) (string, error) {
	bin := filepath.Join(os.TempDir(), "labyrinth-loadtest-backend")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return bin, cmd.Run()
}

// startBackend runs bin against dbURL on port, logging to logPath, and waits
// up to healthTimeout for /healthz.
func startBackend(bin, dir, dbURL, port, logPath string) (*backend, error) {
	logFile, err := os.Create(logPath)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "DATABASE_URL="+dbURL, "PORT="+port)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	b := &backend{cmd: cmd, url: "http://localhost:" + port, exited: make(chan struct{})}
	go func() {
		_ = cmd.Wait()
		logFile.Close()
		close(b.exited)
	}()

	deadline := time.Now().Add(healthTimeout)
	for time.Now().Before(deadline) {
		if res, err := http.Get(b.url + "/healthz"); err == nil {
			res.Body.Close()
			if res.StatusCode == http.StatusOK {
				b.sampler = startSampler(cmd.Process.Pid)
				return b, nil
			}
		}
		select {
		case <-b.exited:
			return nil, fmt.Errorf("backend exited during startup; see %s", logPath)
		case <-time.After(200 * time.Millisecond):
		}
	}
	b.stop()
	return nil, fmt.Errorf("backend did not become healthy within %v; see %s", healthTimeout, logPath)
}

// alive reports whether the backend process is still running.
func (b *backend) alive() bool {
	select {
	case <-b.exited:
		return false
	default:
		return true
	}
}

// stop sends SIGTERM and waits up to 15 s, then kills the process.
func (b *backend) stop() {
	if b.sampler != nil {
		b.sampler.stop()
	}
	_ = b.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-b.exited:
	case <-time.After(stopTimeout):
		_ = b.cmd.Process.Kill()
		<-b.exited
	}
}

// sampler reads the backend's CPU and memory from /proc once a second.
type sampler struct {
	pid  int
	quit chan struct{}
	done chan struct{}

	mu      sync.Mutex
	cpu     []float64 // percent of one core, per interval
	rssMax  float64   // MB
	threads int
}

// resourceStats summarises one measurement window.
type resourceStats struct {
	cpuAvg, cpuMax, rssMaxMB float64
	threadsMax, cores        int
	samples                  int
}

// startSampler starts sampling process pid.
func startSampler(pid int) *sampler {
	s := &sampler{pid: pid, quit: make(chan struct{}), done: make(chan struct{})}
	go s.run()
	return s
}

// run samples once a second until stopped.
func (s *sampler) run() {
	defer close(s.done)
	const ticksPerSec = 100 // USER_HZ on Linux
	prevTicks, prevAt := s.cpuTicks(), time.Now()
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		select {
		case <-s.quit:
			return
		case now := <-t.C:
			ticks := s.cpuTicks()
			pct := float64(ticks-prevTicks) / ticksPerSec / now.Sub(prevAt).Seconds() * 100
			prevTicks, prevAt = ticks, now
			rss, threads := s.status()
			s.mu.Lock()
			s.cpu = append(s.cpu, pct)
			s.rssMax = max(s.rssMax, rss)
			s.threads = max(s.threads, threads)
			s.mu.Unlock()
		}
	}
}

// reset starts a new measurement window (one per scenario or ramp step).
func (s *sampler) reset() {
	s.mu.Lock()
	s.cpu, s.rssMax, s.threads = nil, 0, 0
	s.mu.Unlock()
}

// snapshot summarises the current window.
func (s *sampler) snapshot() resourceStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := resourceStats{rssMaxMB: s.rssMax, threadsMax: s.threads, cores: runtime.NumCPU(), samples: len(s.cpu)}
	for _, c := range s.cpu {
		r.cpuAvg += c
		r.cpuMax = max(r.cpuMax, c)
	}
	if len(s.cpu) > 0 {
		r.cpuAvg /= float64(len(s.cpu))
	}
	return r
}

// stop ends sampling.
func (s *sampler) stop() { close(s.quit); <-s.done }

// cpuTicks returns the process's user plus system CPU time in clock ticks.
func (s *sampler) cpuTicks() int64 {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", s.pid))
	if err != nil {
		return 0
	}
	// Fields after the ")" closing the command name; utime and stime are the
	// 14th and 15th fields overall.
	rest := string(b[strings.LastIndexByte(string(b), ')')+2:])
	f := strings.Fields(rest)
	if len(f) < 13 {
		return 0
	}
	ut, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	return ut + st
}

// status returns the process's resident memory in MB and its thread count.
func (s *sampler) status() (rssMB float64, threads int) {
	f, err := os.Open(fmt.Sprintf("/proc/%d/status", s.pid))
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), ":")
		v = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(v), "kB"))
		switch k {
		case "VmRSS":
			kb, _ := strconv.ParseFloat(v, 64)
			rssMB = kb / 1024
		case "Threads":
			threads, _ = strconv.Atoi(v)
		}
	}
	return rssMB, threads
}
