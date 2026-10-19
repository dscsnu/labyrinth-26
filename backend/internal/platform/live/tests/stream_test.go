package tests

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"labyrinth/internal/platform/live"
)

// streamer returns a Streamer with the given heartbeat and a write timeout
// and retry range short enough for tests.
func streamer(heartbeat time.Duration) *live.Streamer {
	return &live.Streamer{Heartbeat: heartbeat, WriteTimeout: time.Second, RetryMin: time.Second, RetryMax: time.Second}
}

// fakeWriter is a ResponseWriter that records what Serve writes, counts its
// flushes, and supports the deadlines http.ResponseController sets. With
// stall set, every Write blocks until the write deadline and then fails, like
// a client that has stopped reading.
type fakeWriter struct {
	mu       sync.Mutex
	header   http.Header
	buf      bytes.Buffer
	flushes  int
	deadline time.Time
	stall    bool
}

// newFakeWriter returns an empty fakeWriter.
func newFakeWriter() *fakeWriter { return &fakeWriter{header: http.Header{}} }

// Header returns the response headers.
func (f *fakeWriter) Header() http.Header { return f.header }

// WriteHeader ignores the status; Serve always sends 200.
func (f *fakeWriter) WriteHeader(int) {}

// Write records b, or with stall set, fails at the write deadline.
func (f *fakeWriter) Write(b []byte) (int, error) {
	f.mu.Lock()
	stall, deadline := f.stall, f.deadline
	f.mu.Unlock()
	if stall {
		time.Sleep(time.Until(deadline))
		return 0, os.ErrDeadlineExceeded
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.Write(b)
}

// Flush counts a flush.
func (f *fakeWriter) Flush() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.flushes++
}

// SetWriteDeadline records the deadline Write honours when stalled.
func (f *fakeWriter) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.deadline = t
	return nil
}

// SetReadDeadline accepts the read deadline Serve clears.
func (f *fakeWriter) SetReadDeadline(time.Time) error { return nil }

// state returns what has been written so far and how many flushes it took.
func (f *fakeWriter) state() (string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.buf.String(), f.flushes
}

// serve runs s.Serve in the background and returns a channel closed when it
// returns, and a function that disconnects the client.
func serve(s *live.Streamer, w http.ResponseWriter, sub *live.Subscription, initial []live.Event) (done <-chan struct{}, disconnect func()) {
	ctx, cancel := context.WithCancel(context.Background())
	r := httptest.NewRequestWithContext(ctx, "GET", "/api/events", nil)
	ch := make(chan struct{})
	go func() {
		defer close(ch)
		s.Serve(w, r, sub, initial)
	}()
	return ch, cancel
}

// waitFor fails the test unless cond becomes true within a second.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// TestServeWritesFrames checks the exact bytes of a stream: the retry line,
// then each event as one "event:" line and one "data:" line holding the
// envelope {"type", "message"}.
func TestServeWritesFrames(t *testing.T) {
	h, _ := newHub(t)
	sub := h.Subscribe(uuid.New())
	w := newFakeWriter()
	done, disconnect := serve(streamer(time.Hour), w, sub, []live.Event{
		live.MustNewEvent("hello", "connected", struct {
			A int `json:"a"`
		}{1}),
		live.MustNewEvent("raw", "snapshot", json.RawMessage(`{"b": [1, 2]}`)),
		live.MustNewEvent("empty", "left", struct{}{}),
	})
	defer func() { disconnect(); <-done }()

	want := "retry: 1000\n\n" +
		"event: hello\ndata: {\"type\":\"connected\",\"message\":{\"a\":1}}\n\n" +
		"event: raw\ndata: {\"type\":\"snapshot\",\"message\":{\"b\":[1,2]}}\n\n" +
		"event: empty\ndata: {\"type\":\"left\",\"message\":{}}\n\n"
	waitFor(t, "initial events", func() bool { got, _ := w.state(); return got == want })
	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("Content-Type = %q", ct)
	}
}

