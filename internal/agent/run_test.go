package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"james/internal/llm"
)

// fakeProvider replays one scripted response per call, repeating the last one,
// and records every request it received.
type fakeProvider struct {
	// responses holds the events of each call, in order.
	responses [][]llm.Event
	// err is returned by Stream instead of any events when it is set.
	err error
	// requests collects the requests Stream was called with.
	requests []llm.Request
}

// Stream records the request and emits the events of the current response.
func (p *fakeProvider) Stream(ctx context.Context, req llm.Request, emit func(llm.Event) error) error {
	p.requests = append(p.requests, req)
	if p.err != nil {
		return p.err
	}
	if len(p.responses) == 0 {
		return errors.New("fake provider has no response")
	}
	index := len(p.requests) - 1
	if index >= len(p.responses) {
		index = len(p.responses) - 1
	}
	for _, event := range p.responses[index] {
		if err := emit(event); err != nil {
			return err
		}
	}
	return nil
}

// sinkCall is one tool event a recordSink saw.
type sinkCall struct {
	// id is the tool call ID.
	id string
	// name is the tool name.
	name string
	// output is the summary of a ToolEnd call.
	output string
	// isError marks a failed tool in a ToolEnd call.
	isError bool
}

// recordSink keeps everything the turn sent to it.
type recordSink struct {
	// text is all answer text, joined.
	text strings.Builder
	// starts holds the ToolStart calls.
	starts []sinkCall
	// ends holds the ToolEnd calls.
	ends []sinkCall
}

// Text records a piece of answer text.
func (s *recordSink) Text(text string) error {
	s.text.WriteString(text)
	return nil
}

// ToolStart records a starting tool.
func (s *recordSink) ToolStart(id, name string, input json.RawMessage) error {
	s.starts = append(s.starts, sinkCall{id: id, name: name})
	return nil
}

// ToolEnd records a finished tool.
func (s *recordSink) ToolEnd(id, name, output string, isError bool) error {
	s.ends = append(s.ends, sinkCall{id: id, name: name, output: output, isError: isError})
	return nil
}

// fakeTool answers every call with a fixed result or a fixed error.
type fakeTool struct {
	// name is the tool name.
	name string
	// result is returned when err is nil.
	result Result
	// err is returned instead of result when it is set.
	err error
	// inputs collects the inputs the tool was called with.
	inputs []string
}

// Name returns the tool name.
func (t *fakeTool) Name() string { return t.name }

// Description returns a fixed description.
func (t *fakeTool) Description() string { return "does nothing, for tests" }

// Schema returns a fixed empty object schema.
func (t *fakeTool) Schema() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }

// Run records the input and returns the fixed result or error.
func (t *fakeTool) Run(ctx context.Context, input json.RawMessage) (Result, error) {
	t.inputs = append(t.inputs, string(input))
	return t.result, t.err
}

// fakeBrowserTool is a tool the popup runs.
type fakeBrowserTool struct {
	// fakeTool supplies the tool methods.
	fakeTool
}

// RunsInBrowser marks the tool as one the popup runs.
func (t *fakeBrowserTool) RunsInBrowser() {}

// textEvent builds a text event.
func textEvent(text string) llm.Event {
	return llm.Event{Type: llm.EventText, Text: text}
}

// toolEvent builds a tool call event.
func toolEvent(id, name, input string) llm.Event {
	return llm.Event{Type: llm.EventToolUse, ToolUse: &llm.ToolUse{
		ID:    id,
		Name:  name,
		Input: json.RawMessage(input),
	}}
}

// doneEvent builds the closing event of a stream.
func doneEvent(stop llm.StopReason, in, out int) llm.Event {
	return llm.Event{
		Type:       llm.EventDone,
		StopReason: stop,
		Usage:      llm.Usage{InputTokens: in, OutputTokens: out},
	}
}

// userSays builds a user message with one text block.
func userSays(text string) []llm.Message {
	return []llm.Message{{Role: llm.RoleUser, Content: []llm.Block{{Type: llm.BlockText, Text: text}}}}
}

