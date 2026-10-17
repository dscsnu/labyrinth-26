package main

import (
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"os"
	"sync"
	"sync/atomic"
	"text/tabwriter"
	"time"
)

// teamSize is how many players form a team: config.toml teams.size, set by
// run before any scenario.
var teamSize int

// team is a formed team and its players.
type team struct {
	id      string
	leader  player
	members []player // excluding the leader
}

// formTeams splits players into teams of teamSize (teams.size). Each team
// starts at a random moment within window, then: leader creates, the other
// teamSize-1 join at once, they ready up at once, and (if start) the leader
// starts the game.
// A non-nil fanout records when readies and starts were sent.
func formTeams(c *client, players []player, window time.Duration, start bool, f *fanout) []team {
	n := len(players) / teamSize
	teams := make([]*team, n)
	var wg sync.WaitGroup
	for i := range n {
		group := players[i*teamSize : (i+1)*teamSize]
		wg.Add(1)
		go func() {
			defer wg.Done()
			if window > 0 {
				time.Sleep(rand.N(window))
			}
			env, ok := c.call("create team", "POST", "/api/teams", group[0], map[string]string{"name": fmt.Sprintf("Bench %d", i)})
			if !ok {
				return
			}
			var created struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(env.Payload, &created) != nil {
				return
			}
			t := &team{id: created.ID, leader: group[0], members: group[1:]}
			teams[i] = t

			all := func(fn func(p player)) {
				var g sync.WaitGroup
				for _, p := range t.members {
					g.Add(1)
					go func() { defer g.Done(); fn(p) }()
				}
				g.Wait()
			}
			all(func(p player) {
				c.call("join team", "POST", "/api/teams/join", p, map[string]string{"team_id": t.id})
			})
			all(func(p player) {
				if f != nil {
					f.markReady(p.id)
				}
				c.call("ready", "PUT", "/api/teams/me/ready", p, map[string]bool{"ready": true})
			})
			if start {
				if f != nil {
					f.markStart(t.id)
				}
				c.call("start game", "POST", "/api/teams/me/start", t.leader, nil)
			}
		}()
	}
	wg.Wait()

	var out []team
	for _, t := range teams {
		if t != nil {
			out = append(out, *t)
		}
	}
	return out
}

// openStreams connects every player's event stream, at most `parallel` at a
// time so the burst doesn't overflow the server's accept queue.
func openStreams(c *client, players []player, f *fanout, parallel int) ([]*stream, int) {
	streams := make([]*stream, len(players))
	var failed atomic.Int64
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, p := range players {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			start := time.Now()
			s, err := c.openStream(p, f)
			if err != nil {
				failed.Add(1)
				c.rec.fail("sse connect", transportReason(err))
				return
			}
			c.rec.observe("sse connect", time.Since(start))
			streams[i] = s
		}()
	}
	wg.Wait()
	return streams, int(failed.Load())
}

// readStorm has `workers` players call GET /api/teams/me back to back for d.
func readStorm(c *client, players []player, workers int, d time.Duration) {
	deadline := time.Now().Add(d)
	var wg sync.WaitGroup
	for i := range min(workers, len(players)) {
		p := players[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				c.call("get my team", "GET", "/api/teams/me", p, nil)
			}
		}()
	}
	wg.Wait()
}

// rampStep is the result of one ramp step.
type rampStep struct {
	workers          int
	elapsed          time.Duration
	requests, errors int
	p99              time.Duration
	res              resourceStats
}

// ramp runs mixed traffic (90% reads, 10% ready toggles by non-leaders, no
// think time) at each worker count for stepDur, stopping once p99 exceeds
// maxP99 or errors exceed 1% of requests. Teams must be in the lobby.
func ramp(base string, teams []team, steps []int, stepDur, maxP99 time.Duration, b *backend) {
	type worker struct {
		p        player
		isLeader bool
	}
	var pool []worker
	for _, t := range teams {
		pool = append(pool, worker{t.leader, true})
		for _, m := range t.members {
			pool = append(pool, worker{m, false})
		}
	}
	rand.Shuffle(len(pool), func(i, j int) { pool[i], pool[j] = pool[j], pool[i] })

	var results []rampStep
	for _, n := range steps {
		if n > len(pool) {
			n = len(pool)
		}
		rec := newRecorder()
		c := newClient(base, rec)
		b.sampler.reset()
		deadline := time.Now().Add(stepDur)
		start := time.Now()
		var wg sync.WaitGroup
		for _, w := range pool[:n] {
			wg.Add(1)
			go func() {
				defer wg.Done()
				ready := true // formTeams readied every member
				for time.Now().Before(deadline) {
					if !w.isLeader && rand.IntN(10) == 0 {
						ready = !ready
						c.call("ready toggle", "PUT", "/api/teams/me/ready", w.p, map[string]bool{"ready": ready})
					} else {
						c.call("get my team", "GET", "/api/teams/me", w.p, nil)
					}
				}
			}()
		}
		wg.Wait()
		elapsed := time.Since(start)
		count, errs, p99 := rec.total()
		step := rampStep{workers: n, elapsed: elapsed, requests: count + errs, errors: errs, p99: p99, res: b.sampler.snapshot()}
		results = append(results, step)
		fmt.Printf("   ramp %5d workers: %7.0f req/s  p99 %-9s errors %d  cpu avg %.0f%%\n",
			n, float64(step.requests)/elapsed.Seconds(), ms(p99), errs, step.res.cpuAvg)

		if !b.alive() {
			fmt.Println("   backend process died")
			break
		}
		if p99 > maxP99 || (step.requests > 0 && float64(errs)/float64(step.requests) > 0.01) {
			rec.print(fmt.Sprintf("ramp: limit crossed at %d workers", n), elapsed, step.res)
			break
		}
		if n == len(pool) {
			break
		}
	}

	fmt.Println("\n== ramp summary (90% GET /api/teams/me, 10% ready toggles, no think time)")
	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(w, "workers\treq/s\tp99\terrors\tcpu avg\tcpu max\trss MB\t")
	for _, s := range results {
		fmt.Fprintf(w, "%d\t%.0f\t%s\t%d\t%.0f%%\t%.0f%%\t%.0f\t\n", s.workers,
			float64(s.requests)/s.elapsed.Seconds(), ms(s.p99), s.errors, s.res.cpuAvg, s.res.cpuMax, s.res.rssMaxMB)
	}
	w.Flush()
}
