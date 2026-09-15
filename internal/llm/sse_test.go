package llm

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// TestSSEReader reads a stream with comments, multi-line data and a last
// event without a closing blank line.
func TestSSEReader(t *testing.T) {
	stream := ": keep alive\n" +
		"event: message\n" +
		"data: first\n" +
		"\n" +
		"data: line one\n" +
		"data: line two\n" +
		"id: 7\n" +
		"\n" +
		": another comment\n" +
		"\n" +
		"data:no space\r\n" +
		"\r\n" +
		"data: last without blank line\n"

	want := []sseEvent{
		{Name: "message", Data: "first"},
		{Data: "line one\nline two"},
		{Data: "no space"},
		{Data: "last without blank line"},
	}

	reader := newSSEReader(strings.NewReader(stream))
	for i, w := range want {
		got, err := reader.Next()
		if err != nil {
			t.Fatalf("event %d: %v", i, err)
		}
		if got != w {
			t.Errorf("event %d: got %+v, want %+v", i, got, w)
		}
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

// TestSSEReaderSkipsEmptyData checks that an event whose data is empty, a
// keep-alive of some servers, is skipped instead of returned.
func TestSSEReaderSkipsEmptyData(t *testing.T) {
	stream := "data:\n\ndata: \n\nevent: ping\ndata: real\n\n"

	reader := newSSEReader(strings.NewReader(stream))
	got, err := reader.Next()
	if err != nil {
		t.Fatalf("Next: %v", err)
	}
	if want := (sseEvent{Name: "ping", Data: "real"}); got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
	if _, err := reader.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want io.EOF", err)
	}
}
