package tests

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"labyrinth/internal/platform/httpx"
)

// fakeClock is a settable clock for the limiter.
type fakeClock struct{ t time.Time }

// now returns the fake time.
func (c *fakeClock) now() time.Time { return c.t }

// TestLimiter checks the token bucket: a key gets its burst at once, then is
// refused with how long to wait; tokens come back at the rate; keys are
// independent; and buckets idle past idle are dropped (a dropped key starts
// full again).
func TestLimiter(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	l := httpx.NewLimiter(2, 3, time.Minute, clk.now) // 2 a second, bursts of 3
	for i := range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatalf("request %d of the burst refused", i+1)
		}
	}
	ok, wait := l.Allow("a")
	if ok || wait != 500*time.Millisecond {
		t.Fatalf("past the burst: ok %v, wait %v (want 500ms)", ok, wait)
	}
	if ok, _ := l.Allow("b"); !ok {
		t.Fatal("another key shares the first's bucket")
	}
	clk.t = clk.t.Add(500 * time.Millisecond)
	if ok, _ := l.Allow("a"); !ok {
		t.Fatal("a token should be back after 500ms")
	}
	if ok, _ := l.Allow("a"); ok {
		t.Fatal("only one token should be back after 500ms")
	}
	clk.t = clk.t.Add(2 * time.Minute)
	l.Allow("c") // the next call after an idle period sweeps
	if n := l.Len(); n != 1 {
		t.Fatalf("%d buckets after the idle ones were swept, want 1 (c)", n)
	}
	for range 3 {
		if ok, _ := l.Allow("a"); !ok {
			t.Fatal("a dropped key should start with a full bucket")
		}
	}
}

// TestLimiterMiddleware checks the 429: rate_limited in the envelope, a
// whole-second Retry-After of at least 1, and next not called.
func TestLimiterMiddleware(t *testing.T) {
	clk := &fakeClock{t: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	l := httpx.NewLimiter(0.25, 1, time.Minute, clk.now) // one every 4 s
	calls := 0
	h := l.Middleware(func(*http.Request) string { return "u" }, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		httpx.OK(w, nil)
	}))
	for i, want := range []int{http.StatusOK, http.StatusTooManyRequests} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
		if w.Code != want {
			t.Fatalf("request %d: %d, want %d", i+1, w.Code, want)
		}
		if want == http.StatusTooManyRequests && (!strings.Contains(w.Body.String(), `"code":"rate_limited"`) || w.Header().Get("Retry-After") != "4") {
			t.Fatalf("429: %q, Retry-After %q (want 4)", w.Body.String(), w.Header().Get("Retry-After"))
		}
	}
	if calls != 1 {
		t.Fatalf("next called %d times, want 1", calls)
	}
}

// TestRecover checks that a panic becomes a logged 500 envelope; that a panic
// after the response started doesn't write a second one; and that
// http.ErrAbortHandler is re-panicked for net/http.
func TestRecover(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	h := httpx.Recover(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != http.StatusInternalServerError || w.Body.String() != `{"success":false,"message":"internal error"}`+"\n" ||
		!strings.Contains(logs.String(), "boom") || !strings.Contains(logs.String(), "stack=") {
		t.Fatalf("panic: %d %q, log %q", w.Code, w.Body.String(), logs.String())
	}

	h = httpx.Recover(log, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpx.OK(w, "partial")
		panic("late")
	}))
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("GET", "/x", nil))
	if w.Code != http.StatusOK || strings.Count(w.Body.String(), "success") != 1 {
		t.Fatalf("a panic after writing: %d %q", w.Code, w.Body.String())
	}

	defer func() {
		if v := recover(); v != http.ErrAbortHandler {
			t.Fatalf("ErrAbortHandler should be re-panicked, got %v", v)
		}
	}()
	httpx.Recover(log, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(http.ErrAbortHandler) })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/x", nil))
}

// TestAccessLog checks the log line (the route pattern, status, bytes), that
// a skipped path isn't logged, and that http.ResponseController still flushes
// through the recorder (the event stream depends on it).
func TestAccessLog(t *testing.T) {
	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/things/{id}", func(w http.ResponseWriter, _ *http.Request) {
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush through the access log: %v", err)
		}
		httpx.ErrorCode(w, http.StatusConflict, "nope", "no")
	})
	h := httpx.AccessLog(log, mux, "/health")
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/api/things/7", nil))
	line := logs.String()
	if !strings.Contains(line, `route="GET /api/things/{id}"`) || !strings.Contains(line, "status=409") || !strings.Contains(line, "bytes=") {
		t.Fatalf("access log %q", line)
	}
	logs.Reset()
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/health", nil))
	if logs.Len() != 0 {
		t.Fatalf("a skipped path was logged: %q", logs.String())
	}
}
