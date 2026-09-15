// Package server serves the HTTP interface of the agent. All routes live
// under the configured base path.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"james/internal/agent"
	"james/internal/config"
	"james/web"
)

// configPlaceholder is the text in the popup page that the server replaces
// with the popup settings.
const configPlaceholder = "__JAMES_CONFIG__"

// toolsPlaceholder is the text in the widget script that the server replaces
// with the names of the configured browser tools.
const toolsPlaceholder = "__JAMES_TOOLS__"

// staticCacheControl lets the browser keep the static assets for an hour, so
// the large chart and diagram libraries are not fetched on every popup.
const staticCacheControl = "public, max-age=3600"

// bodyReadTimeout caps how long a client may take to send its request body.
const bodyReadTimeout = 30 * time.Second

// server holds the state the handlers share.
type server struct {
	// cfg is the loaded configuration.
	cfg *config.Config
	// agent runs one turn.
	agent *agent.Agent
	// log records the turns.
	log *slog.Logger
	// secret is the key the tokens are signed with.
	secret []byte
	// index is the popup page with the settings filled in.
	index []byte
	// script is the widget script the host page embeds, with the browser tool
	// names filled in.
	script []byte
}

// pageConfig is the settings the server writes into the popup page.
type pageConfig struct {
	// AllowedOrigins lists the host page origins the popup accepts messages
	// from.
	AllowedOrigins []string `json:"allowedOrigins"`
	// MaxImageBytes caps the size of one attached image.
	MaxImageBytes int64 `json:"maxImageBytes"`
}

// New builds the HTTP handler for all routes. It reads the popup page and the
// widget script into memory and fails when the embedded files are missing.
func New(cfg *config.Config, ag *agent.Agent, logger *slog.Logger) (http.Handler, error) {
	s := &server{
		cfg:    cfg,
		agent:  ag,
		log:    logger,
		secret: []byte(cfg.Auth.Secret),
	}

	index, err := web.Files.ReadFile("index.html")
	if err != nil {
		return nil, fmt.Errorf("cannot read the popup page: %w", err)
	}
	s.index = bytes.ReplaceAll(index, []byte(configPlaceholder), pageConfigJSON(cfg))
	script, err := web.Files.ReadFile("james.js")
	if err != nil {
		return nil, fmt.Errorf("cannot read the widget script: %w", err)
	}
	s.script = bytes.ReplaceAll(script, []byte(toolsPlaceholder), []byte(strings.Join(cfg.Tools.Browser, ",")))
	static, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, fmt.Errorf("cannot open the static files: %w", err)
	}

	// The trimmed prefix keeps the route patterns free of double slashes.
	prefix := strings.TrimSuffix(cfg.Server.BasePath, "/")

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"/{$}", s.handleIndex)
	mux.HandleFunc("GET "+prefix+"/james.js", s.handleScript)
	mux.Handle("GET "+prefix+"/static/", http.StripPrefix(prefix+"/static/", staticFiles(http.FileServerFS(static))))
	mux.HandleFunc("GET "+prefix+"/healthz", s.handleHealth)
	mux.HandleFunc("POST "+prefix+"/chat", s.handleChat)
	mux.HandleFunc("OPTIONS "+prefix+"/chat", s.handlePreflight)
	if prefix != "" {
		mux.Handle("GET "+prefix, http.RedirectHandler(prefix+"/", http.StatusMovedPermanently))
	}
	return mux, nil
}

// pageConfigJSON renders the popup settings as JSON that is safe inside a
// script element.
func pageConfigJSON(cfg *config.Config) []byte {
	origins := cfg.Server.AllowedOrigins
	if origins == nil {
		origins = []string{}
	}
	page := pageConfig{
		AllowedOrigins: origins,
		MaxImageBytes:  cfg.Server.MaxImageBytes,
	}
	raw, err := json.Marshal(page)
	if err != nil {
		return []byte("null")
	}
	var escaped bytes.Buffer
	json.HTMLEscape(&escaped, raw)
	return escaped.Bytes()
}

// handleIndex serves the popup page. The page is not cached, because it
// carries the current settings.
func (s *server) handleIndex(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// the popup is a window of its own
	w.Header().Set("Content-Security-Policy", "frame-ancestors 'none'")
	w.Write(s.index)
}

// handleScript serves the widget script. Any host page may load it.
func (s *server) handleScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(s.script)
}

// handleHealth answers that the server is up.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok"))
}

// handlePreflight answers the CORS preflight of the chat route.
func (s *server) handlePreflight(w http.ResponseWriter, r *http.Request) {
	if !s.allowOrigin(w, r) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.Header().Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// allowOrigin sets the CORS headers and reports whether the request may go on.
// A request without an Origin header passes, and so does the server's own
// origin, which the browser sends on the popup's own requests.
func (s *server) allowOrigin(w http.ResponseWriter, r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	w.Header().Add("Vary", "Origin")
	if !slices.Contains(s.cfg.Server.AllowedOrigins, origin) && !isOwnOrigin(origin, r) {
		return false
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	return true
}

// isOwnOrigin reports whether origin names the host the request was sent to.
// The scheme stays out of the comparison, because a proxy that ends the TLS
// connection forwards a plain request and only the client could name the
// original scheme.
func isOwnOrigin(origin string, r *http.Request) bool {
	parsed, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return parsed.Host != "" && parsed.Host == r.Host
}

// staticFiles serves the static assets with a cache header. Any host page may
// read them, because the widget loads its icon and its interface texts from
// here. It answers 404 for directory paths so the file server never lists a
// directory.
func staticFiles(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", staticCacheControl)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		next.ServeHTTP(w, r)
	})
}
