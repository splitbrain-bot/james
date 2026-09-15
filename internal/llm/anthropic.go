package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// anthropicVersion is the API version header every request has to carry.
const anthropicVersion = "2023-06-01"

// anthropic talks to the Anthropic messages API.
type anthropic struct {
	// baseURL is the API root without a trailing slash.
	baseURL string
	// model is the model name.
	model string
	// apiKey is the API key.
	apiKey string
	// client sends the requests.
	client *http.Client
}

// anthropicRequest is the body of one messages call.
type anthropicRequest struct {
	// Model is the model name.
	Model string `json:"model"`
	// MaxTokens caps the length of the answer.
	MaxTokens int `json:"max_tokens"`
	// System is the system prompt.
	System string `json:"system,omitempty"`
	// Messages is the conversation.
	Messages []anthropicMessage `json:"messages"`
	// Tools lists the tools the model may call.
	Tools []anthropicTool `json:"tools,omitempty"`
	// Stream asks for a Server-Sent Events answer.
	Stream bool `json:"stream"`
}

// anthropicMessage is one message of the conversation.
type anthropicMessage struct {
	// Role is user or assistant.
	Role string `json:"role"`
	// Content holds the blocks of the message.
	Content []anthropicBlock `json:"content"`
}

// anthropicBlock is one content block. Which fields are set depends on Type.
type anthropicBlock struct {
	// Type is text, image, tool_use or tool_result.
	Type string `json:"type"`
	// Text is the text of a text block.
	Text string `json:"text,omitempty"`
	// Source carries the picture of an image block.
	Source *anthropicSource `json:"source,omitempty"`
	// ID identifies a tool_use block.
	ID string `json:"id,omitempty"`
	// Name is the tool of a tool_use block.
	Name string `json:"name,omitempty"`
	// Input is the argument object of a tool_use block.
	Input json.RawMessage `json:"input,omitempty"`
	// ToolUseID names the call a tool_result block answers.
	ToolUseID string `json:"tool_use_id,omitempty"`
	// Content holds the blocks of a tool_result block.
	Content []anthropicBlock `json:"content,omitempty"`
	// IsError marks a failed tool result.
	IsError bool `json:"is_error,omitempty"`
}

// anthropicSource is the picture of an image block.
type anthropicSource struct {
	// Type is always base64 here.
	Type string `json:"type"`
	// MediaType is the MIME type of the picture.
	MediaType string `json:"media_type"`
	// Data is the base64 encoded picture.
	Data string `json:"data"`
}

