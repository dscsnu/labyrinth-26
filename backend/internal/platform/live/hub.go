// Package live delivers server-sent events. Use cases publish snapshots to
// users through the Hub; Serve streams them over HTTP.
package live

import (
	"sync"

	"github.com/google/uuid"
)

// Event is one server-sent event. Data is compact JSON (no newlines).
type Event struct {
	Name string
	Data []byte
}

// Subscription is one open event stream for a user. C is closed when the
// subscription ends (dropped for being slow, or the hub closing).
type Subscription struct {
	C  <-chan Event
	ch chan Event
	h  *Hub
	id uuid.UUID
}

// Close ends the subscription. Safe to call more than once.
func (s *Subscription) Close() { s.h.remove(s) }

// Hub fans events out to every open stream, keyed by user, so a stream
// follows its user from team to team.
type Hub struct {
	mu     sync.Mutex
	subs   map[uuid.UUID]map[*Subscription]struct{}
	closed bool
	// buffer is how many events a slow client may fall behind before it's
	// dropped (config.toml live.subscriber_buffer). Events are full
	// snapshots, so a dropped client loses nothing: it reconnects and starts
	// from the current state.
	buffer int
}

// NewHub returns an empty Hub whose subscribers may fall buffer events
// behind before being dropped.
func NewHub(buffer int) *Hub {
	return &Hub{subs: make(map[uuid.UUID]map[*Subscription]struct{}), buffer: buffer}
}

// Subscribe opens a stream for userID. Subscribe before reading the state you
// send first, so no change can slip between the two.
func (h *Hub) Subscribe(userID uuid.UUID) *Subscription {
	ch := make(chan Event, h.buffer)
	sub := &Subscription{C: ch, ch: ch, h: h, id: userID}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(ch)
		return sub
	}
	if h.subs[userID] == nil {
		h.subs[userID] = make(map[*Subscription]struct{})
	}
	h.subs[userID][sub] = struct{}{}
	return sub
}

// remove drops sub from the hub, if it's still registered.
func (h *Hub) remove(sub *Subscription) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.dropLocked(sub)
}

// dropLocked unregisters sub and closes its channel. The caller holds h.mu.
// Dropping an already-dropped subscription is a no-op.
func (h *Hub) dropLocked(sub *Subscription) {
	set := h.subs[sub.id]
	if _, ok := set[sub]; !ok {
		return
	}
	delete(set, sub)
	if len(set) == 0 {
		delete(h.subs, sub.id)
	}
	close(sub.ch)
}

// Send delivers ev to every stream of userID. It never blocks: a subscriber
// whose buffer is full is dropped.
func (h *Hub) Send(userID uuid.UUID, ev Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for sub := range h.subs[userID] {
		select {
		case sub.ch <- ev:
		default:
			h.dropLocked(sub)
		}
	}
}

// Count reports how many streams are open.
func (h *Hub) Count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, set := range h.subs {
		n += len(set)
	}
	return n
}

// Close ends every subscription and refuses new ones. Call on shutdown so
// streaming handlers return.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for _, set := range h.subs {
		for sub := range set {
			close(sub.ch)
		}
	}
	clear(h.subs)
}
