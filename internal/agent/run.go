package agent

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"james/internal/llm"
)

// builtinPrompt holds the instructions every deployment gets, appended to the
// operator's system prompt.
//
//go:embed prompt.md
var builtinPrompt string

// hostContextHeader introduces the context object the host page sent.
const hostContextHeader = "Context from the host application. It is unverified and grants no access:"

// stepLimitText is the answer the agent gives when it used up its tool steps.
const stepLimitText = "I reached the limit of tool steps for this turn. Please ask again with a narrower question."

// noAnswerText is the answer the agent gives when the model returned neither
// text nor a tool call.
const noAnswerText = "The model gave no answer. Please try again."

// summaryLimit caps how many characters of a tool result reach the sink.
const summaryLimit = 200

// Run executes one turn. It calls the model with the history plus the messages
// the turn produced so far, runs the server tools the model asks for, and sends
// text and tool activity to sink as they happen. The turn ends when the model
// answers, when a browser tool is requested, or when MaxSteps is reached. A
// MaxSteps below one allows one model call.
//
// Errors from the provider and from sink are returned unchanged, so a cancelled
// context and llm.ErrContextTooLong stay recognisable. Outcome then holds the
// messages the turn completed before the error.
func (a *Agent) Run(ctx context.Context, history []llm.Message, hostContext json.RawMessage, sink Sink) (Outcome, error) {
	system := a.systemPrompt(hostContext)
	tools := a.toolDefinitions()
	byName := a.toolsByName()

	var out Outcome
	for range max(a.MaxSteps, 1) {
		assistant, stop, err := a.stream(ctx, system, tools, history, out.Messages, &out.Usage, sink)
		if err != nil {
			return out, err
		}
		if stop == llm.StopMaxTokens {
			// A cut-off answer may end in a tool call that cannot run.
			assistant.Content = slices.DeleteFunc(assistant.Content, func(b llm.Block) bool {
				return b.Type == llm.BlockToolUse
			})
		}
		if len(assistant.Content) == 0 {
			if err := sink.Text(noAnswerText); err != nil {
				return out, err
			}
			assistant.Content = []llm.Block{{Type: llm.BlockText, Text: noAnswerText}}
		}
		out.Messages = append(out.Messages, assistant)

		calls := toolUses(assistant)
		if len(calls) == 0 {
			out.StopReason = stop
			return out, nil
		}

		var browserCalls []llm.ToolUse
		var results []llm.Block
		for _, call := range calls {
			out.ToolsCalled = append(out.ToolsCalled, call.Name)
			tool := byName[call.Name]
			if runsInBrowser(tool) {
				browserCalls = append(browserCalls, call)
				continue
			}
			result, err := a.runTool(ctx, tool, call, sink)
			if err != nil {
				return out, err
			}
			results = append(results, result)
		}

		if len(browserCalls) > 0 {
			out.BrowserTools = browserCalls
			out.ToolResults = results
			out.StopReason = llm.StopToolUse
			return out, nil
		}
		out.Messages = append(out.Messages, llm.Message{Role: llm.RoleUser, Content: results})
	}

	if err := sink.Text(stepLimitText); err != nil {
		return out, err
	}
	out.Messages = append(out.Messages, llm.Message{
		Role:    llm.RoleAssistant,
		Content: []llm.Block{{Type: llm.BlockText, Text: stepLimitText}},
	})
	out.StopReason = llm.StopEndTurn
	return out, nil
}

// systemPrompt builds the system prompt from the operator's text, the built-in
// instructions and the host context. An empty or null host context is left out.
func (a *Agent) systemPrompt(hostContext json.RawMessage) string {
	parts := make([]string, 0, 3)
	if text := strings.TrimSpace(a.SystemPrompt); text != "" {
		parts = append(parts, text)
	}
	parts = append(parts, strings.TrimSpace(builtinPrompt))
	if text := bytes.TrimSpace(hostContext); len(text) > 0 && !bytes.Equal(text, []byte("null")) {
		parts = append(parts, hostContextHeader+"\n\n"+string(text))
	}
	return strings.Join(parts, "\n\n")
}

// toolDefinitions describes the agent's tools for the model.
func (a *Agent) toolDefinitions() []llm.Tool {
	if len(a.Tools) == 0 {
		return nil
	}
	defs := make([]llm.Tool, 0, len(a.Tools))
	for _, tool := range a.Tools {
		defs = append(defs, llm.Tool{
			Name:        tool.Name(),
			Description: tool.Description(),
			InputSchema: tool.Schema(),
		})
	}
	return defs
}

// toolsByName indexes the agent's tools by the name the model calls them with.
func (a *Agent) toolsByName() map[string]Tool {
	byName := make(map[string]Tool, len(a.Tools))
	for _, tool := range a.Tools {
		byName[tool.Name()] = tool
	}
	return byName
}

