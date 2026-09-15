package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"james/internal/agent"
	"james/internal/config"
	"james/internal/llm"
	"james/web"
)

// testSecret is the key the test tokens are signed with.
const testSecret = "test secret"

// fakeProvider answers every call with one piece of text and then stops.
type fakeProvider struct {
	// text is the answer the provider streams.
	text string
	// err makes Stream fail instead of answering.
	err error
}

// Stream emits the configured text and then the done event.
func (p *fakeProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.Event) error) error {
	if p.err != nil {
		return p.err
	}
	if err := emit(llm.Event{Type: llm.EventText, Text: p.text}); err != nil {
		return err
	}
	return emit(llm.Event{
		Type:       llm.EventDone,
		StopReason: llm.StopEndTurn,
		Usage:      llm.Usage{InputTokens: 12, OutputTokens: 34},
	})
}

// toolProvider asks for one tool call on the first request and answers with
// text on the second.
type toolProvider struct {
	// calls counts the requests seen so far.
	calls int
}

// Stream emits a tool call first, then a text answer.
func (p *toolProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.Event) error) error {
	p.calls++
	if p.calls == 1 {
		if err := emit(llm.Event{Type: llm.EventToolUse, ToolUse: &llm.ToolUse{
			ID: "call-1", Name: "echo", Input: json.RawMessage(`{"text":"hi"}`),
		}}); err != nil {
			return err
		}
		return emit(llm.Event{Type: llm.EventDone, StopReason: llm.StopToolUse})
	}
	if err := emit(llm.Event{Type: llm.EventText, Text: "done"}); err != nil {
		return err
	}
	return emit(llm.Event{Type: llm.EventDone, StopReason: llm.StopEndTurn})
}

// echoTool answers every call with a fixed text.
type echoTool struct{}

// Name returns the tool name.
func (echoTool) Name() string { return "echo" }

// Description returns a fixed description.
func (echoTool) Description() string { return "echoes" }

// Schema returns an empty object schema.
func (echoTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// Run returns a fixed text.
func (echoTool) Run(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	return agent.Result{Text: "echoed"}, nil
}

// event is one parsed Server-Sent Event.
type event struct {
	// name is the event name.
	name string
	// data is the JSON payload.
	data string
}

// parseEvents reads all events of an event stream body.
func parseEvents(t *testing.T, body string) []event {
	t.Helper()
	var events []event
	for _, block := range strings.Split(strings.TrimSpace(body), "\n\n") {
		if block == "" {
			continue
		}
		var e event
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "event: "):
				e.name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				e.data = strings.TrimPrefix(line, "data: ")
			}
		}
		events = append(events, e)
	}
	return events
}

// names lists the names of the given events.
func names(events []event) []string {
	var out []string
	for _, e := range events {
		out = append(out, e.name)
	}
	return out
}

// token builds a token for the given user and expiry.
func token(t *testing.T, sub string, exp time.Time) string {
	t.Helper()
	encode := base64.RawURLEncoding.EncodeToString
	payload, err := json.Marshal(map[string]any{"sub": sub, "name": "Anna", "exp": exp.Unix()})
	if err != nil {
		t.Fatal(err)
	}
	signed := encode([]byte(`{"alg":"HS256","typ":"JWT"}`)) + "." + encode(payload)
	mac := hmac.New(sha256.New, []byte(testSecret))
	mac.Write([]byte(signed))
	return signed + "." + encode(mac.Sum(nil))
}

// newTestServer builds a handler with the given base path and provider. The
// agent has the given tools.
func newTestServer(t *testing.T, basePath string, provider llm.Provider, tools ...agent.Tool) http.Handler {
	t.Helper()
	cfg := &config.Config{}
	cfg.Server.BasePath = basePath
	cfg.Server.AllowedOrigins = []string{"https://app.example.com"}
	cfg.Server.MaxBodyBytes = 4096
	cfg.Server.MaxImageBytes = 1024
	cfg.Auth.Secret = testSecret
	cfg.Tools.Browser = []string{"read_page"}
	ag := &agent.Agent{Provider: provider, SystemPrompt: "be nice", Tools: tools, MaxSteps: 3, MaxTokens: 100}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	handler, err := New(cfg, ag, logger)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return handler
}

