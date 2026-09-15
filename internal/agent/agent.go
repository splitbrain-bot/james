// Package agent runs one conversation turn against the configured model and tools.
package agent

import (
	"encoding/json"

	"james/internal/llm"
)

// Agent holds the configuration a turn runs with. One instance serves every
// conversation.
type Agent struct {
	// Provider is the model adapter.
	Provider llm.Provider
	// SystemPrompt is the operator's prompt from the configuration. The
	// built-in instructions are appended to it.
	SystemPrompt string
	// Tools are the tools offered to the model, server and browser ones.
	Tools []Tool
	// MaxSteps caps the number of model calls per turn.
	MaxSteps int
	// MaxTokens caps the length of one model answer.
	MaxTokens int
}

// Sink receives the events of a running turn.
type Sink interface {
	// Text delivers a piece of answer text.
	Text(text string) error
	// ToolStart announces that a server tool is about to run.
	ToolStart(id, name string, input json.RawMessage) error
	// ToolEnd reports that a server tool finished. Output is a short summary
	// of the result.
	ToolEnd(id, name, output string, isError bool) error
}

// Outcome is the result of one turn.
type Outcome struct {
	// Messages are the messages produced in this turn, to be appended to the
	// history by the popup.
	Messages []llm.Message
	// BrowserTools lists the tool calls the popup has to run in the host page.
	BrowserTools []llm.ToolUse
	// ToolResults holds the results of the server tools that ran alongside the
	// pending browser tools. The popup puts them, followed by the browser
	// results, into one user message and appends it after Messages.
	ToolResults []llm.Block
	// StopReason says why the turn ended.
	StopReason llm.StopReason
	// Usage sums the tokens of all model calls in the turn.
	Usage llm.Usage
	// ToolsCalled lists the names of all tools called in the turn, in order.
	ToolsCalled []string
}
