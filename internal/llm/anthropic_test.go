package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestAnthropicStream checks the request headers and the events of a recorded
// text and tool call stream.
func TestAnthropicStream(t *testing.T) {
	var headers http.Header
	handler := serveStream(t, "anthropic_text_tool.sse", nil)
	p := newProvider(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		handler(w, r)
	})

	events, err := collect(t, p, Request{Messages: []Message{{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if got := headers.Get("X-Api-Key"); got != "secret" {
		t.Errorf("x-api-key = %q, want %q", got, "secret")
	}
	if got := headers.Get("Anthropic-Version"); got != anthropicVersion {
		t.Errorf("anthropic-version = %q, want %q", got, anthropicVersion)
	}

	checkEvents(t, events, []Event{
		{Type: EventText, Text: "Let me look"},
		{Type: EventText, Text: " at the file."},
		{Type: EventToolUse, ToolUse: &ToolUse{
			ID:    "toolu_01A09q90qw90lq917835lq9",
			Name:  "read_file",
			Input: json.RawMessage(`{"path":"README.md"}`),
		}},
		{Type: EventDone, StopReason: StopToolUse, Usage: Usage{InputTokens: 412, OutputTokens: 57}},
	})
}

// TestAnthropicRequestBody checks the request body built from a history with
// an image and a tool result.
func TestAnthropicRequestBody(t *testing.T) {
	var body []byte
	p := newProvider(t, "anthropic", serveStream(t, "anthropic_text_tool.sse", &body))

	if _, err := collect(t, p, sampleRequest()); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	checkJSON(t, body, `{
		"model": "test-model",
		"max_tokens": 1024,
		"system": "you are james",
		"stream": true,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what is on this screen?"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGk="}}
			]},
			{"role": "assistant", "content": [
				{"type": "text", "text": "I check the file."},
				{"type": "tool_use", "id": "call_1", "name": "read_file", "input": {"path": "README.md"}}
			]},
			{"role": "user", "content": [
				{"type": "tool_result", "tool_use_id": "call_1", "content": [
					{"type": "text", "text": "# title"},
					{"type": "image", "source": {"type": "base64", "media_type": "image/jpeg", "data": "ZmFrZQ=="}}
				]},
				{"type": "text", "text": "and now?"}
			]}
		],
		"tools": [{
			"name": "read_file",
			"description": "read a file",
			"input_schema": {"type": "object", "properties": {"path": {"type": "string"}}}
		}]
	}`)
}

// TestAnthropicFailedToolResult checks that a failed tool result keeps its
// error flag.
func TestAnthropicFailedToolResult(t *testing.T) {
	got, err := json.Marshal(anthropicBlocks([]Block{{
		Type:      BlockToolResult,
		ToolUseID: "call_1",
		IsError:   true,
		Content:   []Block{{Type: BlockText, Text: "no such file"}},
	}}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	checkJSON(t, got, `[{
		"type": "tool_result",
		"tool_use_id": "call_1",
		"is_error": true,
		"content": [{"type": "text", "text": "no such file"}]
	}]`)
}

// TestAnthropicContextTooLong checks that a refused too long prompt becomes
// ErrContextTooLong.
func TestAnthropicContextTooLong(t *testing.T) {
	p := newProvider(t, "anthropic", serveError(http.StatusBadRequest,
		`{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`))

	if _, err := collect(t, p, Request{}); !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("got %v, want ErrContextTooLong", err)
	}
}

// TestAnthropicStatusError checks that a failed call becomes an APIError with
// the status and the message.
func TestAnthropicStatusError(t *testing.T) {
	p := newProvider(t, "anthropic", serveError(http.StatusInternalServerError,
		`{"type":"error","error":{"type":"api_error","message":"internal server error"}}`))

	_, err := collect(t, p, Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want an APIError", err)
	}
	if apiErr.Status != http.StatusInternalServerError {
		t.Errorf("status = %d, want %d", apiErr.Status, http.StatusInternalServerError)
	}
	if apiErr.Message != "internal server error" {
		t.Errorf("message = %q, want %q", apiErr.Message, "internal server error")
	}
}

// TestAnthropicStreamEndsEarly checks that a stream ending before message_stop
// is reported as a provider failure instead of a finished answer.
func TestAnthropicStreamEndsEarly(t *testing.T) {
	p := newProvider(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}` + "\n\n"))
	})

	events, err := collect(t, p, Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	checkEvents(t, events, []Event{{Type: EventText, Text: "hello"}})
}

// TestAnthropicStreamError checks that an error event inside the stream is
// reported as a provider failure.
func TestAnthropicStreamError(t *testing.T) {
	p := newProvider(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: error\n" +
			`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"))
	})

	_, err := collect(t, p, Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	if !strings.Contains(apiErr.Message, "Overloaded") {
		t.Errorf("message = %q, want the provider's text", apiErr.Message)
	}
}

// TestAnthropicCutOffToolCall checks that a tool call cut off by the token
// limit is left out and the stop reason is max_tokens.
func TestAnthropicCutOffToolCall(t *testing.T) {
	p := newProvider(t, "anthropic", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: content_block_start\n" +
			`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"READ"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":0}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":9}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"))
	})

	events, err := collect(t, p, Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	checkEvents(t, events, []Event{
		{Type: EventDone, StopReason: StopMaxTokens, Usage: Usage{OutputTokens: 9}},
	})
}
