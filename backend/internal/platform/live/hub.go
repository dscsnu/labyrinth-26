// Package live delivers server-sent events. Use cases publish snapshots
// through the Hub, to one user (Send) or to everyone (Broadcast); a Streamer
// writes them to each client over HTTP.
package live

import (
	"hash/maphash"
	"sync"

	"github.com/google/uuid"
)

// Subscription is one open event stream for a user. C is closed when the
// subscription ends (dropped for being slow, or the hub closing).
type Subscription struct {
	C     <-chan Event
	ch    chan Event
	shard *shard
	id    uuid.UUID
}

// Close ends the subscription. Safe to call more than once.
func (s *Subscription) Close() {
	s.shard.mu.Lock()
	defer s.shard.mu.Unlock()
	s.shard.dropLocked(s)
}

// Send delivers ev to this stream alone (not the user's other streams), if
// it's still open. Like Hub.Send it never blocks: a full buffer drops it.
// For events only one stream asked for (the admin live view's).
func (s *Subscription) Send(ev Event) {
	s.shard.mu.Lock()
	defer s.shard.mu.Unlock()
	if _, ok := s.shard.subs[s.id][s]; ok {
		s.shard.deliverLocked(s, ev)
	}
}

// Hub fans events out to open streams, keyed by user, so a stream follows its
// user from team to team. Streams are spread over shards by user ID, each
// with its own lock: a Send locks one shard, and a Broadcast locks one shard
// at a time, so neither holds up the whole hub.
type Hub struct {
	shards []shard
	seed   maphash.Seed
	// buffer is how many events a slow client may fall behind before it's
	// dropped (config.toml live.subscriber_buffer). Events are full
	// snapshots, so a dropped client loses nothing: it reconnects and starts
	// from the current state.
	buffer int
}

// shard holds the streams of the users that hash to it.
type shard struct {
	mu     sync.Mutex
	subs   map[uuid.UUID]map[*Subscription]struct{}
	closed bool
}

// NewHub returns an empty Hub with shards shards (config.toml
// live.hub_shards) whose subscribers may fall buffer events behind before
// being dropped. It panics if shards is less than 1.
func NewHub(shards, buffer int) *Hub {
	if shards < 1 {
		panic("live: NewHub needs at least one shard")
	}
	h := &Hub{shards: make([]shard, shards), seed: maphash.MakeSeed(), buffer: buffer}
	for i := range h.shards {
		h.shards[i].subs = make(map[uuid.UUID]map[*Subscription]struct{})
	}
	return h
}

// shardFor returns the shard that holds userID's streams.
func (h *Hub) shardFor(userID uuid.UUID) *shard {
	return &h.shards[maphash.Comparable(h.seed, userID)%uint64(len(h.shards))]
}

// Subscribe opens a stream for userID. Subscribe before reading the state you
// send first, so no change can slip between the two.
func (h *Hub) Subscribe(userID uuid.UUID) *Subscription {
	ch := make(chan Event, h.buffer)
	sh := h.shardFor(userID)
	sub := &Subscription{C: ch, ch: ch, shard: sh, id: userID}

	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.closed {
		close(ch)
		return sub
	}
	set := sh.subs[userID]
	if set == nil {
		set = make(map[*Subscription]struct{})
		sh.subs[userID] = set
	}
	set[sub] = struct{}{}
	return sub
}

// Send delivers ev to every stream of userID. It never blocks: a subscriber
// whose buffer is full is dropped.
func (h *Hub) Send(userID uuid.UUID, ev Event) {
	sh := h.shardFor(userID)
	sh.mu.Lock()
	defer sh.mu.Unlock()
	for sub := range sh.subs[userID] {
		sh.deliverLocked(sub, ev)
	}
}

// Broadcast delivers ev to every open stream. Like Send it never blocks, and
// it locks one shard at a time. Events from one Broadcast reach each stream
// in order with that publisher's other events, but two concurrent Broadcasts
// may interleave differently on different streams: publish game-wide events
// under the lock of the state they show.
func (h *Hub) Broadcast(ev Event) {
	for i := range h.shards {
		sh := &h.shards[i]
		sh.mu.Lock()
		for _, set := range sh.subs {
			for sub := range set {
				sh.deliverLocked(sub, ev)
			}
		}
		sh.mu.Unlock()
	}
}

// Count reports how many streams are open.
func (h *Hub) Count() int {
	n := 0
	for i := range h.shards {
		sh := &h.shards[i]
		sh.mu.Lock()
		for _, set := range sh.subs {
			n += len(set)
		}
		sh.mu.Unlock()
	}
	return n
}

// Close ends every subscription and refuses new ones. Call on shutdown so
// streaming handlers return.
func (h *Hub) Close() {
	for i := range h.shards {
		sh := &h.shards[i]
		sh.mu.Lock()
		sh.closed = true
		for _, set := range sh.subs {
			for sub := range set {
				close(sub.ch)
			}
		}
		clear(sh.subs)
		sh.mu.Unlock()
	}
}

// deliverLocked queues ev for sub, dropping sub if its buffer is full. The
// caller holds sh.mu.
func (sh *shard) deliverLocked(sub *Subscription, ev Event) {
	select {
	case sub.ch <- ev:
	default:
		sh.dropLocked(sub)
	}
}

// dropLocked unregisters sub and closes its channel. The caller holds sh.mu.
// Dropping an already-dropped subscription is a no-op.
func (sh *shard) dropLocked(sub *Subscription) {
	set := sh.subs[sub.id]
	if _, ok := set[sub]; !ok {
		return
	}
	delete(set, sub)
	if len(set) == 0 {
		delete(sh.subs, sub.id)
	}
	close(sub.ch)
}