// anthropicTool describes one tool to the model.
type anthropicTool struct {
	// Name is the identifier the model calls.
	Name string `json:"name"`
	// Description tells the model what the tool does.
	Description string `json:"description,omitempty"`
	// InputSchema is the JSON Schema of the tool's input.
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicStreamEvent is one event of the streamed answer. Only the fields
// this adapter needs are read.
type anthropicStreamEvent struct {
	// Type names the event.
	Type string `json:"type"`
	// Message carries the first usage counts of a message_start event.
	Message *struct {
		// Usage counts the tokens known when the answer starts.
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
	// ContentBlock is the block a content_block_start event opens.
	ContentBlock *struct {
		// Type is text or tool_use.
		Type string `json:"type"`
		// ID identifies a tool_use block.
		ID string `json:"id"`
		// Name is the tool of a tool_use block.
		Name string `json:"name"`
	} `json:"content_block"`
	// Delta carries the change of a content_block_delta or message_delta.
	Delta *struct {
		// Type is text_delta or input_json_delta.
		Type string `json:"type"`
		// Text is a piece of answer text.
		Text string `json:"text"`
		// PartialJSON is a piece of a tool input.
		PartialJSON string `json:"partial_json"`
		// StopReason says why the model stopped.
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	// Usage carries the final counts of a message_delta event.
	Usage *anthropicUsage `json:"usage"`
	// Error carries the message of an error event.
	Error *anthropicError `json:"error"`
}

// anthropicUsage counts the tokens of one call.
type anthropicUsage struct {
	// InputTokens is the number of prompt tokens.
	InputTokens int `json:"input_tokens"`
	// OutputTokens is the number of generated tokens.
	OutputTokens int `json:"output_tokens"`
}

// anthropicError is the error object of a failed call.
type anthropicError struct {
	// Type names the kind of error, for example invalid_request_error.
	Type string `json:"type"`
	// Message describes the error.
	Message string `json:"message"`
}

// Stream sends req to the messages API and reports every event to emit.
func (a *anthropic) Stream(ctx context.Context, req Request, emit func(Event) error) error {
	header := http.Header{
		"X-Api-Key":         {a.apiKey},
		"Anthropic-Version": {anthropicVersion},
	}
	resp, err := postJSON(ctx, a.client, endpoint(a.baseURL, "/v1/messages"), a.buildRequest(req), header)
	if err != nil {
		return fmt.Errorf("anthropic: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return anthropicResponseError(resp)
	}
	return a.readStream(resp.Body, emit)
}

// buildRequest maps a request to the body of a messages call.
func (a *anthropic) buildRequest(req Request) anthropicRequest {
	out := anthropicRequest{
		Model:     a.model,
		MaxTokens: req.MaxTokens,
		System:    req.System,
		Messages:  make([]anthropicMessage, 0, len(req.Messages)),
		Stream:    true,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultMaxTokens
	}
	for _, msg := range req.Messages {
		out.Messages = append(out.Messages, anthropicMessage{
			Role:    string(msg.Role),
			Content: anthropicBlocks(msg.Content),
		})
	}
	for _, tool := range req.Tools {
		schema := tool.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(emptySchema)
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name:        tool.Name,
			Description: tool.Description,
			InputSchema: schema,
		})
	}
	return out
}

// anthropicBlocks maps content blocks to their wire form. Tool results keep
// their nested text and image blocks.
func anthropicBlocks(blocks []Block) []anthropicBlock {
	out := make([]anthropicBlock, 0, len(blocks))
	for _, b := range blocks {
		switch b.Type {
		case BlockText:
			out = append(out, anthropicBlock{Type: "text", Text: b.Text})
		case BlockImage:
			out = append(out, anthropicBlock{Type: "image", Source: &anthropicSource{
				Type:      "base64",
				MediaType: b.MediaType,
				Data:      b.Data,
			}})
		case BlockToolUse:
			input := b.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			out = append(out, anthropicBlock{Type: "tool_use", ID: b.ID, Name: b.Name, Input: input})
		case BlockToolResult:
			out = append(out, anthropicBlock{
				Type:      "tool_result",
				ToolUseID: b.ToolUseID,
				Content:   anthropicBlocks(b.Content),
				IsError:   b.IsError,
			})
		}
	}
	return out
}

// readStream parses the streamed answer and reports its events to emit. A
// successful read ends with one EventDone. A stream that ends before its
// message_stop event is an error, so a cut-off answer never passes as complete.
// The API sends the deltas of one content block together, so one collector for
// the current tool input is enough. A tool call whose input is not complete
// JSON, because the answer was cut off, is left out.
func (a *anthropic) readStream(body io.Reader, emit func(Event) error) error {
	var (
		reader     = newSSEReader(body)
		usage      Usage
		stop       = StopEndTurn
		blockType  string
		toolUse    ToolUse
		toolInput  strings.Builder
		streamDone bool
	)

	for !streamDone {
		sse, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("anthropic: read stream: %w", err)
		}

		var ev anthropicStreamEvent
		if err := json.Unmarshal([]byte(sse.Data), &ev); err != nil {
			return fmt.Errorf("anthropic: parse event: %w", err)
		}
		if ev.Type == "" {
			ev.Type = sse.Name
		}

		switch ev.Type {
		case "message_start":
			if ev.Message != nil {
				usage.InputTokens = ev.Message.Usage.InputTokens
				usage.OutputTokens = ev.Message.Usage.OutputTokens
			}
		case "content_block_start":
			if ev.ContentBlock == nil {
				break
			}
			blockType = ev.ContentBlock.Type
			toolInput.Reset()
			toolUse = ToolUse{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
		case "content_block_delta":
			if ev.Delta == nil {
				break
			}
			switch ev.Delta.Type {
			case "text_delta":
				if ev.Delta.Text == "" {
					break
				}
				if err := emit(Event{Type: EventText, Text: ev.Delta.Text}); err != nil {
					return err
				}
			case "input_json_delta":
				toolInput.WriteString(ev.Delta.PartialJSON)
			}
		case "content_block_stop":
			if blockType == "tool_use" {
				input := strings.TrimSpace(toolInput.String())
				if input == "" {
					input = "{}"
				}
				if json.Valid([]byte(input)) {
					toolUse.Input = json.RawMessage(input)
					call := toolUse
					if err := emit(Event{Type: EventToolUse, ToolUse: &call}); err != nil {
						return err
					}
				}
			}
			blockType = ""
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				stop = anthropicStopReason(ev.Delta.StopReason)
			}
			if ev.Usage != nil {
				if ev.Usage.InputTokens > 0 {
					usage.InputTokens = ev.Usage.InputTokens
				}
				if ev.Usage.OutputTokens > 0 {
					usage.OutputTokens = ev.Usage.OutputTokens
				}
			}
		case "message_stop":
			streamDone = true
		case "error":
			return anthropicStreamError(ev.Error)
		}
	}
	if !streamDone {
		return streamError("anthropic: the stream ended before the answer was complete")
	}

	return emit(Event{Type: EventDone, StopReason: stop, Usage: usage})
}

// anthropicStopReason maps an API stop reason to a StopReason.
func anthropicStopReason(reason string) StopReason {
	switch reason {
	case "tool_use":
		return StopToolUse
	case "max_tokens":
		return StopMaxTokens
	default:
		return StopEndTurn
	}
}

// anthropicResponseError turns a failed response into an error. A refused
// request that is too long for the model becomes ErrContextTooLong.
func anthropicResponseError(resp *http.Response) error {
	body := errorBody(resp)

	var parsed struct {
		Error anthropicError `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)

	message := parsed.Error.Message
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	if resp.StatusCode == http.StatusBadRequest && anthropicIsContextTooLong(message) {
		return ErrContextTooLong
	}
	return &APIError{Status: resp.StatusCode, Message: message}
}

// anthropicStreamError turns an error event inside the stream into an error.
func anthropicStreamError(apiErr *anthropicError) error {
	if apiErr == nil {
		return streamError("anthropic: the stream reported an error")
	}
	if anthropicIsContextTooLong(apiErr.Message) {
		return ErrContextTooLong
	}
	return streamError(fmt.Sprintf("anthropic: %s: %s", apiErr.Type, apiErr.Message))
}

// anthropicIsContextTooLong reports whether an error message says the prompt
// does not fit into the model's context window.
func anthropicIsContextTooLong(message string) bool {
	return strings.Contains(strings.ToLower(message), "prompt is too long")
}