// TestNewEvent checks an event's name, type and data, that a json.RawMessage
// is passed through (compacted), and that a message that can't be encoded is
// an error.
func TestNewEvent(t *testing.T) {
	ev, err := live.NewEvent("team_state", "spell_cast", json.RawMessage(`{"seq": 3}`))
	if err != nil {
		t.Fatal(err)
	}
	if ev.Name() != "team_state" || ev.Type() != "spell_cast" || string(ev.Data()) != `{"type":"spell_cast","message":{"seq":3}}` {
		t.Fatalf("got %q %q %s", ev.Name(), ev.Type(), ev.Data())
	}
	if _, err := live.NewEvent("bad", "x", make(chan int)); err == nil {
		t.Fatal("encoding a channel should fail")
	}
}

// TestVersionNext checks that Next counts up within an epoch and keeps
// counting into a new one.
func TestVersionNext(t *testing.T) {
	v := live.Version{}.Next(7)
	if v != (live.Version{Epoch: 7, Seq: 1}) {
		t.Fatalf("first version %+v", v)
	}
	if v = v.Next(7); v != (live.Version{Epoch: 7, Seq: 2}) {
		t.Fatalf("second version %+v", v)
	}
}

// TestServeRetryWithinRange checks that each stream's retry delay is within
// RetryMin..RetryMax.
func TestServeRetryWithinRange(t *testing.T) {
	h, _ := newHub(t)
	s := &live.Streamer{Heartbeat: time.Hour, WriteTimeout: time.Second, RetryMin: time.Second, RetryMax: 3 * time.Second}
	for range 20 {
		w := newFakeWriter()
		done, disconnect := serve(s, w, h.Subscribe(uuid.New()), nil)
		var line string
		waitFor(t, "retry line", func() bool {
			got, _ := w.state()
			line, _, _ = strings.Cut(got, "\n")
			return strings.HasSuffix(got, "\n\n")
		})
		disconnect()
		<-done
		ms, err := strconv.Atoi(strings.TrimPrefix(line, "retry: "))
		if err != nil || ms < 1000 || ms > 3000 {
			t.Fatalf("retry line %q, want 1000..3000 ms", line)
		}
	}
}

// TestServeCoalescesQueuedEvents checks that events already queued when one
// arrives go out with it under a single flush.
func TestServeCoalescesQueuedEvents(t *testing.T) {
	h, _ := newHub(t)
	user := uuid.New()
	sub := h.Subscribe(user)
	for _, name := range []string{"a", "b", "c"} {
		h.Send(user, event(name, "{}"))
	}
	w := newFakeWriter()
	done, disconnect := serve(streamer(time.Hour), w, sub, nil)
	defer func() { disconnect(); <-done }()

	waitFor(t, "queued events", func() bool { got, _ := w.state(); return strings.Contains(got, "event: c") })
	got, flushes := w.state()
	if flushes != 2 { // the retry line, then all three events together
		t.Fatalf("%d flushes, want 2 (retry, then a+b+c); wrote %q", flushes, got)
	}
	if a, b, c := strings.Index(got, "event: a"), strings.Index(got, "event: b"), strings.Index(got, "event: c"); !(a < b && b < c) {
		t.Fatalf("events out of order: %q", got)
	}
}

// TestServeStalledClientTimesOut checks that a client that stops reading is
// cut off after WriteTimeout instead of holding Serve forever.
func TestServeStalledClientTimesOut(t *testing.T) {
	h, _ := newHub(t)
	w := newFakeWriter()
	w.stall = true
	s := &live.Streamer{Heartbeat: time.Hour, WriteTimeout: 50 * time.Millisecond, RetryMin: time.Second, RetryMax: time.Second}
	done, disconnect := serve(s, w, h.Subscribe(uuid.New()), nil)
	defer disconnect()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Serve still running a second after the write deadline")
	}
}

// TestHeartbeatOnlyWhenIdle checks that a stream receiving events gets no
// pings, and an idle one does, one heartbeat after its last event.
func TestHeartbeatOnlyWhenIdle(t *testing.T) {
	const heartbeat = 150 * time.Millisecond
	hub, _ := newHub(t)
	user := uuid.New()
	s := streamer(heartbeat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sub := hub.Subscribe(user)
		defer sub.Close()
		s.Serve(w, r, sub, []live.Event{event("hello", "{}")})
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
		hub.Send(user, event("tick", "{}"))
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
