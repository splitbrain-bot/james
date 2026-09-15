package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"
	"time"

	readability "codeberg.org/readeck/go-readability/v2"
	"golang.org/x/net/html"

	"james/internal/agent"
	"james/internal/config"
)

// defaultFetchBytes caps one response when the configuration sets no cap.
const defaultFetchBytes = 2 << 20

// defaultFetchTimeout caps one fetch when the configuration sets no timeout.
const defaultFetchTimeout = 15 * time.Second

// maxRedirects caps how many requests one fetch makes while following
// redirects.
const maxRedirects = 5

// fetchTool is the fetch_url tool, which reads URLs from the configured
// allowlist.
type fetchTool struct {
	// allow holds the allowed host names, lower case. An entry starting with
	// *. allows all subdomains of the rest.
	allow []string
	// maxBytes caps the size of one response body.
	maxBytes int64
	// client is the HTTP client with the configured timeout. It checks every
	// redirect target against the allowlist.
	client *http.Client
}

// NewFetchTool returns the fetch_url tool for the configured allowlist.
func NewFetchTool(cfg config.Fetch) agent.Tool {
	t := &fetchTool{maxBytes: cfg.MaxBytes}
	for _, a := range cfg.Allow {
		t.allow = append(t.allow, strings.ToLower(strings.TrimSpace(a)))
	}
	if t.maxBytes <= 0 {
		t.maxBytes = defaultFetchBytes
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultFetchTimeout
	}
	t.client = &http.Client{
		Timeout: timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("more than %d redirects", maxRedirects)
			}
			return t.check(req.URL)
		},
	}
	return t
}

// Name is the identifier the model uses to call the tool.
func (t *fetchTool) Name() string { return "fetch_url" }

// Description tells the model what the tool does and how to use it.
func (t *fetchTool) Description() string {
	return "Fetches one URL and returns its readable text. Only these hosts are allowed: " +
		strings.Join(t.allow, ", ") + ". Set raw to true to get the document as it is, " +
		"for example to look at the HTML itself."
}

