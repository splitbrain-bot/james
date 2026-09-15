package tools

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"james/internal/config"
)

// testFetch returns a fetch tool for the given hosts.
func testFetch(t *testing.T, allow []string, maxBytes int64) *fetchTool {
	t.Helper()
	tl, ok := NewFetchTool(config.Fetch{Allow: allow, MaxBytes: maxBytes}).(*fetchTool)
	if !ok {
		t.Fatal("NewFetchTool did not return a fetch tool")
	}
	return tl
}

// TestFetchToolShape checks the tool name, the hosts in the description and
// the schema.
func TestFetchToolShape(t *testing.T) {
	tl := testFetch(t, []string{"docs.example.com"}, 0)
	if tl.Name() != "fetch_url" {
		t.Errorf("want fetch_url, got %s", tl.Name())
	}
	if !strings.Contains(tl.Description(), "docs.example.com") {
		t.Errorf("the description hides the allowed hosts: %s", tl.Description())
	}
	var schema map[string]any
	if err := json.Unmarshal(tl.Schema(), &schema); err != nil {
		t.Errorf("broken schema: %v", err)
	}
}

// TestFetchAllowlist checks which hosts and schemes the allowlist accepts.
func TestFetchAllowlist(t *testing.T) {
	tl := testFetch(t, []string{"docs.example.com", "*.wiki.example.org"}, 0)
	cases := []struct {
		url string
		ok  bool
	}{
		{"https://docs.example.com/page", true},
		{"https://docs.example.com:8443/page", true},
		{"http://DOCS.example.com/page", true},
		{"https://docs.example.com.evil.test/page", false},
		{"https://example.com/page", false},
		{"https://de.wiki.example.org/page", true},
		{"https://a.b.wiki.example.org/page", true},
		{"https://wiki.example.org/page", false},
		{"https://evil.test/page", false},
		{"file:///etc/passwd", false},
		{"ftp://docs.example.com/x", false},
		{"docs.example.com/page", false},
	}
	for _, c := range cases {
		t.Run(c.url, func(t *testing.T) {
			u, err := url.Parse(c.url)
			if err != nil {
				t.Fatal(err)
			}
			err = tl.check(u)
			if c.ok && err != nil {
				t.Fatalf("want allowed, got %v", err)
			}
			if !c.ok && err == nil {
				t.Fatal("want rejected, got allowed")
			}
		})
	}
}

// TestFetchRejectsBeforeTheCall checks that a host outside the allowlist is
// refused without a request.
func TestFetchRejectsBeforeTheCall(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	tl := testFetch(t, []string{"docs.example.com"}, 0)
	if _, err := run(t, tl, `{"url":"`+srv.URL+`/"}`); err == nil {
		t.Fatal("want an error for a host that is not allowed")
	}
	if called {
		t.Fatal("the server was called although the host is not allowed")
	}
}

// TestFetchRedirect checks that a redirect to a blocked host fails and an
// allowed one is followed.
func TestFetchRedirect(t *testing.T) {
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, "http://blocked.example.test/secret", http.StatusFound)
		case "/here":
			http.Redirect(w, r, srv.URL+"/end", http.StatusFound)
		default:
			w.Header().Set("Content-Type", "text/plain")
			w.Write([]byte("the end"))
		}
	}))
	defer srv.Close()

	tl := testFetch(t, []string{"127.0.0.1"}, 0)

	if _, err := run(t, tl, `{"url":"`+srv.URL+`/away"}`); err == nil {
		t.Fatal("want an error for a redirect to a host that is not allowed")
	} else if !strings.Contains(err.Error(), "blocked.example.test") {
		t.Errorf("want the blocked host in the message, got %v", err)
	}

	res, err := run(t, tl, `{"url":"`+srv.URL+`/here"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "the end") {
		t.Errorf("an allowed redirect was not followed: %q", res.Text)
	}
}

// TestFetchSizeCap checks that a long response is cut to the configured size,
// with a note.
func TestFetchSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Write([]byte(strings.Repeat("x", 500)))
	}))
	defer srv.Close()

	tl := testFetch(t, []string{"127.0.0.1"}, 20)
	res, err := run(t, tl, `{"url":"`+srv.URL+`/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "was cut") {
		t.Errorf("want a note that the response was cut: %q", res.Text)
	}
	if !strings.Contains(res.Text, "\n\n"+strings.Repeat("x", 20)+"\n\n") {
		t.Errorf("want 20 bytes of body, got %q", res.Text)
	}
}