// userMessage builds a history with one user message.
func userMessage(text string) []llm.Message {
	return []llm.Message{{
		Role:    llm.RoleUser,
		Content: []llm.Block{{Type: llm.BlockText, Text: text}},
	}}
}

// postChat sends one chat request and returns the response.
func postChat(t *testing.T, handler http.Handler, path string, body any, origin string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	switch value := body.(type) {
	case string:
		raw = []byte(value)
	default:
		var err error
		raw, err = json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	return rec
}

// TestChatStreamsAnswer covers a turn that streams answer text and ends with
// a done event.
func TestChatStreamsAnswer(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{text: "hello there"})
	rec := postChat(t, handler, "/chat", chatRequest{
		Token:    token(t, "u1", time.Now().Add(time.Hour)),
		Messages: userMessage("hi"),
	}, "")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Errorf("content type = %q", got)
	}
	events := parseEvents(t, rec.Body.String())
	if len(events) != 2 || events[0].name != "text" || events[1].name != "done" {
		t.Fatalf("events = %v", names(events))
	}

	var text textEvent
	if err := json.Unmarshal([]byte(events[0].data), &text); err != nil {
		t.Fatal(err)
	}
	if text.Text != "hello there" {
		t.Errorf("text = %q", text.Text)
	}

	var done doneEvent
	if err := json.Unmarshal([]byte(events[1].data), &done); err != nil {
		t.Fatal(err)
	}
	if done.StopReason != llm.StopEndTurn {
		t.Errorf("stop_reason = %q", done.StopReason)
	}
	if len(done.Messages) != 1 || done.Messages[0].Role != llm.RoleAssistant {
		t.Errorf("messages = %+v", done.Messages)
	}
	if done.Usage.InputTokens != 12 || done.Usage.OutputTokens != 34 {
		t.Errorf("usage = %+v", done.Usage)
	}
	if !strings.Contains(events[1].data, `"browser_tools":[]`) {
		t.Errorf("done data = %s", events[1].data)
	}
}

// TestChatRejectsToken covers a missing, a broken and an expired token.
func TestChatRejectsToken(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{text: "hello"})
	cases := map[string]string{
		"missing": "",
		"invalid": "a.b.c",
		"expired": token(t, "u1", time.Now().Add(-time.Hour)),
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			rec := postChat(t, handler, "/chat", chatRequest{Token: value, Messages: userMessage("hi")}, "")
			events := parseEvents(t, rec.Body.String())
			if len(events) != 1 || events[0].name != "error" {
				t.Fatalf("events = %v", names(events))
			}
			var failure errorEvent
			if err := json.Unmarshal([]byte(events[0].data), &failure); err != nil {
				t.Fatal(err)
			}
			if failure.Code != "unauthorized" {
				t.Errorf("code = %q", failure.Code)
			}
		})
	}
}

// TestChatRejectsBadRequests covers the bodies the server refuses.
func TestChatRejectsBadRequests(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{text: "hello"})
	valid := token(t, "u1", time.Now().Add(time.Hour))

	cases := []struct {
		name string
		body any
	}{
		{"no JSON", "this is not JSON"},
		{"no messages", chatRequest{Token: valid}},
		{"last message from the assistant", chatRequest{
			Token: valid,
			Messages: []llm.Message{{
				Role:    llm.RoleAssistant,
				Content: []llm.Block{{Type: llm.BlockText, Text: "hi"}},
			}},
		}},
		{"body too large", chatRequest{Token: valid, Messages: userMessage(strings.Repeat("x", 5000))}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := postChat(t, handler, "/chat", c.body, "")
			events := parseEvents(t, rec.Body.String())
			if len(events) != 1 || events[0].name != "error" {
				t.Fatalf("events = %v", names(events))
			}
			var failure errorEvent
			if err := json.Unmarshal([]byte(events[0].data), &failure); err != nil {
				t.Fatal(err)
			}
			if failure.Code != "bad_request" {
				t.Errorf("code = %q", failure.Code)
			}
		})
	}
}

