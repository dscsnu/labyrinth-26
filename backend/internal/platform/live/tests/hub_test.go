package tests

import (
	"testing"

	"github.com/google/uuid"

	"labyrinth/internal/platform/config/configtest"
	"labyrinth/internal/platform/live"
)

// testBuffer returns config.toml's live.subscriber_buffer, the buffer the hub
// tests use.
func testBuffer(t *testing.T) int {
	t.Helper()
	return configtest.Settings(t).Live.SubscriberBuffer
}

// TestHubDropsSlowSubscriberOnly checks that a subscriber that falls a full
// buffer behind is dropped while others keep receiving.
func TestHubDropsSlowSubscriberOnly(t *testing.T) {
	buffer := testBuffer(t)
	h := live.NewHub(buffer)
	user := uuid.New()
	slow, fast := h.Subscribe(user), h.Subscribe(user)

	for i := range buffer + 1 {
		h.Send(user, live.Event{Name: "team", Data: []byte{byte('0' + i%10)}})
		<-fast.C // fast keeps up
	}

	n := 0
	for range slow.C { // closed once its buffer overflowed
		n++
	}
	if n != buffer {
		t.Fatalf("slow subscriber got %d buffered events before being dropped, want %d", n, buffer)
	}

	h.Send(user, live.Event{Name: "team", Data: []byte("x")})
	if ev, ok := <-fast.C; !ok || string(ev.Data) != "x" {
		t.Fatalf("fast subscriber stopped receiving: %v %v", ev, ok)
	}
	if got := h.Count(); got != 1 {
		t.Fatalf("count = %d, want 1", got)
	}
}

// TestHubCloseAndRemove checks that Close is idempotent, that closing the hub
// ends every stream, and that late subscribers start closed.
func TestHubCloseAndRemove(t *testing.T) {
	h := live.NewHub(testBuffer(t))
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
	b.Close() // safe after hub close

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
	h := live.NewHub(testBuffer(t))
	alice, bob := uuid.New(), uuid.New()
	a, b := h.Subscribe(alice), h.Subscribe(bob)

	h.Send(alice, live.Event{Name: "team", Data: []byte("{}")})
	if ev := <-a.C; ev.Name != "team" {
		t.Fatalf("alice got %v", ev)
	}
	select {
	case ev := <-b.C:
		t.Fatalf("bob got alice's event %v", ev)
	default:
	}
}
