package activity

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"labyrinth/internal/platform/live"
)

// Feed is the activity use case: the admin live view's queue, its recent
// entries and the streams watching. One mutex guards everything; every
// operation under it is a few slice writes, never I/O or encoding.
//
//	Feed
//	  mu        Mutex
//	  pending   ring  entries since the last flush (activity.batch_max)
//	  dropped   int   entries pending overwrote since the last flush
//	  recent    ring  the latest flushed entries (activity.recent)
//	  held      map[team ID][]string  each team's held powerups, to name
//	                  the one granted or activated
//	  watchers  set of *live.Subscription  the admins' ?admin=1 streams
type Feed struct {
	teams Teams
	log   *slog.Logger
	rules Rules
	now   func() time.Time

	mu       sync.Mutex
	pending  ring
	dropped  int
	recent   ring
	held     map[string][]string
	watchers map[*live.Subscription]struct{}
}

// New returns a feed reading names from t.
func New(t Teams, log *slog.Logger, rules Rules) *Feed {
	now := rules.Now
	if now == nil {
		now = time.Now
	}
	return &Feed{
		teams: t, log: log, rules: rules, now: now,
		pending: newRing(rules.BatchMax), recent: newRing(rules.Recent),
		held: map[string][]string{}, watchers: map[*live.Subscription]struct{}{},
	}
}

// The "activity" event's types, besides live.Snapshot.
const (
	// Batched: the changes since the last flush.
	Batched live.EventType = "batch"
)

// Run flushes every activity.publish_every until ctx is done. main starts it.
func (f *Feed) Run(ctx context.Context) {
	tick := time.NewTicker(f.rules.PublishEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			f.Flush()
		}
	}
}

// SnapshotEvent is Snapshot as the "activity" event of type snapshot.
func (f *Feed) SnapshotEvent() (live.Event, error) {
	return live.NewEvent("activity", live.Snapshot, f.Snapshot())
}

// ring is a fixed-size queue that overwrites its oldest item when full.
// Capacity 0 holds nothing.
type ring struct {
	buf  []Entry
	head int // index of the oldest
	n    int
}

// newRing returns an empty ring holding at most size entries.
func newRing(size int) ring { return ring{buf: make([]Entry, max(size, 0))} }

// push adds e, overwriting the oldest when full, and reports whether it
// overwrote one.
func (r *ring) push(e Entry) bool {
	if len(r.buf) == 0 {
		return true
	}
	if r.n < len(r.buf) {
		r.buf[(r.head+r.n)%len(r.buf)] = e
		r.n++
		return false
	}
	r.buf[r.head] = e
	r.head = (r.head + 1) % len(r.buf)
	return true
}

// items returns the entries oldest first, as a new slice ([] when empty).
func (r *ring) items() []Entry {
	out := make([]Entry, r.n)
	for i := range r.n {
		out[i] = r.buf[(r.head+i)%len(r.buf)]
	}
	return out
}

// clear empties the ring.
func (r *ring) clear() { r.head, r.n = 0, 0 }
