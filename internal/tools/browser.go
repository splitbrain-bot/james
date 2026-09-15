package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"

	"james/internal/agent"
	"james/web"
)

// browserToolDir holds the browser tool modules inside the embedded web files.
const browserToolDir = "static/tools"

// browserTool is a tool the popup runs in the host page.
type browserTool struct {
	// name is the identifier the model uses, the file name of the module.
	name string
	// def is what the module says about itself.
	def definition
}

// definition is the JSON header a browser tool module starts with.
type definition struct {
	// Description tells the model what the tool does.
	Description string `json:"description"`
	// Schema is the JSON Schema of the tool input.
	Schema json.RawMessage `json:"schema"`
}

// Name is the identifier the model uses to call the tool.
func (t *browserTool) Name() string { return t.name }

// Description tells the model what the tool does and how to use it.
func (t *browserTool) Description() string { return t.def.Description }

// Schema is the JSON Schema of the tool's input object.
func (t *browserTool) Schema() json.RawMessage { return t.def.Schema }

// RunsInBrowser marks the tool as one the popup runs.
func (t *browserTool) RunsInBrowser() {}

// Run always fails, because the popup runs the tool and sends back its result.
func (t *browserTool) Run(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	return agent.Result{}, fmt.Errorf("%s runs in the browser, not on the server", t.name)
}

// NewBrowserTools returns the browser tools the configuration names. Every
// name belongs to one module the widget loads, so read_page stands for
// web/static/tools/read_page.js, and that module states what the model learns
// about the tool.
func NewBrowserTools(names []string) ([]agent.Tool, error) {
	var tools []agent.Tool
	for _, name := range names {
		source, err := web.Files.ReadFile(path.Join(browserToolDir, name+".js"))
		if err != nil {
			return nil, fmt.Errorf("there is no browser tool %s", name)
		}
		def, err := parseDefinition(source)
		if err != nil {
			return nil, fmt.Errorf("browser tool %s: %w", name, err)
		}
		tools = append(tools, &browserTool{name: name, def: def})
	}
	return tools, nil
}

// parseDefinition reads the JSON object the module starts with, which holds
// the description and the input schema.
func parseDefinition(source []byte) (definition, error) {
	var def definition
	source = bytes.TrimSpace(source)
	if !bytes.HasPrefix(source, []byte("/*")) {
		return def, errors.New("the module does not start with its definition")
	}
	end := bytes.Index(source, []byte("*/"))
	if end < 0 {
		return def, errors.New("the definition is not closed")
	}
	if err := json.Unmarshal(source[len("/*"):end], &def); err != nil {
		return def, fmt.Errorf("the definition is no JSON object: %w", err)
	}
	if def.Description == "" {
		return def, errors.New("the definition has no description")
	}
	if len(def.Schema) == 0 {
		return def, errors.New("the definition has no schema")
	}
	return def, nil
}
