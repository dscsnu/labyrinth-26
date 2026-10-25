package tests

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
)

// event returns a live event named name of type "test" whose message is the
// JSON msg.
func event(name, msg string) live.Event {
	return live.MustNewEvent(name, "test", json.RawMessage(msg))
}

// newHub returns a hub with config.toml's live.hub_shards and
// live.subscriber_buffer, and that buffer.
func newHub(t testing.TB) (*live.Hub, int) {
	t.Helper()
	set := configtest.Settings(t).Live
	return live.NewHub(set.HubShards, set.SubscriberBuffer), set.SubscriberBuffer
}

// TestHubDropsSlowSubscriberOnly checks that a subscriber that falls a full
// buffer behind is dropped while others keep receiving.
func TestHubDropsSlowSubscriberOnly(t *testing.T) {
	h, buffer := newHub(t)
	user := uuid.New()
	slow, fast := h.Subscribe(user), h.Subscribe(user)

	for i := range buffer + 1 {
		h.Send(user, event("team", fmt.Sprint(i%10)))
		<-fast.C // fast keeps up
	}

	n := 0
	for range slow.C { // closed once its buffer overflowed
		n++
	}
	if n != buffer {
		t.Fatalf("slow subscriber got %d buffered events before being dropped, want %d", n, buffer)
	}

	h.Send(user, event("team", `"x"`))
	if ev, ok := <-fast.C; !ok || string(ev.Data()) != `{"type":"test","message":"x"}` {
		t.Fatalf("fast subscriber stopped receiving: %v %v", ev, ok)
	}
	if got := h.Count(); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}

// TestHubCloseAndRemove checks that Close is idempotent, that closing the hub
// ends every stream, and that late subscribers start closed.
func TestHubCloseAndRemove(t *testing.T) {
	h, _ := newHub(t)
	a, b := h.Subscribe(uuid.New()), h.Subscribe(uuid.New())

	a.Close()
	a.Close() // idempotent
	if _, ok := <-a.C; ok {
		t.Fatal("closed subscription still open")
	}

	h.Close()
	if _, ok := <-b.C; ok {
		t.Fatal("subscription open after hub close")
	}
	b.Close()                          // safe after hub close
	h.Broadcast(event("quiz", "null")) // no-op once closed

	late := h.Subscribe(uuid.New())
	if _, ok := <-late.C; ok {
		t.Fatal("subscription added after close should start closed")
	}
	if h.Count() != 0 {
		t.Fatalf("count = %d after close", h.Count())
	}
}

// TestHubSendsOnlyToThatUser checks that events reach only the addressed
// user's streams.
func TestHubSendsOnlyToThatUser(t *testing.T) {
	h, _ := newHub(t)
	alice, bob := uuid.New(), uuid.New()
	a, b := h.Subscribe(alice), h.Subscribe(bob)

	h.Send(alice, event("team", "{}"))
	if ev := <-a.C; ev.Name() != "team" {
		t.Fatalf("alice got %v", ev)
	}
	select {
	case ev := <-b.C:
		t.Fatalf("bob got alice's event %v", ev)
	default:
	}
}

// TestHubBroadcastReachesEveryStream checks that a broadcast reaches every
// stream of every user, across shards, and that a slow subscriber is dropped
// without holding back the rest.
func TestHubBroadcastReachesEveryStream(t *testing.T) {
	h, buffer := newHub(t)
	const users = 200 // enough to land on every shard of a small hub
	var subs []*live.Subscription
	for range users {
		id := uuid.New()
		subs = append(subs, h.Subscribe(id), h.Subscribe(id)) // two tabs each
	}
	slow := h.Subscribe(uuid.New())

	for i := range buffer + 1 {
		h.Broadcast(event("quiz", fmt.Sprintf(`{"id":%d}`, i)))
		for j, s := range subs {
			if ev := <-s.C; ev.Name() != "quiz" {
				t.Fatalf("stream %d got %q", j, ev.Name())
			}
		}
	}
	n := 0
	for range slow.C {
		n++
	}
	if n != buffer {
		t.Fatalf("slow subscriber got %d events before being dropped, want %d", n, buffer)
	}
	if got := h.Count(); got != len(subs) {
		t.Fatalf("count = %d, want %d", got, len(subs))
	}
}

// TestHubConcurrentUse runs Subscribe, Send, Broadcast and Close side by side;
// run under -race it checks the hub's locking.
func TestHubConcurrentUse(t *testing.T) {
	h, _ := newHub(t)
	ids := make([]uuid.UUID, 32)
	for i := range ids {
		ids[i] = uuid.New()
	}
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Go(func() {
			sub := h.Subscribe(id)
			defer sub.Close()
			for range 50 {
				h.Send(id, event("team", "{}"))
				h.Broadcast(event("quiz", "null"))
				for len(sub.C) > 0 {
					<-sub.C
				}
			}
		})
	}
	wg.Wait()
	h.Close()
}

// BenchmarkHubSend measures Send to one stream per user from many
// goroutines at once, the team-publish hot path.
func BenchmarkHubSend(b *testing.B) {
	h, _ := newHub(b)
	const users = 10_000
	ids := make([]uuid.UUID, users)
	for i := range ids {
		ids[i] = uuid.New()
		sub := h.Subscribe(ids[i])
		go func() { // a reader that keeps up
			for range sub.C {
			}
		}()
	}
	defer h.Close()
	ev := event("team_state", `"`+strings.Repeat("x", 512)+`"`)
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		i := 0
		for pb.Next() {
			h.Send(ids[i%users], ev)
			i++
		}
	})
}

// BenchmarkHubBroadcast measures one Broadcast to 10,000 streams: the time
// to queue the event on every stream, not to write it out.
func BenchmarkHubBroadcast(b *testing.B) {
	h, _ := newHub(b)
	const streams = 10_000
	for range streams {
		sub := h.Subscribe(uuid.New())
		go func() {
			for range sub.C {
			}
		}()
	}
	defer h.Close()
	ev := event("quiz", `"`+strings.Repeat("x", 256)+`"`)
	for b.Loop() {
		h.Broadcast(ev)
	}
}

// TestSubscriptionSendReachesOnlyThatStream checks Subscription.Send: it
// reaches that one stream, not the same user's other streams, and does
// nothing once the stream is closed.
func TestSubscriptionSendReachesOnlyThatStream(t *testing.T) {
	h, _ := newHub(t)
	alice := uuid.New()
	tab1, tab2 := h.Subscribe(alice), h.Subscribe(alice)

	tab1.Send(event("activity", "{}"))
	if ev := <-tab1.C; ev.Name() != "activity" {
		t.Fatalf("tab 1 got %v", ev)
	}
	select {
	case ev := <-tab2.C:
		t.Fatalf("the user's other stream got %v", ev)
	default:
	}
	tab1.Close()
	tab1.Send(event("activity", "{}")) // closed: a no-op, no panic
	if _, ok := <-tab1.C; ok {
		t.Fatal("a closed stream got an event")
	}
}
