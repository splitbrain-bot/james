package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// defaultListen is the address the server binds when none is configured.
const defaultListen = "127.0.0.1:8080"

// defaultMaxBodyBytes caps the size of a chat request body.
const defaultMaxBodyBytes = 20 << 20

// defaultMaxImageBytes caps the size of one attached image.
const defaultMaxImageBytes = 5 << 20

// defaultMaxFileBytes caps the size of one file the file tools read.
const defaultMaxFileBytes = 1 << 20

// defaultMaxTokens caps the length of one model answer.
const defaultMaxTokens = 8192

// defaultMaxToolSteps caps the number of model calls per turn.
const defaultMaxToolSteps = 20

// defaultMaxRows caps the rows one database query returns.
const defaultMaxRows = 200

// defaultQueryTimeout caps the run time of one database query.
const defaultQueryTimeout = 10 * time.Second

// defaultFetchBytes caps the size of one fetched response.
const defaultFetchBytes = 2 << 20

// defaultFetchTimeout caps one fetch.
const defaultFetchTimeout = 15 * time.Second

// anthropicBaseURL is the API root used when the provider is anthropic.
const anthropicBaseURL = "https://api.anthropic.com"

// openaiBaseURL is the API root used when the provider is openai.
const openaiBaseURL = "https://api.openai.com"

// envRef matches one ${NAME} reference.
var envRef = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// basePathPattern matches a base path of plain segments, which is safe inside
// a route pattern.
var basePathPattern = regexp.MustCompile(`^(/[A-Za-z0-9._~-]+)+$`)

// browserToolPattern matches a browser tool name, which is also the file name
// of its module and the name the model calls.
var browserToolPattern = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Load reads the configuration file at path, expands its values, fills in the
// defaults and checks that the result can be used.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := expand(&doc, filepath.Dir(path)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}

	cfg := &Config{}
	// An empty file leaves the document without content.
	if doc.Kind != 0 {
		if err := doc.Decode(cfg); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
	}

	cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return cfg, nil
}

// expand expands every scalar value in the document and resolves relative file
// references against dir. Mapping keys are left as they are.
func expand(node *yaml.Node, dir string) error {
	switch node.Kind {
	case yaml.ScalarNode:
		value, err := expandValue(node.Value, dir)
		if err != nil {
			return fmt.Errorf("line %d: %w", node.Line, err)
		}
		if value != node.Value {
			node.Value = value
			// Drop the old type so the new value decides it.
			node.Tag = ""
			node.Style = 0
		}
	case yaml.MappingNode:
		for i := 1; i < len(node.Content); i += 2 {
			if err := expand(node.Content[i], dir); err != nil {
				return err
			}
		}
	default:
		for _, child := range node.Content {
			if err := expand(child, dir); err != nil {
				return err
			}
		}
	}
	return nil
}

// expandValue expands one scalar. A value that starts with @ names a file
// whose content, without its trailing whitespace, becomes the value. Every
// ${NAME} reference in the value or in the file name is replaced by the
// environment variable of that name. Whether the value is a file reference is
// decided before the expansion, so an expanded secret that starts with @ stays
// a plain value. A relative file path is resolved against dir.
func expandValue(value, dir string) (string, error) {
	if !strings.HasPrefix(value, "@") {
		return expandEnv(value)
	}
	name, err := expandEnv(value[1:])
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(name) {
		name = filepath.Join(dir, name)
	}
	content, err := os.ReadFile(name)
	if err != nil {
		return "", err
	}
	return strings.TrimRightFunc(string(content), unicode.IsSpace), nil
}

// expandEnv replaces every ${NAME} reference by the environment variable of
// that name. A reference to an unset variable is an error.
func expandEnv(value string) (string, error) {
	var missing string
	expanded := envRef.ReplaceAllStringFunc(value, func(ref string) string {
		name := ref[2 : len(ref)-1]
		set, ok := os.LookupEnv(name)
		if !ok {
			if missing == "" {
				missing = name
			}
			return ""
		}
		return set
	})
	if missing != "" {
		return "", fmt.Errorf("environment variable %s is not set", missing)
	}
	return expanded, nil
}

