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

// openai talks to the OpenAI chat completions API.
type openai struct {
	// baseURL is the API root without a trailing slash.
	baseURL string
	// model is the model name.
	model string
	// apiKey is the API key sent as a bearer token.
	apiKey string
	// client sends the requests.
	client *http.Client
}

// openaiRequest is the body of one chat completions call.
type openaiRequest struct {
	// Model is the model name.
	Model string `json:"model"`
	// Messages is the conversation, system prompt first.
	Messages []openaiMessage `json:"messages"`
	// Tools lists the tools the model may call.
	Tools []openaiTool `json:"tools,omitempty"`
	// MaxCompletionTokens caps the length of the answer.
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	// Stream asks for a Server-Sent Events answer.
	Stream bool `json:"stream"`
	// StreamOptions asks for the token counts at the end of the stream.
	StreamOptions openaiStreamOptions `json:"stream_options"`
}

// openaiStreamOptions configures what the stream carries besides the answer.
type openaiStreamOptions struct {
	// IncludeUsage asks for a last chunk with the token counts.
	IncludeUsage bool `json:"include_usage"`
}

// openaiMessage is one message of the conversation.
type openaiMessage struct {
	// Role is system, user, assistant or tool.
	Role string `json:"role"`
	// Content is a string, or a list of parts for text and images.
	Content any `json:"content,omitempty"`
	// ToolCalls holds the calls of an assistant message.
	ToolCalls []openaiToolCall `json:"tool_calls,omitempty"`
	// ToolCallID names the call a tool message answers.
	ToolCallID string `json:"tool_call_id,omitempty"`
}

// openaiPart is one piece of a user message.
type openaiPart struct {
	// Type is text or image_url.
	Type string `json:"type"`
	// Text is the text of a text part.
	Text string `json:"text,omitempty"`
	// ImageURL carries the picture of an image part.
	ImageURL *openaiImageURL `json:"image_url,omitempty"`
}

// openaiImageURL carries a picture as a data URL.
type openaiImageURL struct {
	// URL is a data URL holding the base64 encoded picture.
	URL string `json:"url"`
}

// openaiToolCall is one call the model made.
type openaiToolCall struct {
	// ID identifies the call.
	ID string `json:"id"`
	// Type is always function here.
	Type string `json:"type"`
	// Function names the tool and carries its arguments.
	Function openaiCallFunction `json:"function"`
}

// openaiCallFunction is the tool and the arguments of a call.
type openaiCallFunction struct {
	// Name is the tool called.
	Name string `json:"name"`
	// Arguments is the argument object as a JSON string.
	Arguments string `json:"arguments"`
}

// openaiTool describes one tool to the model.
type openaiTool struct {
	// Type is always function here.
	Type string `json:"type"`
	// Function describes the tool itself.
	Function openaiFunction `json:"function"`
}

// openaiFunction describes one callable tool.
type openaiFunction struct {
	// Name is the identifier the model calls.
	Name string `json:"name"`
	// Description tells the model what the tool does.
	Description string `json:"description,omitempty"`
	// Parameters is the JSON Schema of the tool's input.
	Parameters json.RawMessage `json:"parameters,omitempty"`
}