// Schema is the JSON Schema of the tool's input object.
func (t *fetchTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{` +
		`"url":{"type":"string","description":"the http or https URL to fetch"},` +
		`"raw":{"type":"boolean","description":"return the document unchanged instead of its text"}},` +
		`"required":["url"]}`)
}

// fetchInput is the input of the fetch_url tool.
type fetchInput struct {
	// URL is the address to fetch.
	URL string `json:"url"`
	// Raw returns the document unchanged.
	Raw bool `json:"raw"`
}

// check returns an error when the URL may not be fetched. Only http and https
// are allowed, and the host has to be on the allowlist.
func (t *fetchTool) check(u *url.URL) error {
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("only http and https URLs can be fetched, not %s", u.Scheme)
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return errors.New("the URL has no host")
	}
	for _, a := range t.allow {
		if strings.HasPrefix(a, "*.") {
			if strings.HasSuffix(host, a[1:]) && len(host) > len(a)-1 {
				return nil
			}
			continue
		}
		if host == a {
			return nil
		}
	}
	return fmt.Errorf("%s is not an allowed host", host)
}

// Run fetches the URL and returns its content as text.
func (t *fetchTool) Run(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	var in fetchInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agent.Result{}, fmt.Errorf("bad input: %w", err)
	}
	u, err := url.Parse(strings.TrimSpace(in.URL))
	if err != nil {
		return agent.Result{}, fmt.Errorf("bad URL: %w", err)
	}
	if err := t.check(u); err != nil {
		return agent.Result{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return agent.Result{}, err
	}
	req.Header.Set("Accept", "text/html, text/plain, application/json;q=0.9, */*;q=0.1")
	resp, err := t.client.Do(req)
	if err != nil {
		return agent.Result{}, fmt.Errorf("cannot fetch %s: %w", u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return agent.Result{}, fmt.Errorf("%s answered %s", u, resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, t.maxBytes+1))
	if err != nil {
		return agent.Result{}, fmt.Errorf("cannot read %s: %w", u, err)
	}
	cut := int64(len(body)) > t.maxBytes
	if cut {
		body = body[:t.maxBytes]
	}

	// A response without a content type is read as plain text.
	ctype := resp.Header.Get("Content-Type")
	if ctype == "" {
		ctype = "text/plain"
	}
	media, _, err := mime.ParseMediaType(ctype)
	if err != nil {
		media = strings.ToLower(strings.TrimSpace(strings.Split(ctype, ";")[0]))
	}
	isHTML := media == "text/html" || media == "application/xhtml+xml"
	if !isHTML && !isTextType(media) {
		return agent.Result{}, fmt.Errorf("%s is %s, which is not text", u, media)
	}

	text := string(body)
	if isHTML && !in.Raw {
		text, err = htmlToText(strings.NewReader(text), resp.Request.URL)
		if err != nil {
			return agent.Result{}, fmt.Errorf("cannot read the HTML of %s: %w", u, err)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\n\n", resp.Request.URL, media)
	b.WriteString(text)
	if cut {
		fmt.Fprintf(&b, "\n\n[the response was cut after %s]", humanSize(t.maxBytes))
	}
	return agent.Result{Text: b.String()}, nil
}

// isTextType reports whether a media type carries text the model can read.
func isTextType(media string) bool {
	if media == "" {
		return false
	}
	if strings.HasPrefix(media, "text/") {
		return true
	}
	if strings.HasSuffix(media, "+json") || strings.HasSuffix(media, "+xml") {
		return true
	}
	switch media {
	case "application/json", "application/xml", "application/javascript",
		"application/x-yaml", "application/yaml":
		return true
	}
	return false
}

// skipTags are the elements whose content is not readable page text.
var skipTags = map[string]bool{
	"script":   true,
	"style":    true,
	"noscript": true,
	"head":     true,
	"nav":      true,
	"footer":   true,
	"template": true,
	"svg":      true,
}

// blockTags are the elements that start a new text block, with the prefix the
// block gets.
var blockTags = map[string]string{
	"h1": "# ", "h2": "# ", "h3": "# ", "h4": "# ", "h5": "# ", "h6": "# ",
	"li": "- ",
	"p":  "", "div": "", "section": "", "article": "", "main": "", "aside": "",
	"header": "", "blockquote": "", "pre": "", "table": "", "tr": "", "td": "",
	"th": "", "dt": "", "dd": "", "ul": "", "ol": "", "dl": "", "form": "",
	"figure": "", "figcaption": "", "address": "", "hr": "", "br": "",
}

// htmlToText extracts the readable text of an HTML document. Readability picks
// the main content of the page and drops the clutter around it, and resolves
// relative link addresses against pageURL. What is left becomes text blocks
// separated by blank lines, where headings start with a hash, list items with a
// dash, and links keep their address when it is absolute. A page readability
// finds no main content in is read whole.
func htmlToText(r io.Reader, pageURL *url.URL) (string, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return "", err
	}
	content := doc
	// Readability reads the document without changing it, so the whole
	// document is still there to fall back on.
	if article, err := readability.FromDocument(doc, pageURL); err == nil && article.Node != nil {
		content = article.Node
	}
	e := &extractor{}
	e.walk(content)
	e.flush("")
	return strings.Join(e.blocks, "\n\n"), nil
}

// extractor collects the text blocks of an HTML document.
type extractor struct {
	// blocks are the finished text blocks.
	blocks []string
	// cur is the text of the block being collected.
	cur strings.Builder
}

// flush ends the current block and keeps it, with the given prefix, when it
// holds any text.
func (e *extractor) flush(prefix string) {
	text := strings.Join(strings.Fields(e.cur.String()), " ")
	e.cur.Reset()
	if text != "" {
		e.blocks = append(e.blocks, prefix+text)
	}
}

// walk collects the text of one node and its children.
func (e *extractor) walk(n *html.Node) {
	switch n.Type {
	case html.TextNode:
		e.cur.WriteString(n.Data)
		return
	case html.ElementNode:
		if skipTags[n.Data] {
			return
		}
		if n.Data == "a" {
			e.walkLink(n)
			return
		}
		if prefix, ok := blockTags[n.Data]; ok {
			e.flush("")
			e.children(n)
			e.flush(prefix)
			return
		}
	}
	e.children(n)
}

// children collects the text of every child of n.
func (e *extractor) children(n *html.Node) {
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		e.walk(c)
	}
}

// walkLink collects the text of a link and appends its address when that
// address is absolute.
func (e *extractor) walkLink(n *html.Node) {
	href := ""
	for _, a := range n.Attr {
		if a.Key == "href" {
			href = strings.TrimSpace(a.Val)
			break
		}
	}
	before := e.cur.Len()
	e.children(n)
	if href == "" || e.cur.Len() == before {
		return
	}
	if u, err := url.Parse(href); err == nil && u.IsAbs() {
		e.cur.WriteString(" (" + href + ")")
	}
}