// TestChatReportsContextTooLong covers a turn that outgrows the context window.
func TestChatReportsContextTooLong(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{err: llm.ErrContextTooLong})
	rec := postChat(t, handler, "/chat", chatRequest{
		Token:    token(t, "u1", time.Now().Add(time.Hour)),
		Messages: userMessage("hi"),
	}, "")

	events := parseEvents(t, rec.Body.String())
	if len(events) != 1 || events[0].name != "error" {
		t.Fatalf("events = %v", names(events))
	}
	var failure errorEvent
	if err := json.Unmarshal([]byte(events[0].data), &failure); err != nil {
		t.Fatal(err)
	}
	if failure.Code != "context_too_long" {
		t.Errorf("code = %q", failure.Code)
	}
}

// TestChatCORS covers which origins the chat route accepts.
func TestChatCORS(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{text: "hello"})
	body := chatRequest{Token: token(t, "u1", time.Now().Add(time.Hour)), Messages: userMessage("hi")}

	t.Run("allowed origin", func(t *testing.T) {
		rec := postChat(t, handler, "/chat", body, "https://app.example.com")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
			t.Errorf("allow origin = %q", got)
		}
		if got := rec.Header().Get("Vary"); got != "Origin" {
			t.Errorf("vary = %q", got)
		}
	})

	t.Run("own origin", func(t *testing.T) {
		rec := postChat(t, handler, "/chat", body, "http://example.com")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("own host with another scheme", func(t *testing.T) {
		rec := postChat(t, handler, "/chat", body, "https://example.com")
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("other origin", func(t *testing.T) {
		rec := postChat(t, handler, "/chat", body, "https://evil.example.com")
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
	})
}

// TestChatPreflight covers the preflight of an allowed and a denied origin.
func TestChatPreflight(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{text: "hello"})

	t.Run("allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/chat", nil)
		req.Header.Set("Origin", "https://app.example.com")
		req.Header.Set("Access-Control-Request-Method", "POST")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
			t.Errorf("allow origin = %q", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Methods"); !strings.Contains(got, "POST") {
			t.Errorf("allow methods = %q", got)
		}
	})

	t.Run("denied", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodOptions, "/chat", nil)
		req.Header.Set("Origin", "https://evil.example.com")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)

		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d", rec.Code)
		}
		if rec.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Error("the denied origin was echoed")
		}
	})
}

// TestRoutes covers the routes under the root base path.
func TestRoutes(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{text: "hello"})

	t.Run("healthz", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
			t.Fatalf("status = %d, body = %q", rec.Code, rec.Body.String())
		}
	})

	t.Run("popup page", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Header().Get("Cache-Control"); got != "no-store" {
			t.Errorf("cache control = %q", got)
		}
		body := rec.Body.String()
		if strings.Contains(body, configPlaceholder) {
			t.Error("the placeholder was not replaced")
		}
		if !strings.Contains(body, `"allowedOrigins":["https://app.example.com"]`) {
			t.Errorf("settings missing in %q", body)
		}
		if !strings.Contains(body, `"maxImageBytes":1024`) {
			t.Errorf("settings missing in %q", body)
		}
	})

	t.Run("widget script", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/james.js", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/javascript") {
			t.Errorf("content type = %q", got)
		}
		if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("allow origin = %q", got)
		}
		body := rec.Body.String()
		if strings.Contains(body, toolsPlaceholder) {
			t.Error("the placeholder was not replaced")
		}
		if !strings.Contains(body, `"read_page"`) {
			t.Error("the browser tool is missing in the script")
		}
	})
}