// TestRunTextOnly covers a turn that ends with answer text and no tool call.
func TestRunTextOnly(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{{
		textEvent("Hello "),
		textEvent("world"),
		doneEvent(llm.StopEndTurn, 10, 5),
	}}}
	sink := &recordSink{}
	a := &Agent{Provider: provider, MaxSteps: 5}

	out, err := a.Run(context.Background(), userSays("hi"), nil, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if out.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", out.StopReason, llm.StopEndTurn)
	}
	if len(out.Messages) != 1 {
		t.Fatalf("got %d messages, want 1", len(out.Messages))
	}
	message := out.Messages[0]
	if message.Role != llm.RoleAssistant {
		t.Errorf("role = %q, want %q", message.Role, llm.RoleAssistant)
	}
	if len(message.Content) != 1 || message.Content[0].Text != "Hello world" {
		t.Errorf("content = %+v, want one text block with the whole answer", message.Content)
	}
	if sink.text.String() != "Hello world" {
		t.Errorf("sink text = %q, want %q", sink.text.String(), "Hello world")
	}
	if out.Usage != (llm.Usage{InputTokens: 10, OutputTokens: 5}) {
		t.Errorf("usage = %+v", out.Usage)
	}
	if len(out.ToolsCalled) != 0 {
		t.Errorf("tools called = %v, want none", out.ToolsCalled)
	}
}

// TestRunServerToolThenAnswer covers a server tool whose result is fed back and
// answered in a second model call.
func TestRunServerToolThenAnswer(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{
		{
			textEvent("let me look"),
			toolEvent("call-1", "read_file", `{"path":"a.txt"}`),
			doneEvent(llm.StopToolUse, 10, 5),
		},
		{
			textEvent("the file says hello"),
			doneEvent(llm.StopEndTurn, 20, 7),
		},
	}}
	tool := &fakeTool{name: "read_file", result: Result{Text: "hello"}}
	sink := &recordSink{}
	a := &Agent{Provider: provider, Tools: []Tool{tool}, MaxSteps: 5}

	out, err := a.Run(context.Background(), userSays("what is in a.txt?"), nil, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(provider.requests) != 2 {
		t.Fatalf("got %d provider calls, want 2", len(provider.requests))
	}
	second := provider.requests[1].Messages
	if len(second) != 3 {
		t.Fatalf("second request had %d messages, want 3", len(second))
	}
	results := second[2]
	if results.Role != llm.RoleUser || len(results.Content) != 1 {
		t.Fatalf("last message = %+v, want one user message with one block", results)
	}
	result := results.Content[0]
	if result.Type != llm.BlockToolResult || result.ToolUseID != "call-1" {
		t.Errorf("result block = %+v, want a tool_result for call-1", result)
	}
	if len(result.Content) != 1 || result.Content[0].Text != "hello" {
		t.Errorf("result content = %+v, want the tool text", result.Content)
	}
	if result.IsError {
		t.Error("result is marked as an error")
	}

	if len(sink.starts) != 1 || sink.starts[0].name != "read_file" || sink.starts[0].id != "call-1" {
		t.Errorf("tool starts = %+v", sink.starts)
	}
	if len(sink.ends) != 1 || sink.ends[0].output != "hello" || sink.ends[0].isError {
		t.Errorf("tool ends = %+v", sink.ends)
	}
	if len(out.ToolsCalled) != 1 || out.ToolsCalled[0] != "read_file" {
		t.Errorf("tools called = %v, want [read_file]", out.ToolsCalled)
	}
	if out.Usage != (llm.Usage{InputTokens: 30, OutputTokens: 12}) {
		t.Errorf("usage = %+v, want the sum of both calls", out.Usage)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("got %d messages, want 3", len(out.Messages))
	}
	if out.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", out.StopReason, llm.StopEndTurn)
	}
	if len(out.BrowserTools) != 0 {
		t.Errorf("browser tools = %+v, want none", out.BrowserTools)
	}
}

// TestRunToolError covers a failing server tool, reported to the model as a
// failed result.
func TestRunToolError(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{
		{
			toolEvent("call-1", "read_file", `{"path":"gone.txt"}`),
			doneEvent(llm.StopToolUse, 1, 1),
		},
		{
			textEvent("that file is missing"),
			doneEvent(llm.StopEndTurn, 1, 1),
		},
	}}
	tool := &fakeTool{name: "read_file", err: errors.New("no such file")}
	sink := &recordSink{}
	a := &Agent{Provider: provider, Tools: []Tool{tool}, MaxSteps: 5}

	out, err := a.Run(context.Background(), userSays("read gone.txt"), nil, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(provider.requests) != 2 {
		t.Fatalf("got %d provider calls, want 2", len(provider.requests))
	}
	last := provider.requests[1].Messages
	result := last[len(last)-1].Content[0]
	if !result.IsError {
		t.Error("result is not marked as an error")
	}
	if len(result.Content) != 1 || !strings.Contains(result.Content[0].Text, "no such file") {
		t.Errorf("result content = %+v, want the error text", result.Content)
	}
	if len(sink.ends) != 1 || !sink.ends[0].isError {
		t.Errorf("tool ends = %+v, want one failed tool", sink.ends)
	}
	if out.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", out.StopReason, llm.StopEndTurn)
	}
}

