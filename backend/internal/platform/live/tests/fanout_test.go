package tests

import (
	"bufio"
	"context"
	"flag"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/live"
)

// fanoutStreams is how many real streams BenchmarkBroadcastFanout opens. The
// game-day figure is 10,000, which needs a file-descriptor limit above
// 20,000 (client and server ends): go test -bench Fanout -fanout.streams 10000
// after ulimit -n 30000.
var fanoutStreams = flag.Int("fanout.streams", 1000, "streams BenchmarkBroadcastFanout opens")

// fanoutDialers is how many streams BenchmarkBroadcastFanout connects at
// once while setting up.
const fanoutDialers = 64

// fanoutRound is one broadcast being timed: every stream marks it received.
type fanoutRound struct {
	start   time.Time
	pending sync.WaitGroup
	mu      sync.Mutex
	lat     []time.Duration
}

// BenchmarkBroadcastFanout measures one Broadcast end to end over real HTTP
// streams: from the call until the last client has read the event. It
// reports the per-client delivery latency at p50 and p99 (ms) alongside
// ns/op, which is the time until every client has it.
func BenchmarkBroadcastFanout(b *testing.B) {
	if testing.Short() {
		b.Skip("opens many connections")
	}
	n := *fanoutStreams
	hub, _ := newHub(b)
	s := &live.Streamer{Heartbeat: time.Hour, WriteTimeout: 10 * time.Second, RetryMin: time.Second, RetryMax: time.Second}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := hub.Subscribe(uuid.New())
		defer sub.Close()
		s.Serve(w, r, sub, nil)
	}))
	defer srv.Close()
	defer hub.Close()

	client := &http.Client{Transport: &http.Transport{
		MaxIdleConnsPerHost: n,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var round atomic.Pointer[fanoutRound]
	var connected sync.WaitGroup
	// Connect a few at a time: the listen backlog (128 on macOS) resets
	// connections that arrive in one burst.
	dialing := make(chan struct{}, fanoutDialers)
	for range n {
		connected.Add(1)
		dialing <- struct{}{}
		go func() {
			req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
			res, err := client.Do(req)
			if err != nil {
				<-dialing
				connected.Done()
				b.Errorf("connect: %v", err)
				return
			}
			defer res.Body.Close()
			sc := bufio.NewScanner(res.Body)
			sc.Scan() // the retry line: connected
			<-dialing
			connected.Done()
			for sc.Scan() {
				if !strings.HasPrefix(sc.Text(), "data: ") {
					continue
				}
				r := round.Load()
				lat := time.Since(r.start)
				r.mu.Lock()
				r.lat = append(r.lat, lat)
				r.mu.Unlock()
				r.pending.Done()
			}
		}()
	}
	connected.Wait()
	if b.Failed() {
		return
	}
	for hub.Count() < n {
		time.Sleep(time.Millisecond)
	}

	ev := live.NewEvent("quiz", []byte(`{"id":"q1","status":"open"}`))
	var all []time.Duration
	for b.Loop() {
		r := &fanoutRound{lat: make([]time.Duration, 0, n)}
		r.pending.Add(n)
		r.start = time.Now()
		round.Store(r)
		hub.Broadcast(ev)
		r.pending.Wait()
		all = append(all, r.lat...)
	}
	slices.Sort(all)
	b.ReportMetric(float64(all[len(all)/2].Microseconds())/1000, "p50-ms")
	b.ReportMetric(float64(all[len(all)*99/100].Microseconds())/1000, "p99-ms")
}
