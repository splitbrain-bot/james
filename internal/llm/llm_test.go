package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// newProvider starts a test server with handler and builds a provider for it.
func newProvider(t *testing.T, provider string, handler http.HandlerFunc) Provider {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	p, err := NewProvider(Options{
		Provider: provider,
		BaseURL:  srv.URL,
		Model:    "test-model",
		APIKey:   "secret",
	})
	if err != nil {
		t.Fatalf("NewProvider: %v", err)
	}
	return p
}

// serveStream answers with the recorded stream from testdata. It stores the
// request body in body when body is not nil.
func serveStream(t *testing.T, name string, body *[]byte) http.HandlerFunc {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read recording: %v", err)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		got, _ := io.ReadAll(r.Body)
		if body != nil {
			*body = got
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(data)
	}
}

// serveError answers with the status and the body of a failed call.
func serveError(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// collect runs one call and returns the events the provider emitted.
func collect(t *testing.T, p Provider, req Request) ([]Event, error) {
	t.Helper()
	var events []Event
	err := p.Stream(context.Background(), req, func(ev Event) error {
		events = append(events, ev)
		return nil
	})
	return events, err
}

// sampleRequest returns a request whose history holds text, an image, a tool
// call and its result.
func sampleRequest() Request {
	return Request{
		System:    "you are james",
		MaxTokens: 1024,
		Tools: []Tool{{
			Name:        "read_file",
			Description: "read a file",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"}}}`),
		}},
		Messages: []Message{
			{Role: RoleUser, Content: []Block{
				{Type: BlockText, Text: "what is on this screen?"},
				{Type: BlockImage, MediaType: "image/png", Data: "aGk="},
			}},
			{Role: RoleAssistant, Content: []Block{
				{Type: BlockText, Text: "I check the file."},
				{Type: BlockToolUse, ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"README.md"}`)},
			}},
			{Role: RoleUser, Content: []Block{
				{Type: BlockToolResult, ToolUseID: "call_1", Content: []Block{
					{Type: BlockText, Text: "# title"},
					{Type: BlockImage, MediaType: "image/jpeg", Data: "ZmFrZQ=="},
				}},
				{Type: BlockText, Text: "and now?"},
			}},
		},
	}
}

// checkJSON fails the test when got and want are different JSON documents.
func checkJSON(t *testing.T, got []byte, want string) {
	t.Helper()
	var gotValue, wantValue any
	if err := json.Unmarshal(got, &gotValue); err != nil {
		t.Fatalf("request body is no JSON: %v: %s", err, got)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected body is no JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Errorf("request body\n got: %s\nwant: %s", got, want)
	}
}

// checkEvents fails the test when the events are not the expected ones. Tool
// inputs are compared as JSON documents.
func checkEvents(t *testing.T, got []Event, want []Event) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		g, w := got[i], want[i]
		if g.Type != w.Type || g.Text != w.Text || g.StopReason != w.StopReason || g.Usage != w.Usage {
			t.Errorf("event %d: got %+v, want %+v", i, g, w)
			continue
		}
		if (g.ToolUse == nil) != (w.ToolUse == nil) {
			t.Errorf("event %d: got tool use %v, want %v", i, g.ToolUse, w.ToolUse)
			continue
		}
		if g.ToolUse != nil {
			if g.ToolUse.ID != w.ToolUse.ID || g.ToolUse.Name != w.ToolUse.Name {
				t.Errorf("event %d: got tool use %+v, want %+v", i, g.ToolUse, w.ToolUse)
			}
			checkJSON(t, g.ToolUse.Input, string(w.ToolUse.Input))
		}
	}
}

// TestNewProviderUnknown checks that an unknown provider name is refused.
func TestNewProviderUnknown(t *testing.T) {
	if _, err := NewProvider(Options{Provider: "llama", Model: "m", APIKey: "k"}); err == nil {
		t.Fatal("expected an error for an unknown provider")
	}
}

// TestEndpoint checks that a base URL already ending in the version segment
// does not get it twice.
func TestEndpoint(t *testing.T) {
	cases := []struct {
		base string
		path string
		want string
	}{
		{"https://api.openai.com", "/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1", "/v1/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.anthropic.com", "/v1/messages", "https://api.anthropic.com/v1/messages"},
	}
	for _, c := range cases {
		if got := endpoint(c.base, c.path); got != c.want {
			t.Errorf("endpoint(%q, %q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}
