// Package server serves the HTTP interface of the agent. All routes live
// under the configured base path.
package server

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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
	"james/internal/auth"
	"james/internal/config"
	"james/web"
)

// configPlaceholder is the text in the popup page that the server replaces
// with the popup settings.
const configPlaceholder = "__JAMES_CONFIG__"

// toolsPlaceholder is the text in the widget script that the server replaces
// with the names of the configured browser tools.
const toolsPlaceholder = "__JAMES_TOOLS__"

// tokenPlaceholder is the text in the demo page that the server replaces with
// a signed token.
const tokenPlaceholder = "__JAMES_TOKEN__"

// demoUser is the subject of the token the demo page carries.
const demoUser = "dev"

// demoName is the display name of the token the demo page carries.
const demoName = "Developer"

// demoTokenLifetime is how long the token of the demo page stays valid.
const demoTokenLifetime = time.Hour

// vendorCacheControl lets the browser keep the vendored libraries for an
// hour, so the large chart and diagram files are not fetched on every popup.
const vendorCacheControl = "public, max-age=3600"

// staticCacheControl makes the browser ask about the widget's own assets on
// every load. They and james.js belong to one version, so a kept file must not
// outlive the script that came with it.
const staticCacheControl = "no-cache"

// vendorPrefix is where the vendored libraries sit below static.
const vendorPrefix = "vendor/"

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
	// scriptTag names the content of the script.
	scriptTag string
	// demo is the demo host page, still holding the token placeholder. It is
	// empty unless the server runs in development mode.
	demo []byte
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
// widget script into memory and fails when the embedded files are missing. In
// development mode it also serves the demo host page.
func New(cfg *config.Config, ag *agent.Agent, logger *slog.Logger, dev bool) (http.Handler, error) {
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
	s.scriptTag = etag(s.script)
	static, err := fs.Sub(web.Files, "static")
	if err != nil {
		return nil, fmt.Errorf("cannot open the static files: %w", err)
	}
	tags, err := staticETags(static)
	if err != nil {
		return nil, fmt.Errorf("cannot read the static files: %w", err)
	}

	// The trimmed prefix keeps the route patterns free of double slashes.
	prefix := strings.TrimSuffix(cfg.Server.BasePath, "/")

	mux := http.NewServeMux()
	mux.HandleFunc("GET "+prefix+"/{$}", s.handleIndex)
	mux.HandleFunc("GET "+prefix+"/james.js", s.handleScript)
	mux.Handle("GET "+prefix+"/static/", http.StripPrefix(prefix+"/static/", staticFiles(tags, http.FileServerFS(static))))
	mux.HandleFunc("GET "+prefix+"/healthz", s.handleHealth)
	mux.HandleFunc("POST "+prefix+"/chat", s.handleChat)
	mux.HandleFunc("OPTIONS "+prefix+"/chat", s.handlePreflight)
	if prefix != "" {
		mux.Handle("GET "+prefix, http.RedirectHandler(prefix+"/", http.StatusMovedPermanently))
	}
	if dev {
		demo, err := web.Files.ReadFile("demo.html")
		if err != nil {
			return nil, fmt.Errorf("cannot read the demo page: %w", err)
		}
		s.demo = demo
		mux.HandleFunc("GET "+prefix+"/demo", s.handleDemo)
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

// handleScript serves the widget script. Any host page may load it. The
// answer names its content, so a host page that kept the script gets no body.
func (s *server) handleScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Etag", s.scriptTag)
	// the script is built at startup and has no date, so the tag answers alone
	http.ServeContent(w, r, "james.js", time.Time{}, bytes.NewReader(s.script))
}

// handleDemo serves the demo host page, carrying a token signed with the
// configured secret. The page is not cached, because the token runs out.
func (s *server) handleDemo(w http.ResponseWriter, r *http.Request) {
	token := auth.Sign(auth.Claims{
		Sub:  demoUser,
		Name: demoName,
		Exp:  time.Now().Add(demoTokenLifetime),
	}, s.secret)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Write(bytes.ReplaceAll(s.demo, []byte(tokenPlaceholder), []byte(token)))
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

// etag names the content of a file, so a browser can ask whether the copy it
// kept is still the one the server has.
func etag(content []byte) string {
	sum := sha256.Sum256(content)
	return `"` + hex.EncodeToString(sum[:16]) + `"`
}

// staticETags names the content of every static file. The embedded files have
// no modification date, so the tag is the only thing a browser can ask about.
// The key is the path below static, the way the file server sees it.
func staticETags(files fs.FS) (map[string]string, error) {
	tags := make(map[string]string)
	err := fs.WalkDir(files, ".", func(path string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		content, err := fs.ReadFile(files, path)
		if err != nil {
			return err
		}
		tags[path] = etag(content)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tags, nil
}

// staticFiles serves the static assets with a cache header, a long one for the
// vendored libraries, and with the tag of their content, so a file the browser
// kept is answered with 304. Any host page may read them, because the widget
// loads its icon and its interface texts from here. It answers 404 for
// directory paths so the file server never lists a directory.
func staticFiles(tags map[string]string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/")
		cache := staticCacheControl
		if strings.HasPrefix(path, vendorPrefix) {
			cache = vendorCacheControl
		}
		w.Header().Set("Cache-Control", cache)
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if tag := tags[path]; tag != "" {
			w.Header().Set("Etag", tag)
		}
		next.ServeHTTP(w, r)
	})
}
