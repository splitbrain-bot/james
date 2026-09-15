// Package llm defines the provider-independent message model and the streaming
// interface that the agent loop uses. Adapters for the Anthropic and the OpenAI
// chat APIs translate between this model and their wire formats.
package llm

import (
	"context"
	"encoding/json"
	"errors"
)

// ErrContextTooLong reports that the conversation no longer fits into the
// model's context window. Adapters return it when the API rejects the request
// for that reason.
var ErrContextTooLong = errors.New("conversation is too long for the model")

// Role names the author of a message.
type Role string

const (
	// RoleUser marks a message written by the user or holding tool results.
	RoleUser Role = "user"
	// RoleAssistant marks a message written by the model.
	RoleAssistant Role = "assistant"
)

// BlockType names the kind of content a Block holds.
type BlockType string

const (
	// BlockText is plain text.
	BlockText BlockType = "text"
	// BlockImage is an image, base64 encoded, with its media type.
	BlockImage BlockType = "image"
	// BlockToolUse is a tool call made by the model.
	BlockToolUse BlockType = "tool_use"
	// BlockToolResult is the result of a tool call, sent back to the model.
	BlockToolResult BlockType = "tool_result"
)

// Block is one piece of message content. Which fields are set depends on Type.
// The JSON form is the wire format between the popup and the server. The
// browser stores its history in the same form.
type Block struct {
	// Type says which kind of block this is.
	Type BlockType `json:"type"`

	// Text holds the text of a text block.
	Text string `json:"text,omitempty"`

	// MediaType is the MIME type of an image block, for example image/png.
	MediaType string `json:"media_type,omitempty"`
	// Data is the base64 encoded content of an image block.
	Data string `json:"data,omitempty"`

	// ID identifies a tool_use block so its result can refer to it.
	ID string `json:"id,omitempty"`
	// Name is the tool called by a tool_use block.
	Name string `json:"name,omitempty"`
	// Input is the JSON object passed to the tool by a tool_use block.
	Input json.RawMessage `json:"input,omitempty"`

	// ToolUseID names the tool_use block a tool_result block answers.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content holds the text and image blocks of a tool_result block.
	Content []Block `json:"content,omitempty"`
	// IsError marks a tool_result block that reports a failure.
	IsError bool `json:"is_error,omitempty"`
}

// Message is one entry of the conversation.
type Message struct {
	// Role is who wrote the message.
	Role Role `json:"role"`
	// Content holds the blocks of the message in order.
	Content []Block `json:"content"`
}

// Tool describes a tool the model may call.
type Tool struct {
	// Name is the identifier the model uses to call the tool.
	Name string
	// Description tells the model what the tool does and how to use it.
	Description string
	// InputSchema is the JSON Schema of the tool's input object.
	InputSchema json.RawMessage
}

// ToolUse is one tool call made by the model.
type ToolUse struct {
	// ID identifies the call. The result refers to it.
	ID string `json:"id"`
	// Name is the tool called.
	Name string `json:"name"`
	// Input is the JSON object passed to the tool.
	Input json.RawMessage `json:"input"`
}

// Usage counts the tokens of one model call.
type Usage struct {
	// InputTokens is the number of prompt tokens.
	InputTokens int `json:"input_tokens"`
	// OutputTokens is the number of generated tokens.
	OutputTokens int `json:"output_tokens"`
}

// StopReason says why the model stopped generating.
type StopReason string

const (
	// StopEndTurn means the model finished its answer.
	StopEndTurn StopReason = "end_turn"
	// StopToolUse means the model wants tool results before it continues.
	StopToolUse StopReason = "tool_use"
	// StopMaxTokens means the answer was cut off at the token limit.
	StopMaxTokens StopReason = "max_tokens"
)

// Request is one call to the model.
type Request struct {
	// System is the system prompt.
	System string
	// Messages is the conversation so far.
	Messages []Message
	// Tools lists the tools the model may call.
	Tools []Tool
	// MaxTokens caps the length of the answer.
	MaxTokens int
}

// EventType names the kind of streaming event an adapter emits.
type EventType string

const (
	// EventText carries a piece of answer text.
	EventText EventType = "text"
	// EventToolUse carries one complete tool call.
	EventToolUse EventType = "tool_use"
	// EventDone closes the stream and carries the stop reason and the usage.
	EventDone EventType = "done"
)

// Event is one streaming event from the model.
type Event struct {
	// Type says which kind of event this is.
	Type EventType
	// Text holds the text of an EventText.
	Text string
	// ToolUse holds the call of an EventToolUse.
	ToolUse *ToolUse
	// StopReason is set on EventDone.
	StopReason StopReason
	// Usage is set on EventDone.
	Usage Usage
}

// Provider streams one model call.
type Provider interface {
	// Stream sends req to the model and calls emit for every event, in order,
	// ending with an EventDone. It returns the first error from the API or from
	// emit. It returns ErrContextTooLong when the conversation does not fit.
	Stream(ctx context.Context, req Request, emit func(Event) error) error
}
