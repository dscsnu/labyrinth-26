package live

import "bytes"

// Event is one server-sent event, encoded once into its wire frame so that
// sending it to many streams copies bytes rather than formatting them again.
// Build it with NewEvent; the zero Event is an empty frame that Serve skips.
type Event struct {
	name  string
	data  []byte
	frame []byte
}

// NewEvent returns the event name with payload data, normally compact JSON.
// A newline in data is sent as a second data line, which the client joins
// back with a newline, so any data round-trips.
func NewEvent(name string, data []byte) Event {
	frame := make([]byte, 0, len("event: \n")+len(name)+len("data: \n\n")+len(data))
	frame = append(frame, "event: "...)
	frame = append(frame, name...)
	frame = append(frame, '\n')
	for line := range bytes.Lines(data) {
		frame = append(frame, "data: "...)
		frame = append(frame, bytes.TrimSuffix(line, []byte("\n"))...)
		frame = append(frame, '\n')
	}
	if len(data) == 0 || data[len(data)-1] == '\n' {
		frame = append(frame, "data: \n"...)
	}
	frame = append(frame, '\n')
	return Event{name: name, data: data, frame: frame}
}

// Name returns the event's name, the SSE "event:" field.
func (e Event) Name() string { return e.name }

// Data returns the event's payload. The caller must not modify it.
func (e Event) Data() []byte { return e.data }