// applyDefaults fills in every value the file left out and brings the base path
// into its canonical form.
func (c *Config) applyDefaults() {
	if c.Server.Listen == "" {
		c.Server.Listen = defaultListen
	}
	c.Server.BasePath = normalizeBasePath(c.Server.BasePath)
	if c.Server.MaxBodyBytes <= 0 {
		c.Server.MaxBodyBytes = defaultMaxBodyBytes
	}
	if c.Server.MaxImageBytes <= 0 {
		c.Server.MaxImageBytes = defaultMaxImageBytes
	}

	if c.LLM.BaseURL == "" {
		switch c.LLM.Provider {
		case "anthropic":
			c.LLM.BaseURL = anthropicBaseURL
		case "openai":
			c.LLM.BaseURL = openaiBaseURL
		}
	}
	c.LLM.BaseURL = strings.TrimSuffix(c.LLM.BaseURL, "/")
	if c.LLM.MaxTokens <= 0 {
		c.LLM.MaxTokens = defaultMaxTokens
	}

	if c.Agent.MaxToolSteps <= 0 {
		c.Agent.MaxToolSteps = defaultMaxToolSteps
	}

	for i := range c.Tools.Files {
		if c.Tools.Files[i].MaxFileBytes <= 0 {
			c.Tools.Files[i].MaxFileBytes = defaultMaxFileBytes
		}
	}
	for name, db := range c.Tools.Databases {
		if db.MaxRows <= 0 {
			db.MaxRows = defaultMaxRows
		}
		if db.Timeout <= 0 {
			db.Timeout = defaultQueryTimeout
		}
		c.Tools.Databases[name] = db
	}
	if c.Tools.Fetch != nil {
		if c.Tools.Fetch.MaxBytes <= 0 {
			c.Tools.Fetch.MaxBytes = defaultFetchBytes
		}
		if c.Tools.Fetch.Timeout <= 0 {
			c.Tools.Fetch.Timeout = defaultFetchTimeout
		}
	}

	if c.Log.Format == "" {
		c.Log.Format = "text"
	}
}

// normalizeBasePath makes the base path start with a slash and end without
// one, except for the root path itself.
func normalizeBasePath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	for len(path) > 1 && strings.HasSuffix(path, "/") {
		path = path[:len(path)-1]
	}
	return path
}

// validate reports the first setting that is missing or unusable. Every message
// names the key it is about.
func (c *Config) validate() error {
	if c.Server.BasePath != "/" && !basePathPattern.MatchString(c.Server.BasePath) {
		return errors.New("server.base_path may only hold plain path segments")
	}
	if len(c.Server.AllowedOrigins) == 0 {
		return errors.New("server.allowed_origins needs at least one origin")
	}
	for i, origin := range c.Server.AllowedOrigins {
		if !isOrigin(origin) {
			return fmt.Errorf("server.allowed_origins[%d] must be an origin such as https://app.example.com", i)
		}
	}
	if c.Auth.Secret == "" {
		return errors.New("auth.secret is required")
	}

	switch c.LLM.Provider {
	case "anthropic", "openai":
	case "":
		return errors.New("llm.provider is required")
	default:
		return errors.New("llm.provider must be anthropic or openai")
	}
	if c.LLM.Model == "" {
		return errors.New("llm.model is required")
	}
	if c.LLM.APIKey == "" {
		return errors.New("llm.api_key is required")
	}
	if c.LLM.BaseURL == "" {
		return errors.New("llm.base_url is required")
	}

	for i, root := range c.Tools.Files {
		if root.Path == "" {
			return fmt.Errorf("tools.files[%d].path is required", i)
		}
	}
	for name, db := range c.Tools.Databases {
		switch db.Driver {
		case "mysql", "postgres", "sqlite":
		case "":
			return fmt.Errorf("tools.databases.%s.driver is required", name)
		default:
			return fmt.Errorf("tools.databases.%s.driver must be mysql, postgres or sqlite", name)
		}
		if db.DSN == "" {
			return fmt.Errorf("tools.databases.%s.dsn is required", name)
		}
	}

	if c.Tools.Fetch != nil && len(c.Tools.Fetch.Allow) == 0 {
		return errors.New("tools.fetch.allow needs at least one host")
	}
	for i, name := range c.Tools.Browser {
		if !browserToolPattern.MatchString(name) {
			return fmt.Errorf("tools.browser[%d] must be a tool name of lower case letters, digits and underscores", i)
		}
	}

	switch c.Log.Format {
	case "text", "json":
	default:
		return errors.New("log.format must be text or json")
	}
	return nil
}

// isOrigin reports whether value is a web origin in the form the browser
// sends: a scheme and a host, with nothing after the host.
func isOrigin(value string) bool {
	u, err := url.Parse(value)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != "" &&
		u.Path == "" && u.RawQuery == "" && u.Fragment == "" && u.User == nil && u.Opaque == ""
}