// openaiChunk is one chunk of the streamed answer. Only the fields this
// adapter needs are read.
type openaiChunk struct {
	// Choices holds the answer. Only the first choice is used.
	Choices []struct {
		// Delta is the piece of the answer this chunk carries.
		Delta struct {
			// Content is a piece of answer text.
			Content string `json:"content"`
			// ToolCalls holds pieces of tool calls, kept apart by index.
			ToolCalls []struct {
				// Index says which call of the answer this piece belongs to.
				Index int `json:"index"`
				// ID identifies the call. Only the first piece carries it.
				ID string `json:"id"`
				// Function carries the tool name and a piece of its arguments.
				Function struct {
					// Name is the tool called.
					Name string `json:"name"`
					// Arguments is a piece of the argument JSON.
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		// FinishReason says why the model stopped. It is empty until the end.
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	// Usage carries the token counts. Only the last chunk of a stream has
	// them.
	Usage *struct {
		// PromptTokens is the number of prompt tokens.
		PromptTokens int `json:"prompt_tokens"`
		// CompletionTokens is the number of generated tokens.
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
	// Error carries a failure the provider reports inside the stream.
	Error *openaiError `json:"error"`
}

// openaiError is the error object of a failed call.
type openaiError struct {
	// Message describes the error.
	Message string `json:"message"`
	// Type names the kind of error.
	Type string `json:"type"`
	// Code names the reason, for example context_length_exceeded.
	Code string `json:"code"`
}

// openaiPendingCall collects one tool call while it arrives in pieces.
type openaiPendingCall struct {
	// ID identifies the call.
	ID string
	// Name is the tool called.
	Name string
	// Arguments collects the argument JSON.
	Arguments strings.Builder
}

// Stream sends req to the chat completions API and reports every event to emit.
func (o *openai) Stream(ctx context.Context, req Request, emit func(Event) error) error {
	header := http.Header{"Authorization": {"Bearer " + o.apiKey}}
	resp, err := postJSON(ctx, o.client, endpoint(o.baseURL, "/v1/chat/completions"), o.buildRequest(req), header)
	if err != nil {
		return fmt.Errorf("openai: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return openaiResponseError(resp)
	}
	return o.readStream(resp.Body, emit)
}

// buildRequest maps a request to the body of a chat completions call.
func (o *openai) buildRequest(req Request) openaiRequest {
	out := openaiRequest{
		Model:               o.model,
		MaxCompletionTokens: req.MaxTokens,
		Stream:              true,
		StreamOptions:       openaiStreamOptions{IncludeUsage: true},
	}
	if out.MaxCompletionTokens <= 0 {
		out.MaxCompletionTokens = defaultMaxTokens
	}
	if req.System != "" {
		out.Messages = append(out.Messages, openaiMessage{Role: "system", Content: req.System})
	}
	for _, msg := range req.Messages {
		if msg.Role == RoleAssistant {
			out.Messages = append(out.Messages, openaiAssistantMessage(msg))
			continue
		}
		out.Messages = append(out.Messages, openaiUserMessages(msg)...)
	}
	for _, tool := range req.Tools {
		params := tool.InputSchema
		if len(params) == 0 {
			params = json.RawMessage(emptySchema)
		}
		out.Tools = append(out.Tools, openaiTool{
			Type: "function",
			Function: openaiFunction{
				Name:        tool.Name,
				Description: tool.Description,
				Parameters:  params,
			},
		})
	}
	return out
}

// openaiAssistantMessage maps an assistant message. Its text becomes the
// content, its tool_use blocks become tool calls.
func openaiAssistantMessage(msg Message) openaiMessage {
	out := openaiMessage{Role: "assistant"}
	var text []string
	for _, b := range msg.Content {
		switch b.Type {
		case BlockText:
			text = append(text, b.Text)
		case BlockToolUse:
			args := string(b.Input)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, openaiToolCall{
				ID:       b.ID,
				Type:     "function",
				Function: openaiCallFunction{Name: b.Name, Arguments: args},
			})
		}
	}
	if len(text) > 0 {
		out.Content = strings.Join(text, "\n")
	}
	return out
}

// openaiUserMessages maps a user message to the messages the API expects. A
// tool result becomes a message with the tool role, which carries text only, so
// an image inside the result follows as an extra user message naming the call
// it belongs to, and the tool message says so. The tool messages come first,
// because the API wants them directly after the assistant message that asked
// for them.
func openaiUserMessages(msg Message) []openaiMessage {
	var toolMsgs, imageMsgs []openaiMessage
	var parts []openaiPart

	for _, b := range msg.Content {
		switch b.Type {
		case BlockText:
			parts = append(parts, openaiPart{Type: "text", Text: b.Text})
		case BlockImage:
			parts = append(parts, openaiImagePart(b))
		case BlockToolResult:
			var text []string
			var images []openaiPart
			if b.IsError {
				text = append(text, "error:")
			}
			for _, inner := range b.Content {
				switch inner.Type {
				case BlockText:
					text = append(text, inner.Text)
				case BlockImage:
					images = append(images, openaiImagePart(inner))
				}
			}
			if len(text) == 0 && len(images) > 0 {
				text = append(text, "The result is an image. It follows in the next message.")
			}
			toolMsgs = append(toolMsgs, openaiMessage{
				Role:       "tool",
				ToolCallID: b.ToolUseID,
				Content:    strings.Join(text, "\n"),
			})
			if len(images) > 0 {
				note := openaiPart{
					Type: "text",
					Text: "The following image is part of the result of tool call " + b.ToolUseID + ".",
				}
				imageMsgs = append(imageMsgs, openaiMessage{
					Role:    "user",
					Content: append([]openaiPart{note}, images...),
				})
			}
		}
	}

	out := append(toolMsgs, imageMsgs...)
	if len(parts) > 0 {
		out = append(out, openaiMessage{Role: "user", Content: parts})
	}
	return out
}

// openaiImagePart turns an image block into a part holding a data URL.
func openaiImagePart(b Block) openaiPart {
	return openaiPart{
		Type:     "image_url",
		ImageURL: &openaiImageURL{URL: "data:" + b.MediaType + ";base64," + b.Data},
	}
}

// readStream parses the streamed answer and reports its events to emit. A
// successful read ends with one EventDone. A stream that ends without a finish
// reason and without the [DONE] marker is an error, so a cut-off answer never
// passes as complete. A tool call whose arguments are not complete JSON,
// because the answer was cut off, is left out.
func (o *openai) readStream(body io.Reader, emit func(Event) error) error {
	var (
		reader     = newSSEReader(body)
		usage      Usage
		finish     string
		order      []int
		calls      = map[int]*openaiPendingCall{}
		streamDone bool
	)

	for !streamDone {
		sse, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("openai: read stream: %w", err)
		}
		if strings.TrimSpace(sse.Data) == "[DONE]" {
			streamDone = true
			continue
		}

		var chunk openaiChunk
		if err := json.Unmarshal([]byte(sse.Data), &chunk); err != nil {
			return fmt.Errorf("openai: parse chunk: %w", err)
		}
		if chunk.Error != nil {
			return openaiStreamError(chunk.Error)
		}
		if chunk.Usage != nil {
			usage.InputTokens = chunk.Usage.PromptTokens
			usage.OutputTokens = chunk.Usage.CompletionTokens
		}
		if len(chunk.Choices) == 0 {
			continue
		}

		choice := chunk.Choices[0]
		if choice.Delta.Content != "" {
			if err := emit(Event{Type: EventText, Text: choice.Delta.Content}); err != nil {
				return err
			}
		}
		for _, delta := range choice.Delta.ToolCalls {
			call, ok := calls[delta.Index]
			if !ok {
				call = &openaiPendingCall{}
				calls[delta.Index] = call
				order = append(order, delta.Index)
			}
			if delta.ID != "" {
				call.ID = delta.ID
			}
			if delta.Function.Name != "" {
				call.Name = delta.Function.Name
			}
			call.Arguments.WriteString(delta.Function.Arguments)
		}
		if choice.FinishReason != "" {
			finish = choice.FinishReason
		}
	}

	if !streamDone && finish == "" {
		return streamError("openai: the stream ended before the answer was complete")
	}

	// a call is complete only once the stream ends
	emitted := 0
	for _, index := range order {
		call := calls[index]
		args := strings.TrimSpace(call.Arguments.String())
		if args == "" {
			args = "{}"
		}
		if !json.Valid([]byte(args)) {
			continue
		}
		use := ToolUse{ID: call.ID, Name: call.Name, Input: json.RawMessage(args)}
		if err := emit(Event{Type: EventToolUse, ToolUse: &use}); err != nil {
			return err
		}
		emitted++
	}

	stop := openaiStopReason(finish)
	if emitted > 0 && stop == StopEndTurn {
		stop = StopToolUse
	}
	return emit(Event{Type: EventDone, StopReason: stop, Usage: usage})
}

// openaiStopReason maps an API finish reason to a StopReason.
func openaiStopReason(reason string) StopReason {
	switch reason {
	case "tool_calls":
		return StopToolUse
	case "length":
		return StopMaxTokens
	default:
		return StopEndTurn
	}
}

// openaiResponseError turns a failed response into an error. A refused request
// that is too long for the model becomes ErrContextTooLong.
func openaiResponseError(resp *http.Response) error {
	body := errorBody(resp)

	var parsed struct {
		Error openaiError `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)

	if resp.StatusCode == http.StatusBadRequest && openaiIsContextTooLong(&parsed.Error) {
		return ErrContextTooLong
	}
	message := parsed.Error.Message
	if message == "" {
		message = strings.TrimSpace(string(body))
	}
	return &APIError{Status: resp.StatusCode, Message: message}
}

// openaiStreamError turns an error reported inside the stream into an error.
func openaiStreamError(apiErr *openaiError) error {
	if openaiIsContextTooLong(apiErr) {
		return ErrContextTooLong
	}
	return streamError("openai: " + apiErr.Message)
}

// openaiIsContextTooLong reports whether an error says the conversation does
// not fit into the model's context window.
func openaiIsContextTooLong(apiErr *openaiError) bool {
	if apiErr == nil {
		return false
	}
	return apiErr.Code == "context_length_exceeded" ||
		strings.Contains(strings.ToLower(apiErr.Message), "maximum context length")
}
