package live

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/config/configtest"
)

// TestHeartbeatOnlyWhenIdle checks that a stream receiving events gets no
// pings, and an idle one does, one heartbeat after its last event.
func TestHeartbeatOnlyWhenIdle(t *testing.T) {
	const heartbeat = 150 * time.Millisecond
	hub := NewHub(configtest.Settings(t).Live.SubscriberBuffer)
	user := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := hub.Subscribe(user)
		defer sub.Close()
		Serve(w, r, sub, []Event{{Name: "hello", Data: []byte("{}")}}, heartbeat)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	lines := make(chan string, 256)
	go func() {
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			lines <- sc.Text()
		}
		close(lines)
	}()
	for hub.Count() == 0 {
		time.Sleep(time.Millisecond)
	}

	// Busy: an event every heartbeat/5 for 3 heartbeats.
	busyUntil := time.Now().Add(3 * heartbeat)
	for time.Now().Before(busyUntil) {
		hub.Send(user, Event{Name: "tick", Data: []byte("{}")})
		time.Sleep(heartbeat / 5)
	}
	drain := func(d time.Duration) (pings, ticks int) {
		deadline := time.After(d)
		for {
			select {
			case l := <-lines:
				switch {
				case l == ": ping":
					pings++
				case strings.HasPrefix(l, "event: tick"):
					ticks++
				}
			case <-deadline:
				return pings, ticks
			}
		}
	}
	if pings, ticks := drain(heartbeat / 10); pings != 0 || ticks == 0 {
		t.Fatalf("while busy: %d pings, %d events; want 0 pings", pings, ticks)
	}
	if pings, _ := drain(2*heartbeat + heartbeat/2); pings == 0 {
		t.Fatal("idle stream got no ping")
	}
}
