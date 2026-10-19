package live

import (
	"encoding/json"
	"fmt"
)

// EventType is why an event was sent: the cause of the change, such as
// "spell_cast" or "member_joined", or "snapshot" for the state a stream opens
// with. It travels as the "type" field of every event's data. The publishing
// use case declares its values as constants; the payload itself is always
// the full state, whatever the type.
type EventType string

// The two types every resource uses. Use-case specific types (the causes of
// changes) are declared by the use case that publishes them.
const (
	// Snapshot is the state a stream opens with, on every connect: not a
	// change, the current truth.
	Snapshot EventType = "snapshot"
	// Resync is the state reloaded from Postgres after a write failed to
	// persist: not an action, a correction the client applies like any other
	// snapshot.
	Resync EventType = "resync"
)

// Version orders the snapshots of one resource (one team's "team", or its
// "team_state") so a client can keep the newest copy whatever order copies
// arrive in, from the stream or from write responses. Seq goes up by one
// with every change; Epoch identifies the backend process, so a restart,
// which starts Seq again, is never mistaken for going back. A client keeps an
// incoming copy if its Epoch differs from the one it holds, or if the Epoch
// matches and Seq is higher. Two numbers rather than one, because JavaScript
// can't hold integers above 2^53 exactly.
type Version struct {
	// Epoch is the backend process's start time, in Unix milliseconds.
	Epoch int64 `json:"epoch"`
	// Seq counts the resource's changes since the process started.
	Seq uint64 `json:"seq"`
}

// Next returns the version after v, in epoch.
func (v Version) Next(epoch int64) Version { return Version{Epoch: epoch, Seq: v.Seq + 1} }

// envelope is every event's data: why it was sent, and the full payload.
type envelope struct {
	Type    EventType `json:"type"`
	Message any       `json:"message"`
}

// Event is one server-sent event, encoded once into its wire frame so that
// sending it to many streams copies bytes rather than formatting them again.
// Its data is always the envelope {"type": …, "message": …}. Build it with
// NewEvent; the zero Event is an empty frame that Serve skips.
type Event struct {
	name  string
	typ   EventType
	data  []byte
	frame []byte
}

// NewEvent returns the event name whose data is {"type": typ, "message":
// message}, with message encoded as JSON (a json.RawMessage is passed
// through as is). It fails only if message can't be encoded.
func NewEvent(name string, typ EventType, message any) (Event, error) {
	data, err := json.Marshal(envelope{Type: typ, Message: message})
	if err != nil {
		return Event{}, fmt.Errorf("encode %s event: %w", name, err)
	}
	return Event{name: name, typ: typ, data: data, frame: frame(name, data)}, nil
}

// MustNewEvent is NewEvent for a message known to encode (a constant
// payload); it panics if encoding fails.
func MustNewEvent(name string, typ EventType, message any) Event {
	ev, err := NewEvent(name, typ, message)
	if err != nil {
		panic(err)
	}
	return ev
}

// frame builds the wire bytes "event: name\ndata: data\n\n". data is JSON
// from encoding/json, which is compact and so never holds a newline: one
// data line always carries it.
func frame(name string, data []byte) []byte {
	f := make([]byte, 0, len("event: \ndata: \n\n")+len(name)+len(data))
	f = append(f, "event: "...)
	f = append(f, name...)
	f = append(f, "\ndata: "...)
	f = append(f, data...)
	return append(f, "\n\n"...)
}

// Name returns the event's name, the SSE "event:" field: the resource
// ("team", "team_state").
func (e Event) Name() string { return e.name }

// Type returns why the event was sent, the envelope's "type".
func (e Event) Type() EventType { return e.typ }

// Data returns the event's data, the whole envelope. The caller must not
// modify it.
func (e Event) Data() []byte { return e.data }
