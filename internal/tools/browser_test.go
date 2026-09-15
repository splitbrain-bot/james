package tools

import (
	"context"
	"encoding/json"
	"io/fs"
	"strings"
	"testing"

	"james/internal/agent"
	"james/web"
)

// TestNewBrowserTools checks that a configured name gives the tool of that
// module and that the server refuses to run it.
func TestNewBrowserTools(t *testing.T) {
	tools, err := NewBrowserTools([]string{"navigate", "read_page"})
	if err != nil {
		t.Fatalf("cannot build the tools: %v", err)
	}
	want := []string{"navigate", "read_page"}
	if len(tools) != len(want) {
		t.Fatalf("want %v, got %d tools", want, len(tools))
	}
	for i, name := range want {
		tl := tools[i]
		if tl.Name() != name {
			t.Fatalf("want %s, got %s", name, tl.Name())
		}
		if tl.Description() == "" {
			t.Errorf("%s has no description", name)
		}
		if _, ok := tl.(agent.BrowserTool); !ok {
			t.Errorf("%s is not marked as a browser tool", name)
		}
		if _, err := tl.Run(context.Background(), json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s ran on the server", name)
		}
	}
}

// TestNewBrowserToolsEmpty checks that a missing section gives no tools.
func TestNewBrowserToolsEmpty(t *testing.T) {
	tools, err := NewBrowserTools(nil)
	if err != nil || len(tools) != 0 {
		t.Fatalf("want no tools, got %d and %v", len(tools), err)
	}
}

// TestNewBrowserToolsUnknown checks that a name without a module is reported.
func TestNewBrowserToolsUnknown(t *testing.T) {
	if _, err := NewBrowserTools([]string{"read_minds"}); err == nil {
		t.Fatal("an unknown tool was accepted")
	}
}

// TestBrowserToolModules checks that every module that ships with james states
// a usable definition, so a new module is caught before it is configured.
func TestBrowserToolModules(t *testing.T) {
	entries, err := fs.ReadDir(web.Files, browserToolDir)
	if err != nil {
		t.Fatalf("cannot read the tool modules: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("no tool module ships with james")
	}
	for _, entry := range entries {
		name := strings.TrimSuffix(entry.Name(), ".js")
		if name == entry.Name() {
			t.Errorf("%s is no JavaScript module", entry.Name())
			continue
		}
		tools, err := NewBrowserTools([]string{name})
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		var schema map[string]any
		if err := json.Unmarshal(tools[0].Schema(), &schema); err != nil {
			t.Errorf("%s has a broken schema: %v", name, err)
		}
	}
}

// TestParseDefinition checks the definitions a module may be written with.
func TestParseDefinition(t *testing.T) {
	cases := []struct {
		name   string
		source string
		ok     bool
	}{
		{"complete", "/*{\"description\":\"d\",\"schema\":{}}*/\nexport default () => {};", true},
		{"leading blank lines", "\n\n/*{\"description\":\"d\",\"schema\":{}}*/", true},
		{"no definition", "export default () => {};", false},
		{"unclosed", "/*{\"description\":\"d\",\"schema\":{}}", false},
		{"broken JSON", "/*{\"description\":}*/", false},
		{"no description", "/*{\"schema\":{}}*/", false},
		{"no schema", "/*{\"description\":\"d\"}*/", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseDefinition([]byte(c.source))
			if c.ok && err != nil {
				t.Fatalf("want a definition, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("want an error, got a definition")
			}
		})
	}
}
