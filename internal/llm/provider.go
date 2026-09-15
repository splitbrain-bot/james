package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// defaultMaxTokens caps the answer when the request does not say otherwise.
const defaultMaxTokens = 4096

// emptySchema describes a tool input without properties. It stands in for a
// tool that declares no schema.
const emptySchema = `{"type":"object"}`

// maxErrorBodyBytes caps how much of a failed response is read for its message.
const maxErrorBodyBytes = 64 * 1024

// Options selects and configures the provider adapter.
type Options struct {
	// Provider is anthropic or openai.
	Provider string
	// BaseURL is the API root. Empty picks the provider's public API.
	BaseURL string
	// Model is the model name.
	Model string
	// APIKey authenticates the requests.
	APIKey string
}

// APIError reports a call the provider refused.
type APIError struct {
	// Status is the HTTP status code of the response.
	Status int
	// Message is the error message the provider sent, if any.
	Message string
}

// Error renders the status and the provider's message.
func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("provider returned status %d", e.Status)
	}
	return fmt.Sprintf("provider returned status %d: %s", e.Status, e.Message)
}

// NewProvider builds the adapter for the given options. It accepts the
// providers anthropic and openai.
func NewProvider(opts Options) (Provider, error) {
	if opts.Model == "" {
		return nil, errors.New("llm: no model configured")
	}
	if opts.APIKey == "" {
		return nil, errors.New("llm: no api key configured")
	}

	client := newHTTPClient()
	switch opts.Provider {
	case "anthropic":
		return &anthropic{
			baseURL: baseURLOr(opts.BaseURL, "https://api.anthropic.com"),
			model:   opts.Model,
			apiKey:  opts.APIKey,
			client:  client,
		}, nil
	case "openai":
		return &openai{
			baseURL: baseURLOr(opts.BaseURL, "https://api.openai.com"),
			model:   opts.Model,
			apiKey:  opts.APIKey,
			client:  client,
		}, nil
	default:
		return nil, fmt.Errorf("llm: unknown provider %q", opts.Provider)
	}
}

// newHTTPClient builds the client the adapters use. It has no total timeout,
// because one streamed answer may take many minutes.
func newHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			ForceAttemptHTTP2:     true,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 2 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
			MaxIdleConnsPerHost:   2,
		},
	}
}

// baseURLOr returns the configured base URL without its trailing slashes, or
// def when nothing is configured.
func baseURLOr(configured, def string) string {
	if configured == "" {
		return def
	}
	return strings.TrimRight(configured, "/")
}

// endpoint joins the base URL and the path of an API. A base URL that already
// ends in the version segment of the path does not get it twice, so both
// https://host and https://host/v1 work.
func endpoint(baseURL, path string) string {
	version, rest, found := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if found && strings.HasSuffix(baseURL, "/"+version) {
		return baseURL + "/" + rest
	}
	return baseURL + path
}

// postJSON sends body as JSON to url and returns the response, whatever its
// status. header carries the provider's authentication. The caller closes the
// response body.
func postJSON(ctx context.Context, client *http.Client, url string, body any, header http.Header) (*http.Response, error) {
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	for name, values := range header {
		req.Header[name] = values
	}
	return client.Do(req)
}

// errorBody reads the start of a failed response so its message can be shown.
func errorBody(resp *http.Response) []byte {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	return body
}

// streamError reports a failure the provider sent inside the stream. It is an
// APIError with status 502, because the provider, not this server, failed.
func streamError(message string) error {
	return &APIError{Status: http.StatusBadGateway, Message: message}
}
