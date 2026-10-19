package live

import (
	"errors"
	"math/rand/v2"
	"net/http"
	"strconv"
	"time"
)

// ping is the comment sent to an idle stream. Clients ignore comments.
var ping = []byte(": ping\n\n")

// Streamer writes subscriptions to clients as server-sent events. Its fields
// come from config.toml's [live] table; build one at startup and share it.
type Streamer struct {
	// Heartbeat is how long a stream may go without an event before it gets
	// a ping, so mobile networks and proxies don't drop the idle connection
	// (live.heartbeat).
	Heartbeat time.Duration
	// WriteTimeout bounds each write to the client. A client that can't take
	// a write within it is disconnected, which frees its goroutine instead of
	// leaving it stuck until TCP gives up (live.write_timeout).
	WriteTimeout time.Duration
	// RetryMin and RetryMax bound the reconnect delay sent to each client,
	// picked at random per stream, so that clients cut off together (a
	// restart) don't all reconnect in the same instant (live.retry_min and
	// live.retry_max).
	RetryMin, RetryMax time.Duration
}

// Serve streams sub to the client: a reconnect delay, then the initial
// snapshot events, then every event published to the subscription, with a
// ping after each Heartbeat of silence. It returns when the client
// disconnects, a write fails or times out, or the subscription ends; the
// client's EventSource then reconnects.
func (s *Streamer) Serve(w http.ResponseWriter, r *http.Request, sub *Subscription, initial []Event) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	w.WriteHeader(http.StatusOK)

	c := conn{w: w, rc: http.NewResponseController(w), timeout: s.WriteTimeout}
	// The server's read timeout covers a whole request; a stream lasts for
	// hours, and hitting the deadline would cancel its context.
	if err := c.rc.SetReadDeadline(time.Time{}); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return
	}

	c.write(s.retryField())
	for _, ev := range initial {
		c.write(ev.frame)
	}
	if c.flush() != nil {
		return
	}

	ticker := time.NewTicker(s.Heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return // dropped for being slow, or shutting down
			}
			c.write(ev.frame)
			// Send whatever else is already queued with the same flush. At
			// most a buffer's worth, so a busy stream still flushes.
			open := drain(sub.C, cap(sub.C), func(ev Event) { c.write(ev.frame) })
			if c.flush() != nil || !open {
				return
			}
			ticker.Reset(s.Heartbeat) // pings are only for idle streams
		case <-ticker.C:
			c.write(ping)
			if c.flush() != nil {
				return
			}
		}
	}
}

// retryField returns the SSE "retry:" line for one stream: a delay picked
// uniformly between RetryMin and RetryMax, in milliseconds.
func (s *Streamer) retryField() []byte {
	d := s.RetryMin
	if spread := s.RetryMax - s.RetryMin; spread > 0 {
		d += rand.N(spread + 1)
	}
	b := append([]byte("retry: "), strconv.FormatInt(d.Milliseconds(), 10)...)
	return append(b, '\n', '\n')
}

// drain passes up to limit events already queued on c to fn without
// waiting, and reports whether c is still open.
func drain(c <-chan Event, limit int, fn func(Event)) bool {
	for range limit {
		select {
		case ev, ok := <-c:
			if !ok {
				return false
			}
			fn(ev)
		default:
			return true
		}
	}
	return true
}

// conn writes frames to one client, remembering the first error so a batch
// of writes needs one check, at the flush.
type conn struct {
	w       http.ResponseWriter
	rc      *http.ResponseController
	timeout time.Duration
	err     error
}

// write sends b, under a fresh write deadline, unless an earlier write
// failed. Writers without deadline support (httptest's) write without one.
func (c *conn) write(b []byte) {
	if c.err != nil || len(b) == 0 {
		return
	}
	if err := c.rc.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		c.err = err
		return
	}
	_, c.err = c.w.Write(b)
}

// flush pushes buffered frames to the client and returns the first error
// since the stream began.
func (c *conn) flush() error {
	if c.err == nil {
		c.err = c.rc.Flush()
	}
	return c.err
}
