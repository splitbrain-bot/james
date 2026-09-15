package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"james/internal/auth"
	"james/internal/llm"
)

// chatRequest is the body the popup posts for one turn.
type chatRequest struct {
	// Token is the JWT the host application issued.
	Token string `json:"token"`
	// Context is the unverified object the host page sent. It reaches the
	// system prompt as text.
	Context json.RawMessage `json:"context"`
	// Messages is the whole history. The last message has to come from the
	// user.
	Messages []llm.Message `json:"messages"`
}

// textEvent is the data of a text event.
type textEvent struct {
	// Text is a piece of the answer.
	Text string `json:"text"`
}

// toolStartEvent announces a server tool that is about to run.
type toolStartEvent struct {
	// ID identifies the tool call.
	ID string `json:"id"`
	// Name is the name of the tool.
	Name string `json:"name"`
	// Input is the JSON object the model passed.
	Input json.RawMessage `json:"input"`
}

// toolEndEvent reports a server tool that finished.
type toolEndEvent struct {
	// ID identifies the tool call.
	ID string `json:"id"`
	// Name is the name of the tool.
	Name string `json:"name"`
	// Output is a short summary of the result.
	Output string `json:"output"`
	// IsError is true when the tool failed.
	IsError bool `json:"is_error"`
}

// doneEvent ends a turn that worked.
type doneEvent struct {
	// StopReason says why the turn ended.
	StopReason llm.StopReason `json:"stop_reason"`
	// Messages are the messages the popup appends to the history.
	Messages []llm.Message `json:"messages"`
	// BrowserTools lists the tool calls the popup has to run.
	BrowserTools []llm.ToolUse `json:"browser_tools"`
	// ToolResults holds the results of the server tools that ran alongside
	// the browser tools.
	ToolResults []llm.Block `json:"tool_results"`
	// Usage counts the tokens of the whole turn.
	Usage llm.Usage `json:"usage"`
}

// errorEvent ends a turn that failed.
type errorEvent struct {
	// Code names the kind of failure for the popup.
	Code string `json:"code"`
	// Message describes the failure for the user.
	Message string `json:"message"`
}

// handleChat runs one turn and streams it as Server-Sent Events. A failure
// after the headers went out becomes an error event.
func (s *server) handleChat(w http.ResponseWriter, r *http.Request) {
	if !s.allowOrigin(w, r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	// The HTTP server closes an unread body once the response starts.
	r.Body = http.MaxBytesReader(w, r.Body, s.cfg.Server.MaxBodyBytes)
	control := http.NewResponseController(w)
	_ = control.SetReadDeadline(time.Now().Add(bodyReadTimeout))
	var req chatRequest
	decodeErr := json.NewDecoder(r.Body).Decode(&req)
	_ = control.SetReadDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	// Ask a proxy not to buffer the stream.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	stream := newStream(w)

	if decodeErr != nil {
		s.log.Warn("chat request rejected", "reason", "body", "error", decodeErr)
		s.end(stream, "error", errorEvent{Code: "bad_request", Message: "the request could not be read"})
		return
	}

	claims, err := auth.Verify(req.Token, s.secret, time.Now())
	if err != nil {
		s.log.Warn("chat request rejected", "reason", "token", "error", err)
		s.end(stream, "error", errorEvent{Code: "unauthorized", Message: "the token is missing or not valid"})
		return
	}

	if len(req.Messages) == 0 || req.Messages[len(req.Messages)-1].Role != llm.RoleUser {
		s.log.Warn("chat request rejected", "reason", "messages", "user", claims.Sub)
		s.end(stream, "error", errorEvent{Code: "bad_request", Message: "the last message has to come from the user"})
		return
	}

	started := time.Now()
	outcome, runErr := s.agent.Run(r.Context(), req.Messages, req.Context, stream)
	s.logTurn(claims.Sub, claims.Name, outcome.ToolsCalled, outcome.Usage, time.Since(started), runErr)

	if runErr != nil {
		code, message := errorCode(runErr)
		s.end(stream, "error", errorEvent{Code: code, Message: message})
		return
	}

	s.end(stream, "done", doneEvent{
		StopReason:   outcome.StopReason,
		Messages:     orEmpty(outcome.Messages),
		BrowserTools: orEmpty(outcome.BrowserTools),
		ToolResults:  orEmpty(outcome.ToolResults),
		Usage:        outcome.Usage,
	})
}

// end sends the event that closes a turn. A failure to send it is logged,
// because the handler can do nothing else about it.
func (s *server) end(stream *stream, event string, data any) {
	if err := stream.send(event, data); err != nil {
		s.log.Error("cannot send the closing event", "event", event, "error", err)
	}
}

// orEmpty returns an empty slice for nil, so the JSON shows [] and not null.
func orEmpty[T any](list []T) []T {
	if list == nil {
		return []T{}
	}
	return list
}

// logTurn writes one line per turn. Message texts never reach the log.
func (s *server) logTurn(sub, name string, tools []string, usage llm.Usage, took time.Duration, err error) {
	attrs := []any{
		"user", sub,
		"name", name,
		"tools", tools,
		"input_tokens", usage.InputTokens,
		"output_tokens", usage.OutputTokens,
		"duration_ms", took.Milliseconds(),
	}
	if err != nil {
		s.log.Error("turn failed", append(attrs, "error", err)...)
		return
	}
	s.log.Info("turn", attrs...)
}

// errorCode maps a failed turn to the code and the message the popup shows.
func errorCode(err error) (string, string) {
	switch {
	case errors.Is(err, llm.ErrContextTooLong):
		return "context_too_long", "this conversation is too long for the model"
	case errors.Is(err, context.Canceled):
		return "cancelled", "the turn was stopped"
	}
	var apiErr *llm.APIError
	if errors.As(err, &apiErr) {
		return "provider", fmt.Sprintf("the model provider refused the request with status %d", apiErr.Status)
	}
	return "internal", "the turn could not be finished"
}