// TestRunBrowserToolPending covers a turn that ends with a pending browser tool
// next to a finished server tool.
func TestRunBrowserToolPending(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{{
		textEvent("checking the page"),
		toolEvent("call-1", "read_file", `{"path":"a.txt"}`),
		toolEvent("call-2", "read_page", `{}`),
		doneEvent(llm.StopToolUse, 3, 4),
	}}}
	server := &fakeTool{name: "read_file", result: Result{Text: "hello"}}
	browser := &fakeBrowserTool{fakeTool{name: "read_page"}}
	sink := &recordSink{}
	a := &Agent{Provider: provider, Tools: []Tool{server, browser}, MaxSteps: 5}

	out, err := a.Run(context.Background(), userSays("what is on the page?"), nil, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(provider.requests) != 1 {
		t.Fatalf("got %d provider calls, want 1", len(provider.requests))
	}
	if len(out.BrowserTools) != 1 || out.BrowserTools[0].Name != "read_page" || out.BrowserTools[0].ID != "call-2" {
		t.Fatalf("browser tools = %+v, want the read_page call", out.BrowserTools)
	}
	if len(out.ToolResults) != 1 || out.ToolResults[0].ToolUseID != "call-1" {
		t.Fatalf("tool results = %+v, want the read_file result", out.ToolResults)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != llm.RoleAssistant {
		t.Fatalf("messages = %+v, want one assistant message", out.Messages)
	}
	if out.StopReason != llm.StopToolUse {
		t.Errorf("stop reason = %q, want %q", out.StopReason, llm.StopToolUse)
	}
	if len(browser.inputs) != 0 {
		t.Errorf("browser tool ran on the server: %v", browser.inputs)
	}
	if len(sink.starts) != 1 || sink.starts[0].name != "read_file" {
		t.Errorf("tool starts = %+v, want only the server tool", sink.starts)
	}
	want := []string{"read_file", "read_page"}
	if fmt.Sprint(out.ToolsCalled) != fmt.Sprint(want) {
		t.Errorf("tools called = %v, want %v", out.ToolsCalled, want)
	}
}

// TestRunStepLimit covers a turn that runs into MaxSteps.
func TestRunStepLimit(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{{
		toolEvent("call-1", "read_file", `{"path":"a.txt"}`),
		doneEvent(llm.StopToolUse, 1, 1),
	}}}
	tool := &fakeTool{name: "read_file", result: Result{Text: "hello"}}
	sink := &recordSink{}
	a := &Agent{Provider: provider, Tools: []Tool{tool}, MaxSteps: 2}

	out, err := a.Run(context.Background(), userSays("read everything"), nil, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if len(provider.requests) != 2 {
		t.Fatalf("got %d provider calls, want 2", len(provider.requests))
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != llm.RoleAssistant || len(last.Content) != 1 || last.Content[0].Text != stepLimitText {
		t.Errorf("last message = %+v, want the step limit text", last)
	}
	if out.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", out.StopReason, llm.StopEndTurn)
	}
	if !strings.Contains(sink.text.String(), stepLimitText) {
		t.Errorf("sink text = %q, want the step limit text", sink.text.String())
	}
}

// TestRunContextTooLong covers llm.ErrContextTooLong reaching the caller.
func TestRunContextTooLong(t *testing.T) {
	provider := &fakeProvider{err: fmt.Errorf("anthropic: %w", llm.ErrContextTooLong)}
	a := &Agent{Provider: provider, MaxSteps: 5}

	_, err := a.Run(context.Background(), userSays("hi"), nil, &recordSink{})
	if !errors.Is(err, llm.ErrContextTooLong) {
		t.Fatalf("err = %v, want llm.ErrContextTooLong", err)
	}
}

// TestRunCancelledContext covers a cancelled context reaching the caller.
func TestRunCancelledContext(t *testing.T) {
	provider := &fakeProvider{err: context.Canceled}
	a := &Agent{Provider: provider, MaxSteps: 5}

	_, err := a.Run(context.Background(), userSays("hi"), nil, &recordSink{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

// TestRunUnknownTool covers a call to a tool the agent does not have.
func TestRunUnknownTool(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{
		{
			toolEvent("call-1", "delete_everything", `{}`),
			doneEvent(llm.StopToolUse, 1, 1),
		},
		{
			textEvent("I cannot do that"),
			doneEvent(llm.StopEndTurn, 1, 1),
		},
	}}
	sink := &recordSink{}
	a := &Agent{Provider: provider, MaxSteps: 5}

	out, err := a.Run(context.Background(), userSays("delete it all"), nil, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	last := provider.requests[1].Messages
	result := last[len(last)-1].Content[0]
	if !result.IsError || !strings.Contains(result.Content[0].Text, "unknown tool") {
		t.Errorf("result = %+v, want an unknown tool error", result)
	}
	if out.StopReason != llm.StopEndTurn {
		t.Errorf("stop reason = %q, want %q", out.StopReason, llm.StopEndTurn)
	}
}

// TestRunSystemPrompt covers the assembled system prompt and the host context
// that is left out when it is empty or null.
func TestRunSystemPrompt(t *testing.T) {
	cases := []struct {
		name        string
		hostContext json.RawMessage
		wantContext bool
	}{
		{name: "object", hostContext: json.RawMessage(`{"user":"Anna"}`), wantContext: true},
		{name: "empty", hostContext: nil, wantContext: false},
		{name: "null", hostContext: json.RawMessage("null"), wantContext: false},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			provider := &fakeProvider{responses: [][]llm.Event{{
				textEvent("ok"),
				doneEvent(llm.StopEndTurn, 1, 1),
			}}}
			a := &Agent{Provider: provider, SystemPrompt: "Answer in German.", MaxSteps: 5}

			if _, err := a.Run(context.Background(), userSays("hi"), test.hostContext, &recordSink{}); err != nil {
				t.Fatalf("Run: %v", err)
			}

			system := provider.requests[0].System
			if !strings.HasPrefix(system, "Answer in German.") {
				t.Errorf("system prompt does not start with the operator prompt: %q", system)
			}
			if !strings.Contains(system, "You only read.") {
				t.Error("system prompt is missing the built-in part")
			}
			hasContext := strings.Contains(system, hostContextHeader)
			if hasContext != test.wantContext {
				t.Errorf("host context present = %v, want %v", hasContext, test.wantContext)
			}
			if test.wantContext && !strings.Contains(system, `{"user":"Anna"}`) {
				t.Error("system prompt is missing the context JSON")
			}
		})
	}
}

// TestRunCutOffToolCall covers an answer cut off at the token limit while it
// held a tool call. The call is dropped and nothing runs.
func TestRunCutOffToolCall(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{{
		textEvent("let me look"),
		toolEvent("call-1", "read_file", `{"path":"a.txt"}`),
		doneEvent(llm.StopMaxTokens, 1, 1),
	}}}
	tool := &fakeTool{name: "read_file", result: Result{Text: "hello"}}
	a := &Agent{Provider: provider, Tools: []Tool{tool}, MaxSteps: 5}

	out, err := a.Run(context.Background(), userSays("read a.txt"), nil, &recordSink{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(tool.inputs) != 0 {
		t.Errorf("tool ran with %v, want no run", tool.inputs)
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Content) != 1 || out.Messages[0].Content[0].Type != llm.BlockText {
		t.Errorf("messages = %+v, want one assistant message with the text only", out.Messages)
	}
	if out.StopReason != llm.StopMaxTokens {
		t.Errorf("stop reason = %q, want %q", out.StopReason, llm.StopMaxTokens)
	}
}

// TestRunEmptyAnswer covers a model answer without text and without a tool
// call, which becomes a short note instead of an empty message.
func TestRunEmptyAnswer(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{{doneEvent(llm.StopEndTurn, 1, 0)}}}
	sink := &recordSink{}
	a := &Agent{Provider: provider, MaxSteps: 5}

	out, err := a.Run(context.Background(), userSays("hi"), nil, sink)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(out.Messages) != 1 || len(out.Messages[0].Content) != 1 || out.Messages[0].Content[0].Text != noAnswerText {
		t.Errorf("messages = %+v, want one assistant message with the no answer text", out.Messages)
	}
	if sink.text.String() != noAnswerText {
		t.Errorf("sink text = %q, want %q", sink.text.String(), noAnswerText)
	}
}

// TestRunZeroMaxSteps covers an Agent without a step limit, which still makes
// one model call.
func TestRunZeroMaxSteps(t *testing.T) {
	provider := &fakeProvider{responses: [][]llm.Event{{
		textEvent("hello"),
		doneEvent(llm.StopEndTurn, 1, 1),
	}}}
	a := &Agent{Provider: provider}

	out, err := a.Run(context.Background(), userSays("hi"), nil, &recordSink{})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(provider.requests) != 1 {
		t.Fatalf("got %d provider calls, want 1", len(provider.requests))
	}
	if len(out.Messages) != 1 || out.Messages[0].Content[0].Text != "hello" {
		t.Errorf("messages = %+v, want the answer", out.Messages)
	}
}