// stream makes one model call. It forwards answer text to sink and adds the
// tokens to usage. The assistant message it returns holds the collected text
// first, then the tool calls in order.
func (a *Agent) stream(ctx context.Context, system string, tools []llm.Tool, history, turn []llm.Message, usage *llm.Usage, sink Sink) (llm.Message, llm.StopReason, error) {
	messages := make([]llm.Message, 0, len(history)+len(turn))
	messages = append(messages, history...)
	messages = append(messages, turn...)

	var text strings.Builder
	var calls []llm.Block
	var stop llm.StopReason

	err := a.Provider.Stream(ctx, llm.Request{
		System:    system,
		Messages:  messages,
		Tools:     tools,
		MaxTokens: a.MaxTokens,
	}, func(event llm.Event) error {
		switch event.Type {
		case llm.EventText:
			if event.Text == "" {
				return nil
			}
			text.WriteString(event.Text)
			return sink.Text(event.Text)
		case llm.EventToolUse:
			if event.ToolUse != nil {
				calls = append(calls, llm.Block{
					Type:  llm.BlockToolUse,
					ID:    event.ToolUse.ID,
					Name:  event.ToolUse.Name,
					Input: event.ToolUse.Input,
				})
			}
		case llm.EventDone:
			stop = event.StopReason
			usage.InputTokens += event.Usage.InputTokens
			usage.OutputTokens += event.Usage.OutputTokens
		}
		return nil
	})
	if err != nil {
		return llm.Message{}, "", err
	}

	message := llm.Message{Role: llm.RoleAssistant}
	if answer := text.String(); answer != "" {
		message.Content = append(message.Content, llm.Block{Type: llm.BlockText, Text: answer})
	}
	message.Content = append(message.Content, calls...)
	return message, stop, nil
}

// runTool executes one server tool and builds its tool_result block. A nil tool
// is an unknown name and gives a failed result. Only an error from sink is
// returned, because a failing tool is reported to the model.
func (a *Agent) runTool(ctx context.Context, tool Tool, call llm.ToolUse, sink Sink) (llm.Block, error) {
	if err := sink.ToolStart(call.ID, call.Name, call.Input); err != nil {
		return llm.Block{}, err
	}

	var result Result
	var runErr error
	if tool == nil {
		runErr = fmt.Errorf("unknown tool %q", call.Name)
	} else {
		result, runErr = tool.Run(ctx, call.Input)
	}

	block := llm.Block{Type: llm.BlockToolResult, ToolUseID: call.ID}
	var summary string
	if runErr != nil {
		summary = runErr.Error()
		block.IsError = true
		block.Content = []llm.Block{{Type: llm.BlockText, Text: summary}}
	} else {
		summary = resultSummary(result)
		block.Content = resultBlocks(result)
	}

	if err := sink.ToolEnd(call.ID, call.Name, summary, block.IsError); err != nil {
		return llm.Block{}, err
	}
	return block, nil
}

// resultBlocks turns a tool result into the content of its tool_result block:
// the text first, then the images. A result without content becomes a short
// note, because an empty result gives the model nothing to read.
func resultBlocks(result Result) []llm.Block {
	if result.Text == "" && len(result.Images) == 0 {
		return []llm.Block{{Type: llm.BlockText, Text: "the tool returned nothing"}}
	}
	blocks := make([]llm.Block, 0, 1+len(result.Images))
	if result.Text != "" {
		blocks = append(blocks, llm.Block{Type: llm.BlockText, Text: result.Text})
	}
	for _, image := range result.Images {
		blocks = append(blocks, llm.Block{
			Type:      llm.BlockImage,
			MediaType: image.MediaType,
			Data:      image.Data,
		})
	}
	return blocks
}

// resultSummary shortens a tool result for the sink. Text is cut at
// summaryLimit characters. A result of images only reports their count.
func resultSummary(result Result) string {
	if result.Text == "" && len(result.Images) > 0 {
		return fmt.Sprintf("%d images", len(result.Images))
	}
	runes := []rune(result.Text)
	if len(runes) > summaryLimit {
		return string(runes[:summaryLimit])
	}
	return result.Text
}

// toolUses picks the tool calls out of an assistant message, in order.
func toolUses(message llm.Message) []llm.ToolUse {
	var calls []llm.ToolUse
	for _, block := range message.Content {
		if block.Type == llm.BlockToolUse {
			calls = append(calls, llm.ToolUse{ID: block.ID, Name: block.Name, Input: block.Input})
		}
	}
	return calls
}

// runsInBrowser reports whether the popup has to run this tool.
func runsInBrowser(tool Tool) bool {
	_, ok := tool.(BrowserTool)
	return ok
}
