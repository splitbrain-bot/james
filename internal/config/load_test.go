package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeConfig writes body as a configuration file into a new directory and
// returns its path.
func writeConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "james.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// minimal is a configuration with every required value and nothing else.
const minimal = `
server:
  allowed_origins:
    - https://app.example.com
auth:
  secret: s3cret
llm:
  provider: anthropic
  model: claude-opus-5
  api_key: key
`

// TestLoadExpandsEnvAndFiles checks the expansion of ${NAME} and @path values.
func TestLoadExpandsEnvAndFiles(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: ${JAMES_LISTEN}
  base_path: /agent/
  allowed_origins:
    - https://app.example.com
auth:
  secret: "@secret.txt"
llm:
  provider: anthropic
  model: claude-opus-5
  api_key: "prefix-${JAMES_KEY}-suffix"
  max_tokens: ${JAMES_TOKENS}
agent:
  system_prompt: "@${JAMES_PROMPT_FILE}"
tools:
  files:
    - path: ${JAMES_ROOT}
      exclude: ["${JAMES_EXCLUDE}"]
`)
	dir := filepath.Dir(path)
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("from-file\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "prompt.md"), []byte("be nice\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JAMES_LISTEN", "0.0.0.0:9000")
	t.Setenv("JAMES_KEY", "abc")
	t.Setenv("JAMES_TOKENS", "1234")
	t.Setenv("JAMES_PROMPT_FILE", "sub/prompt.md")
	t.Setenv("JAMES_ROOT", "/var/www/app")
	t.Setenv("JAMES_EXCLUDE", ".git")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != "0.0.0.0:9000" {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Auth.Secret != "from-file" {
		t.Errorf("secret = %q", cfg.Auth.Secret)
	}
	if cfg.LLM.APIKey != "prefix-abc-suffix" {
		t.Errorf("api_key = %q", cfg.LLM.APIKey)
	}
	if cfg.LLM.MaxTokens != 1234 {
		t.Errorf("max_tokens = %d", cfg.LLM.MaxTokens)
	}
	if cfg.Agent.SystemPrompt != "be nice" {
		t.Errorf("system_prompt = %q", cfg.Agent.SystemPrompt)
	}
	if len(cfg.Tools.Files) != 1 || cfg.Tools.Files[0].Path != "/var/www/app" {
		t.Fatalf("files = %+v", cfg.Tools.Files)
	}
	if got := cfg.Tools.Files[0].Exclude; len(got) != 1 || got[0] != ".git" {
		t.Errorf("exclude = %v", got)
	}
	if cfg.Server.BasePath != "/agent" {
		t.Errorf("base_path = %q", cfg.Server.BasePath)
	}
}

// TestLoadMissingEnv checks that an unset environment variable fails the load.
func TestLoadMissingEnv(t *testing.T) {
	path := writeConfig(t, minimal+"\nagent:\n  system_prompt: ${JAMES_MISSING}\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "JAMES_MISSING") {
		t.Fatalf("err = %v", err)
	}
}

// TestLoadMissingFile checks that a missing @path file fails the load.
func TestLoadMissingFile(t *testing.T) {
	path := writeConfig(t, minimal+"\nagent:\n  system_prompt: \"@nope.md\"\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "nope.md") {
		t.Fatalf("err = %v", err)
	}
}

// TestLoadDefaults checks the defaults a minimal file gets filled in with.
func TestLoadDefaults(t *testing.T) {
	path := writeConfig(t, minimal+`
tools:
  files:
    - path: /var/www/app
  databases:
    main:
      driver: sqlite
      dsn: file:app.db
  fetch:
    allow: ["docs.example.com"]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Listen != defaultListen {
		t.Errorf("listen = %q", cfg.Server.Listen)
	}
	if cfg.Server.BasePath != "/" {
		t.Errorf("base_path = %q", cfg.Server.BasePath)
	}
	if cfg.Server.MaxBodyBytes != defaultMaxBodyBytes || cfg.Server.MaxImageBytes != defaultMaxImageBytes {
		t.Errorf("body/image limits = %d/%d", cfg.Server.MaxBodyBytes, cfg.Server.MaxImageBytes)
	}
	if cfg.LLM.BaseURL != anthropicBaseURL {
		t.Errorf("base_url = %q", cfg.LLM.BaseURL)
	}
	if cfg.LLM.MaxTokens != defaultMaxTokens {
		t.Errorf("max_tokens = %d", cfg.LLM.MaxTokens)
	}
	if cfg.Agent.MaxToolSteps != defaultMaxToolSteps {
		t.Errorf("max_tool_steps = %d", cfg.Agent.MaxToolSteps)
	}
	if cfg.Tools.Files[0].MaxFileBytes != defaultMaxFileBytes {
		t.Errorf("max_file_bytes = %d", cfg.Tools.Files[0].MaxFileBytes)
	}
	db := cfg.Tools.Databases["main"]
	if db.MaxRows != defaultMaxRows || db.Timeout != defaultQueryTimeout {
		t.Errorf("database defaults = %d/%s", db.MaxRows, db.Timeout)
	}
	if cfg.Tools.Fetch.MaxBytes != defaultFetchBytes || cfg.Tools.Fetch.Timeout != defaultFetchTimeout {
		t.Errorf("fetch defaults = %d/%s", cfg.Tools.Fetch.MaxBytes, cfg.Tools.Fetch.Timeout)
	}
	if cfg.Log.Format != "text" {
		t.Errorf("log.format = %q", cfg.Log.Format)
	}
}