// TestFetchBinaryIsRefused checks that a response that is not text is refused
// with its media type.
func TestFetchBinaryIsRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Write([]byte("\x89PNG"))
	}))
	defer srv.Close()

	tl := testFetch(t, []string{"127.0.0.1"}, 0)
	_, err := run(t, tl, `{"url":"`+srv.URL+`/"}`)
	if err == nil {
		t.Fatal("want an error for a response that is not text")
	}
	if !strings.Contains(err.Error(), "image/png") {
		t.Errorf("want the content type in the message, got %v", err)
	}
}

// TestFetchWithoutContentType checks that a response without a content type is
// read as plain text.
func TestFetchWithoutContentType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		w.Write([]byte("plain words"))
	}))
	defer srv.Close()

	tl := testFetch(t, []string{"127.0.0.1"}, 0)
	res, err := run(t, tl, `{"url":"`+srv.URL+`/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "plain words") {
		t.Errorf("want the body in:\n%s", res.Text)
	}
}

// fixture is a small HTML page with everything the extraction has to handle.
const fixture = `<!doctype html>
<html><head><title>Ignored</title><style>body{color:red}</style></head>
<body>
<nav>menu one menu two</nav>
<h1>The   Title</h1>
<p>First paragraph with a <a href="https://example.com/x">link</a> and a
<a href="/relative">relative one</a>.</p>
<ul><li>one</li><li>two</li></ul>
<script>alert("hi")</script>
<footer>copyright</footer>
</body></html>`

// TestHTMLToText checks the extraction of headings, lists and links, the
// resolution of a relative link and the skipped elements.
func TestHTMLToText(t *testing.T) {
	page, err := url.Parse("https://docs.example.com/dir/page")
	if err != nil {
		t.Fatal(err)
	}
	text, err := htmlToText(strings.NewReader(fixture), page)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"# The Title",
		"First paragraph with a link (https://example.com/x) and a " +
			"relative one (https://docs.example.com/relative).",
		"- one",
		"- two",
	}
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("want %q in:\n%s", w, text)
		}
	}
	for _, w := range []string{"alert", "color:red", "menu one", "copyright", "Ignored"} {
		if strings.Contains(text, w) {
			t.Errorf("do not want %q in:\n%s", w, text)
		}
	}
	if !strings.Contains(text, "\n\n") {
		t.Errorf("want blocks separated by blank lines:\n%s", text)
	}
}

// TestFetchHTMLAndRaw checks that HTML comes back as text, and unchanged when
// raw is set.
func TestFetchHTMLAndRaw(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte(fixture))
	}))
	defer srv.Close()

	tl := testFetch(t, []string{"127.0.0.1"}, 0)

	res, err := run(t, tl, `{"url":"`+srv.URL+`/"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "# The Title") || strings.Contains(res.Text, "<h1>") {
		t.Errorf("want readable text, got:\n%s", res.Text)
	}

	res, err = run(t, tl, `{"url":"`+srv.URL+`/","raw":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "<h1>The   Title</h1>") {
		t.Errorf("want the document as it is, got:\n%s", res.Text)
	}
}

// TestFetchErrorStatus checks that a status of 400 or more becomes an error.
func TestFetchErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "gone", http.StatusNotFound)
	}))
	defer srv.Close()

	tl := testFetch(t, []string{"127.0.0.1"}, 0)
	if _, err := run(t, tl, `{"url":"`+srv.URL+`/"}`); err == nil {
		t.Fatal("want an error for status 404")
	}
}
