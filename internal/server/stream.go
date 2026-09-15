package server

import (
	"encoding/json"
	"fmt"
	"net/http"
)

// stream writes Server-Sent Events to one response and flushes every event. It
// is the sink of the agent loop. After the first write failure it stops writing
// and returns that failure to every later call. Data that cannot be encoded
// fails only its own event, because the connection is still good.
type stream struct {
	// writer is the response the events go to.
	writer http.ResponseWriter
	// control flushes the response.
	control *http.ResponseController
	// failure is the first write failure, if any.
	failure error
}

// newStream starts the event stream on w and flushes the response headers.
func newStream(w http.ResponseWriter) *stream {
	s := &stream{writer: w, control: http.NewResponseController(w)}
	s.control.Flush()
	return s
}

// send writes one event with its JSON data and flushes it.
func (s *stream) send(event string, data any) error {
	if s.failure != nil {
		return s.failure
	}
	payload, err := json.Marshal(data)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(s.writer, "event: %s\ndata: %s\n\n", event, payload); err != nil {
		s.failure = err
		return err
	}
	if err := s.control.Flush(); err != nil {
		s.failure = err
		return err
	}
	return nil
}

// Text delivers a piece of answer text.
func (s *stream) Text(text string) error {
	return s.send("text", textEvent{Text: text})
}

// ToolStart announces that a server tool is about to run. An input that is not
// valid JSON is sent as null, so a broken tool call cannot break the event.
func (s *stream) ToolStart(id, name string, input json.RawMessage) error {
	if !json.Valid(input) {
		input = json.RawMessage("null")
	}
	return s.send("tool_start", toolStartEvent{ID: id, Name: name, Input: input})
}

// ToolEnd reports that a server tool finished.
func (s *stream) ToolEnd(id, name, output string, isError bool) error {
	return s.send("tool_end", toolEndEvent{ID: id, Name: name, Output: output, IsError: isError})
}