// TestBasePathRouting covers the routes under a base path with a prefix.
func TestBasePathRouting(t *testing.T) {
	handler := newTestServer(t, "/agent", &fakeProvider{text: "hello"})

	t.Run("healthz", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/healthz", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("root is not served", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if rec.Code != http.StatusNotFound {
			t.Fatalf("status = %d", rec.Code)
		}
	})

	t.Run("base path redirects", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent", nil))
		if rec.Code != http.StatusMovedPermanently {
			t.Fatalf("status = %d", rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/agent/" {
			t.Errorf("location = %q", got)
		}
	})

	t.Run("popup page", func(t *testing.T) {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/agent/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d", rec.Code)
		}
		if strings.Contains(rec.Body.String(), configPlaceholder) {
			t.Error("the placeholder was not replaced")
		}
	})

	t.Run("chat", func(t *testing.T) {
		rec := postChat(t, handler, "/agent/chat", chatRequest{
			Token:    token(t, "u1", time.Now().Add(time.Hour)),
			Messages: userMessage("hi"),
		}, "")
		events := parseEvents(t, rec.Body.String())
		if len(events) != 2 || events[1].name != "done" {
			t.Fatalf("events = %v", names(events))
		}
	})
}

// anyStaticFile returns the path of one embedded static file.
func anyStaticFile(t *testing.T) string {
	t.Helper()
	var found string
	err := fs.WalkDir(web.Files, "static", func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() && found == "" {
			found = path
		}
		return nil
	})
	if err != nil || found == "" {
		t.Skip("no static file is embedded yet")
	}
	return found
}

// TestStaticFiles covers an embedded static file under both base paths.
func TestStaticFiles(t *testing.T) {
	file := anyStaticFile(t)
	for _, basePath := range []string{"/", "/agent"} {
		t.Run(basePath, func(t *testing.T) {
			handler := newTestServer(t, basePath, &fakeProvider{text: "hello"})
			path := strings.TrimSuffix(basePath, "/") + "/" + file
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("GET %s = %d", path, rec.Code)
			}
			// the widget reads its icon and its texts from a host page
			if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
				t.Errorf("allow origin = %q", got)
			}
		})
	}
}

// TestChatOverRealConnection covers a turn over a real listener, where the HTTP
// server closes an unread request body once the response starts.
func TestChatOverRealConnection(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{text: "hello"})
	srv := httptest.NewServer(handler)
	defer srv.Close()

	raw, err := json.Marshal(chatRequest{
		Token:    token(t, "u1", time.Now().Add(time.Hour)),
		Messages: userMessage("hi"),
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(srv.URL+"/chat", "application/json", strings.NewReader(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := parseEvents(t, string(body))
	if len(events) != 2 || events[0].name != "text" || events[1].name != "done" {
		t.Fatalf("events = %v (%s)", names(events), body)
	}
}

// TestChatStreamsToolEvents covers the tool_start and tool_end events of a
// server tool that runs inside the turn.
func TestChatStreamsToolEvents(t *testing.T) {
	handler := newTestServer(t, "/", &toolProvider{}, echoTool{})
	rec := postChat(t, handler, "/chat", chatRequest{
		Token:    token(t, "u1", time.Now().Add(time.Hour)),
		Messages: userMessage("hi"),
	}, "")
	events := parseEvents(t, rec.Body.String())
	want := []string{"tool_start", "tool_end", "text", "done"}
	if strings.Join(names(events), ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", names(events), want)
	}
	var end toolEndEvent
	if err := json.Unmarshal([]byte(events[1].data), &end); err != nil {
		t.Fatal(err)
	}
	if end.ID != "call-1" || end.Name != "echo" || end.Output != "echoed" || end.IsError {
		t.Errorf("tool_end = %+v", end)
	}
	var done doneEvent
	if err := json.Unmarshal([]byte(events[3].data), &done); err != nil {
		t.Fatal(err)
	}
	if len(done.Messages) != 3 {
		t.Errorf("done carries %d messages, want the call, its result and the answer", len(done.Messages))
	}
}

// TestStaticStaysInStaticDir checks that a path with .. under static cannot
// reach files outside the static directory.
func TestStaticStaysInStaticDir(t *testing.T) {
	handler := newTestServer(t, "/", &fakeProvider{text: "hello"})
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/%2e%2e/james.js", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET /static/%%2e%%2e/james.js = %d, want 404", rec.Code)
	}
}