// TestLoadOpenAIBaseURL checks the base URL default of the openai provider.
func TestLoadOpenAIBaseURL(t *testing.T) {
	path := writeConfig(t, `
server:
  listen: 127.0.0.1:8080
  allowed_origins:
    - https://app.example.com
auth:
  secret: s3cret
llm:
  provider: openai
  model: gpt-5
  api_key: key
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LLM.BaseURL != openaiBaseURL {
		t.Errorf("base_url = %q", cfg.LLM.BaseURL)
	}
}

// TestLoadReadsDuration checks that a database timeout is read as a duration.
func TestLoadReadsDuration(t *testing.T) {
	path := writeConfig(t, minimal+`
tools:
  databases:
    main:
      driver: mysql
      dsn: user@/db
      timeout: 25s
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Tools.Databases["main"].Timeout; got != 25*time.Second {
		t.Errorf("timeout = %s", got)
	}
}

// TestLoadValidationErrors checks the message for every rejected configuration.
func TestLoadValidationErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"no origins", "server:\n  listen: :8080\nauth:\n  secret: s\nllm:\n  provider: anthropic\n  model: m\n  api_key: k\n", "server.allowed_origins needs at least one origin"},
		{"no secret", "server:\n  listen: :8080\n  allowed_origins: [https://a.example.com]\nllm:\n  provider: anthropic\n  model: m\n  api_key: k\n", "auth.secret is required"},
		{"no provider", "server:\n  listen: :8080\n  allowed_origins: [https://a.example.com]\nauth:\n  secret: s\nllm:\n  model: m\n  api_key: k\n", "llm.provider is required"},
		{"bad provider", "server:\n  listen: :8080\n  allowed_origins: [https://a.example.com]\nauth:\n  secret: s\nllm:\n  provider: gemini\n  model: m\n  api_key: k\n", "llm.provider must be anthropic or openai"},
		{"no model", "server:\n  listen: :8080\n  allowed_origins: [https://a.example.com]\nauth:\n  secret: s\nllm:\n  provider: openai\n  api_key: k\n", "llm.model is required"},
		{"no key", "server:\n  listen: :8080\n  allowed_origins: [https://a.example.com]\nauth:\n  secret: s\nllm:\n  provider: openai\n  model: m\n", "llm.api_key is required"},
		{"bad driver", minimal + "tools:\n  databases:\n    main:\n      driver: oracle\n      dsn: x\n", "tools.databases.main.driver must be mysql, postgres or sqlite"},
		{"no dsn", minimal + "tools:\n  databases:\n    main:\n      driver: sqlite\n", "tools.databases.main.dsn is required"},
		{"no root path", minimal + "tools:\n  files:\n    - exclude: [x]\n", "tools.files[0].path is required"},
		{"bad log format", minimal + "log:\n  format: xml\n", "log.format must be text or json"},
		{"bad base path", "server:\n  listen: :8080\n  base_path: /a{b\n  allowed_origins: [https://a.example.com]\nauth:\n  secret: s\nllm:\n  provider: anthropic\n  model: m\n  api_key: k\n", "server.base_path may only hold plain path segments"},
		{"origin with slash", "server:\n  listen: :8080\n  allowed_origins: [https://a.example.com/]\nauth:\n  secret: s\nllm:\n  provider: anthropic\n  model: m\n  api_key: k\n", "server.allowed_origins[0] must be an origin"},
		{"origin without scheme", "server:\n  listen: :8080\n  allowed_origins: [a.example.com]\nauth:\n  secret: s\nllm:\n  provider: anthropic\n  model: m\n  api_key: k\n", "server.allowed_origins[0] must be an origin"},
		{"fetch without hosts", minimal + "tools:\n  fetch:\n    max_bytes: 1024\n", "tools.fetch.allow needs at least one host"},
		{"bad browser tool", minimal + "tools:\n  browser: [Read Page]\n", "tools.browser[0] must be a tool name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, c.body))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// TestNormalizeBasePath checks the canonical form of a base path.
func TestNormalizeBasePath(t *testing.T) {
	cases := map[string]string{
		"":         "/",
		"/":        "/",
		"agent":    "/agent",
		"/agent/":  "/agent",
		"/agent//": "/agent",
		"/a/b/":    "/a/b",
	}
	for in, want := range cases {
		if got := normalizeBasePath(in); got != want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadKeepsExpandedAtSign checks that a value from the environment that
// starts with @ stays a plain value instead of naming a file.
func TestLoadKeepsExpandedAtSign(t *testing.T) {
	t.Setenv("JAMES_AT_SECRET", "@not-a-file")
	path := writeConfig(t, `
server:
  listen: 127.0.0.1:8080
  allowed_origins:
    - https://app.example.com
auth:
  secret: ${JAMES_AT_SECRET}
llm:
  provider: anthropic
  model: claude-opus-5
  api_key: key
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Auth.Secret != "@not-a-file" {
		t.Errorf("secret = %q, want the literal value", cfg.Auth.Secret)
	}
}
