package agent

import (
	"context"
	"encoding/json"
)

// Tool is one capability the model may call.
type Tool interface {
	// Name is the identifier the model uses to call the tool.
	Name() string
	// Description tells the model what the tool does and how to use it.
	Description() string
	// Schema is the JSON Schema of the tool's input object.
	Schema() json.RawMessage
	// Run executes the tool. A returned error is reported to the model as a
	// failed tool result and does not end the turn.
	Run(ctx context.Context, input json.RawMessage) (Result, error)
}

// BrowserTool marks a tool the popup runs in the host page. Its Run method is
// never called on the server.
type BrowserTool interface {
	Tool
	// RunsInBrowser marks the tool as one the popup runs.
	RunsInBrowser()
}

// Result is what a server tool returns to the model.
type Result struct {
	// Text is the result as text.
	Text string
	// Images holds pictures the tool read, for example from an image file.
	Images []Image
}

// Image is one picture returned by a tool.
type Image struct {
	// MediaType is the MIME type, for example image/png.
	MediaType string
	// Data is the base64 encoded image.
	Data string
}
