package llm

import (
	"bufio"
	"io"
	"strings"
)

// maxSSELineBytes caps the length of one line in a stream. One line can hold a
// whole tool input.
const maxSSELineBytes = 4 << 20

// sseEvent is one event of a Server-Sent Events stream.
type sseEvent struct {
	// Name is the value of the event field. It is empty when the stream does
	// not name its events.
	Name string
	// Data is the data of the event. Several data fields are joined with
	// newlines.
	Data string
}

// sseReader reads events from a Server-Sent Events stream.
type sseReader struct {
	// sc reads the stream line by line.
	sc *bufio.Scanner
}

// newSSEReader returns a reader for the stream r.
func newSSEReader(r io.Reader) *sseReader {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)
	return &sseReader{sc: sc}
}

// Next returns the next event. It skips comments, unknown fields and events
// whose data is empty, which some servers send to keep the connection alive.
// It returns io.EOF once the stream ends.
func (r *sseReader) Next() (sseEvent, error) {
	var ev sseEvent
	var data []string

	for r.sc.Scan() {
		line := strings.TrimSuffix(r.sc.Text(), "\r")
		switch {
		case line == "":
			if joined := strings.Join(data, "\n"); joined != "" {
				ev.Data = joined
				return ev, nil
			}
			ev = sseEvent{}
			data = nil
		case strings.HasPrefix(line, ":"):
			// a comment, usually a keep-alive
		default:
			field, value, _ := strings.Cut(line, ":")
			value = strings.TrimPrefix(value, " ")
			switch field {
			case "event":
				ev.Name = value
			case "data":
				data = append(data, value)
			}
		}
	}
	if err := r.sc.Err(); err != nil {
		return sseEvent{}, err
	}

	// the stream ended without the blank line that closes the last event
	if joined := strings.Join(data, "\n"); joined != "" {
		ev.Data = joined
		return ev, nil
	}
	return sseEvent{}, io.EOF
}
