package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestOpenAIStream checks the request headers and the events of a recorded
// text and tool call stream.
func TestOpenAIStream(t *testing.T) {
	var headers http.Header
	handler := serveStream(t, "openai_text_tool.sse", nil)
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		headers = r.Header.Clone()
		handler(w, r)
	})

	events, err := collect(t, p, Request{Messages: []Message{{Role: RoleUser, Content: []Block{{Type: BlockText, Text: "hi"}}}}})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	if got := headers.Get("Authorization"); got != "Bearer secret" {
		t.Errorf("authorization = %q, want %q", got, "Bearer secret")
	}

	checkEvents(t, events, []Event{
		{Type: EventText, Text: "Let me look"},
		{Type: EventText, Text: " at the file."},
		{Type: EventToolUse, ToolUse: &ToolUse{
			ID:    "call_9dR2",
			Name:  "read_file",
			Input: json.RawMessage(`{"path":"README.md"}`),
		}},
		{Type: EventDone, StopReason: StopToolUse, Usage: Usage{InputTokens: 412, OutputTokens: 57}},
	})
}

// TestOpenAIRequestBody checks the request body built from a history with an
// image and a tool result.
func TestOpenAIRequestBody(t *testing.T) {
	var body []byte
	p := newProvider(t, "openai", serveStream(t, "openai_text_tool.sse", &body))

	if _, err := collect(t, p, sampleRequest()); err != nil {
		t.Fatalf("Stream: %v", err)
	}

	checkJSON(t, body, `{
		"model": "test-model",
		"max_completion_tokens": 1024,
		"stream": true,
		"stream_options": {"include_usage": true},
		"messages": [
			{"role": "system", "content": "you are james"},
			{"role": "user", "content": [
				{"type": "text", "text": "what is on this screen?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,aGk="}}
			]},
			{"role": "assistant", "content": "I check the file.", "tool_calls": [
				{"id": "call_1", "type": "function", "function": {"name": "read_file", "arguments": "{\"path\":\"README.md\"}"}}
			]},
			{"role": "tool", "tool_call_id": "call_1", "content": "# title"},
			{"role": "user", "content": [
				{"type": "text", "text": "The following image is part of the result of tool call call_1."},
				{"type": "image_url", "image_url": {"url": "data:image/jpeg;base64,ZmFrZQ=="}}
			]},
			{"role": "user", "content": [{"type": "text", "text": "and now?"}]}
		],
		"tools": [{
			"type": "function",
			"function": {
				"name": "read_file",
				"description": "read a file",
				"parameters": {"type": "object", "properties": {"path": {"type": "string"}}}
			}
		}]
	}`)
}

// TestOpenAIFailedToolResult checks that a failed tool result becomes a tool
// message whose text opens with error:.
func TestOpenAIFailedToolResult(t *testing.T) {
	got, err := json.Marshal(openaiUserMessages(Message{Role: RoleUser, Content: []Block{{
		Type:      BlockToolResult,
		ToolUseID: "call_1",
		IsError:   true,
		Content:   []Block{{Type: BlockText, Text: "no such file"}},
	}}}))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	checkJSON(t, got, `[{"role": "tool", "tool_call_id": "call_1", "content": "error:\nno such file"}]`)
}

// TestOpenAIContextTooLong checks that the error code and the error message
// both lead to ErrContextTooLong.
func TestOpenAIContextTooLong(t *testing.T) {
	cases := map[string]string{
		"by code":    `{"error":{"message":"too long","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`,
		"by message": `{"error":{"message":"This model's maximum context length is 128000 tokens.","type":"invalid_request_error","param":"messages","code":null}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			p := newProvider(t, "openai", serveError(http.StatusBadRequest, body))
			if _, err := collect(t, p, Request{}); !errors.Is(err, ErrContextTooLong) {
				t.Fatalf("got %v, want ErrContextTooLong", err)
			}
		})
	}
}

// TestOpenAIStatusError checks that a failed call becomes an APIError with
// the status and the message.
func TestOpenAIStatusError(t *testing.T) {
	p := newProvider(t, "openai", serveError(http.StatusTooManyRequests,
		`{"error":{"message":"rate limit reached","type":"rate_limit_error","code":"rate_limit_exceeded"}}`))

	_, err := collect(t, p, Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("got %v, want an APIError", err)
	}
	if apiErr.Status != http.StatusTooManyRequests {
		t.Errorf("status = %d, want %d", apiErr.Status, http.StatusTooManyRequests)
	}
	if apiErr.Message != "rate limit reached" {
		t.Errorf("message = %q, want %q", apiErr.Message, "rate limit reached")
	}
}

// TestOpenAIStreamEndsWithoutUsage checks that a stream without a usage chunk
// still closes with an EventDone.
func TestOpenAIStreamEndsWithoutUsage(t *testing.T) {
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n" +
			`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
			"data: [DONE]\n\n"))
	})

	events, err := collect(t, p, Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	checkEvents(t, events, []Event{
		{Type: EventText, Text: "hello"},
		{Type: EventDone, StopReason: StopEndTurn},
	})
}

// TestOpenAIStreamEndsEarly checks that a stream ending without a finish reason
// and without [DONE] is reported as a provider failure.
func TestOpenAIStreamEndsEarly(t *testing.T) {
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"index":0,"delta":{"content":"hello"},"finish_reason":null}]}` + "\n\n"))
	})

	events, err := collect(t, p, Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	checkEvents(t, events, []Event{{Type: EventText, Text: "hello"}})
}

// TestOpenAIStreamError checks that an error object inside the stream is
// reported as a provider failure.
func TestOpenAIStreamError(t *testing.T) {
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"message":"The server is overloaded","type":"server_error"}}` + "\n\n"))
	})

	_, err := collect(t, p, Request{})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an APIError", err)
	}
	if !strings.Contains(apiErr.Message, "overloaded") {
		t.Errorf("message = %q, want the provider's text", apiErr.Message)
	}
}

// TestOpenAIInterleavedToolCalls checks that two tool calls whose pieces
// arrive interleaved are put together by their index, in order.
func TestOpenAIInterleavedToolCalls(t *testing.T) {
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_b","type":"function","function":{"name":"glob","arguments":""}}]},"finish_reason":null}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{\"pattern\":\"*.go\"}"}}]},"finish_reason":null}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":null}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
				"data: [DONE]\n\n"))
	})

	events, err := collect(t, p, Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	checkEvents(t, events, []Event{
		{Type: EventToolUse, ToolUse: &ToolUse{ID: "call_a", Name: "read_file", Input: json.RawMessage(`{"path":"a.txt"}`)}},
		{Type: EventToolUse, ToolUse: &ToolUse{ID: "call_b", Name: "glob", Input: json.RawMessage(`{"pattern":"*.go"}`)}},
		{Type: EventDone, StopReason: StopToolUse},
	})
}

// TestOpenAICutOffToolCall checks that a tool call cut off by the token limit
// is left out and the stop reason is max_tokens.
func TestOpenAICutOffToolCall(t *testing.T) {
	p := newProvider(t, "openai", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"READ"}}]},"finish_reason":null}]}` + "\n\n" +
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
				"data: [DONE]\n\n"))
	})

	events, err := collect(t, p, Request{})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	checkEvents(t, events, []Event{{Type: EventDone, StopReason: StopMaxTokens}})
}
