package live

import (
	"fmt"
	"net/http"
	"time"
)

// Serve streams sub to the client as server-sent events: first the initial
// snapshot events, then every event published to the subscription. After a
// heartbeat (config.toml live.heartbeat) with no event it sends a ": ping"
// comment, so mobile networks and proxies don't drop the idle connection. Returns when the client
// disconnects or the subscription ends; the client's EventSource then
// reconnects.
func Serve(w http.ResponseWriter, r *http.Request, sub *Subscription, initial []Event, heartbeat time.Duration) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer the stream
	w.WriteHeader(http.StatusOK)

	rc := http.NewResponseController(w)
	// The server's read timeout covers a whole request; a stream lasts for
	// hours, and hitting the deadline would cancel its context. Not every
	// writer supports deadlines (httptest's doesn't), which is fine.
	_ = rc.SetReadDeadline(time.Time{})
	write := func(ev Event) error {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Name, ev.Data); err != nil {
			return err
		}
		return rc.Flush()
	}

	for _, ev := range initial {
		if err := write(ev); err != nil {
			return
		}
	}

	ticker := time.NewTicker(heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case ev, ok := <-sub.C:
			if !ok {
				return // dropped for being slow, or shutting down
			}
			if err := write(ev); err != nil {
				return
			}
			ticker.Reset(heartbeat) // pings are only for idle streams
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	}
}
