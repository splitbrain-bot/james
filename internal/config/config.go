// Package config loads the YAML configuration file.
package config

import "time"

// Config is the whole configuration file.
type Config struct {
	// Server configures the HTTP listener.
	Server Server `yaml:"server"`
	// Auth configures the token check.
	Auth Auth `yaml:"auth"`
	// LLM configures the model provider.
	LLM LLM `yaml:"llm"`
	// Agent configures the conversation loop.
	Agent Agent `yaml:"agent"`
	// Tools configures which tools exist and what they may reach.
	Tools Tools `yaml:"tools"`
	// Log configures the log output.
	Log Log `yaml:"log"`
}

// Server configures the HTTP listener.
type Server struct {
	// Listen is the address to bind. Defaults to 127.0.0.1:8080.
	Listen string `yaml:"listen"`
	// BasePath is the URL prefix all routes live under. Defaults to /.
	BasePath string `yaml:"base_path"`
	// AllowedOrigins lists the origins of host pages that may embed the
	// widget and call the API.
	AllowedOrigins []string `yaml:"allowed_origins"`
	// MaxBodyBytes caps the size of a chat request body. Defaults to 20 MiB.
	MaxBodyBytes int64 `yaml:"max_body_bytes"`
	// MaxImageBytes caps the size of one attached image. Defaults to 5 MiB.
	// The popup rejects larger images before sending.
	MaxImageBytes int64 `yaml:"max_image_bytes"`
}

// Auth configures the token check.
type Auth struct {
	// Secret is the HS256 signing key shared with the host application.
	Secret string `yaml:"secret"`
}

// LLM configures the model provider.
type LLM struct {
	// Provider is anthropic or openai.
	Provider string `yaml:"provider"`
	// BaseURL is the API root, for example https://api.anthropic.com.
	BaseURL string `yaml:"base_url"`
	// Model is the model name.
	Model string `yaml:"model"`
	// APIKey authenticates the requests to the provider.
	APIKey string `yaml:"api_key"`
	// MaxTokens caps the length of one model answer. Defaults to 8192.
	MaxTokens int `yaml:"max_tokens"`
}

// Agent configures the conversation loop.
type Agent struct {
	// SystemPrompt is the operator's prompt. Usually written as @prompt.md.
	SystemPrompt string `yaml:"system_prompt"`
	// MaxToolSteps caps the number of model calls per turn. Defaults to 20.
	MaxToolSteps int `yaml:"max_tool_steps"`
}

// Tools configures which tools exist and what they may reach. A section that
// is absent switches its tools off.
type Tools struct {
	// Files lists the directory roots the file tools may read.
	Files []FileRoot `yaml:"files"`
	// Databases maps a connection name to its settings.
	Databases map[string]Database `yaml:"databases"`
	// Fetch configures the URL fetch tool.
	Fetch *Fetch `yaml:"fetch"`
	// Browser lists the tools that run in the host page, by module name.
	Browser []string `yaml:"browser"`
}

// FileRoot is one directory the file tools may read.
type FileRoot struct {
	// Path is the directory.
	Path string `yaml:"path"`
	// Exclude lists names and glob patterns that are hidden, matched against
	// each path segment, for example .git, vendor or *.env.
	Exclude []string `yaml:"exclude"`
	// MaxFileBytes caps the size of one file read. Defaults to 1 MiB.
	MaxFileBytes int64 `yaml:"max_file_bytes"`
}

// Database is one database connection.
type Database struct {
	// Driver is mysql, postgres or sqlite.
	Driver string `yaml:"driver"`
	// DSN is the connection string for the driver.
	DSN string `yaml:"dsn"`
	// MaxRows caps the rows one query returns. Defaults to 200.
	MaxRows int `yaml:"max_rows"`
	// Timeout caps the run time of one query. Defaults to 10s.
	Timeout time.Duration `yaml:"timeout"`
}

// Fetch configures the URL fetch tool.
type Fetch struct {
	// Allow lists the host names the tool may fetch from. A leading *. allows
	// all subdomains.
	Allow []string `yaml:"allow"`
	// MaxBytes caps the size of one response. Defaults to 2 MiB.
	MaxBytes int64 `yaml:"max_bytes"`
	// Timeout caps one fetch. Defaults to 15s.
	Timeout time.Duration `yaml:"timeout"`
}

// Log configures the log output.
type Log struct {
	// Format is text or json. Defaults to text.
	Format string `yaml:"format"`
}
