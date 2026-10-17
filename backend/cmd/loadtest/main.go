// Command loadtest drives a real backend with thousands of virtual players to
// find where it slows down or fails.
//
// It builds the backend, runs it on its own port against its own database
// (created next to the dev one, name must contain "bench"), creates players
// directly in that database, and signs their tokens with JWT_PRIVATE_KEY, so
// no Google login is needed. Your dev database is never touched. Each
// scenario starts from an empty database and a fresh backend.
//
//	cd backend && go run ./cmd/loadtest                       # everything, 10k players
//	go run ./cmd/loadtest -scenarios rush,sse -players 3000
//	go run ./cmd/loadtest -scenarios ramp -pool 32             # compare Postgres pool sizes
package main

import (
	"context"
	"flag"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"labyrinth/internal/platform/config"
)

// main runs the load test and exits non-zero on failure.
func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "loadtest:", err)
		os.Exit(1)
	}
}

// run parses flags, prepares the bench database and backend, and runs the
// chosen scenarios, each against a fresh backend and database.
func run() error {
	var (
		players      = flag.Int("players", 10_000, "virtual players (rounded down to whole teams of config.toml teams.size)")
		scenarioList = flag.String("scenarios", "rush,sse,reads,ramp", "comma-separated: rush, sse, reads, ramp")
		dbName       = flag.String("db", "labyrinth_bench", "benchmark database name (must contain \"bench\")")
		port         = flag.Int("port", 3100, "port for the backend under test")
		pool         = flag.Int("pool", 0, "backend Postgres pool size (pool_max_conns); 0 = pgx default")
		window       = flag.Duration("rush-window", 30*time.Second, "teams start forming at random moments within this window")
		streamPar    = flag.Int("stream-parallel", 200, "how many event streams to open at once")
		readWorkers  = flag.Int("read-workers", 1000, "concurrent players in the read storm")
		readDur      = flag.Duration("read-duration", 15*time.Second, "read storm length")
		rampSteps    = flag.String("ramp-steps", "250,500,1000,2000,4000,8000,10000", "ramp worker counts")
		rampDur      = flag.Duration("ramp-step", 15*time.Second, "length of each ramp step")
		rampP99      = flag.Duration("ramp-p99", 500*time.Millisecond, "ramp stops once p99 exceeds this")
	)
	flag.Parse()

	dir, err := os.Getwd()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return fmt.Errorf("run from the backend directory: go run ./cmd/loadtest")
	}
	_ = godotenv.Load()
	key, err := config.ParsePrivateKey(os.Getenv("JWT_PRIVATE_KEY"))
	if err != nil {
		return fmt.Errorf("JWT_PRIVATE_KEY: %w", err)
	}
	settings, err := config.LoadSettings(config.SettingsPath())
	if err != nil {
		return err
	}
	teamSize = settings.Teams.Size

	scenarios := strings.Split(*scenarioList, ",")
	for _, s := range scenarios {
		if !slices.Contains([]string{"rush", "sse", "reads", "ramp"}, s) {
			return fmt.Errorf("unknown scenario %q", s)
		}
	}
	var steps []int
	for _, s := range strings.Split(*rampSteps, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil || n <= 0 {
			return fmt.Errorf("bad ramp step %q", s)
		}
		steps = append(steps, n)
	}
	n := *players / teamSize * teamSize

	ctx := context.Background()
	benchURL, err := benchDB(ctx, os.Getenv("DATABASE_URL"), *dbName)
	if err != nil {
		return err
	}
	backendDB := benchURL
	if *pool > 0 {
		u, _ := url.Parse(benchURL)
		q := u.Query()
		q.Set("pool_max_conns", strconv.Itoa(*pool))
		u.RawQuery = q.Encode()
		backendDB = u.String()
	}

	fmt.Fprintln(os.Stderr, "building backend…")
	bin, err := buildBackend(dir)
	if err != nil {
		return fmt.Errorf("build backend: %w", err)
	}
	logPath := filepath.Join(os.TempDir(), "labyrinth-loadtest-backend.log")
	poolDesc := "pgx default"
	if *pool > 0 {
		poolDesc = strconv.Itoa(*pool)
	}
	fmt.Printf("loadtest: %d players (%d teams), %d cores shared by load generator and backend, pg pool %s\n",
		n, n/teamSize, runtime.NumCPU(), poolDesc)
	fmt.Printf("backend log: %s\n", logPath)

	// fresh gives each scenario an empty database, a new backend and new players.
	fresh := func() (*backend, []player, error) {
		if err := resetDB(ctx, benchURL); err != nil {
			return nil, nil, fmt.Errorf("reset bench db: %w", err)
		}
		b, err := startBackend(bin, dir, backendDB, strconv.Itoa(*port), logPath)
		if err != nil {
			return nil, nil, err
		}
		ps, err := createPlayers(ctx, benchURL, key, settings.Auth.TokenTTL.Duration, n)
		if err != nil {
			b.stop()
			return nil, nil, fmt.Errorf("create players: %w", err)
		}
		return b, ps, nil
	}
	has := func(s string) bool { return slices.Contains(scenarios, s) }

	if has("rush") {
		b, ps, err := fresh()
		if err != nil {
			return err
		}
		rec := newRecorder()
		c := newClient(b.url, rec)
		b.sampler.reset()
		start := time.Now()
		teams := formTeams(c, ps, *window, true, nil)
		rec.print(fmt.Sprintf("event-start rush: %d teams form and start within %s", len(teams), *window), time.Since(start), b.sampler.snapshot())
		b.stop()
	}

	if has("sse") || has("reads") {
		b, ps, err := fresh()
		if err != nil {
			return err
		}
		var streams []*stream
		if has("sse") {
			rec := newRecorder()
			c := newClient(b.url, rec)
			f := newFanout()

			b.sampler.reset()
			start := time.Now()
			var failed int
			streams, failed = openStreams(c, ps, f, *streamPar)
			rec.print(fmt.Sprintf("open %d event streams (%d at a time)", len(ps), *streamPar), time.Since(start), b.sampler.snapshot())
			if failed > 0 {
				fmt.Printf("   %d streams failed to open\n", failed)
			}

			rec = newRecorder()
			c.rec = rec
			b.sampler.reset()
			start = time.Now()
			teams := formTeams(c, ps, *window, true, f)
			missed := waitStarted(streams, fanoutTimeout)
			rec.print(fmt.Sprintf("SSE fan-out: rush of %d teams with every player streaming", len(teams)), time.Since(start), b.sampler.snapshot())
			open := 0
			for _, s := range streams {
				if s != nil {
					open++
				}
			}
			fmt.Printf("   streams: %d open, %d never saw their team start (waited 15s)\n", open, missed)
		} else {
			formTeams(newClient(b.url, newRecorder()), ps, 0, true, nil)
		}

		if has("reads") {
			rec := newRecorder()
			c := newClient(b.url, rec)
			b.sampler.reset()
			start := time.Now()
			readStorm(c, ps, *readWorkers, *readDur)
			title := fmt.Sprintf("read storm: %d players polling GET /api/teams/me", min(*readWorkers, len(ps)))
			if streams != nil {
				title += fmt.Sprintf(" (with %d streams open)", len(ps))
			}
			rec.print(title, time.Since(start), b.sampler.snapshot())
		}
		for _, s := range streams {
			if s != nil {
				s.cancel()
			}
		}
		b.stop()
	}

	if has("ramp") {
		b, ps, err := fresh()
		if err != nil {
			return err
		}
		// Teams stay in the lobby so ready toggles are valid writes.
		teams := formTeams(newClient(b.url, newRecorder()), ps, 0, false, nil)
		fmt.Printf("\n== ramp to breaking point: %d lobby teams, %s per step, stop at p99 > %s or > 1%% errors\n",
			len(teams), *rampDur, *rampP99)
		ramp(b.url, teams, steps, *rampDur, *rampP99, b)
		b.stop()
	}
	return nil
}

// waitStarted waits up to d for every stream to see its team start, and
// returns how many didn't.
func waitStarted(streams []*stream, d time.Duration) int {
	deadline := time.After(d)
	missed := 0
	for _, s := range streams {
		if s == nil {
			continue
		}
		select {
		case <-s.started:
		case <-deadline:
			missed++
			continue
		}
	}
	return missed
}
