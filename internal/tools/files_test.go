package tools

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"james/internal/agent"
	"james/internal/config"
)

// testTree creates a small directory tree and returns its path together with
// the file tools for it, keyed by tool name.
func testTree(t *testing.T, exclude []string, maxFileBytes int64) (string, map[string]agent.Tool) {
	t.Helper()
	dir := t.TempDir()
	write := func(rel, content string) {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "line1\nline2\nline3\nline4\nline5\n")
	write("sub/b.go", "package sub\n\n// TODO think about it\n")
	write("sub/deep/c.go", "package deep\n\n// TODO here too\n")
	write("sub/notes.md", "nothing to see\n")
	write("vendor/lib.go", "package lib\n\n// TODO in vendor\n")
	write("secret.env", "TOKEN=1234\n")
	write(".git/config", "[core]\n")
	write("bin.dat", "abc\x00def")
	write("pic.png", "\x89PNG\r\n\x1a\nfake image data")

	roots := []config.FileRoot{{Path: dir, Exclude: exclude, MaxFileBytes: maxFileBytes}}
	return dir, byName(NewFileTools(roots))
}

// byName keys a list of tools by their name.
func byName(tools []agent.Tool) map[string]agent.Tool {
	m := make(map[string]agent.Tool, len(tools))
	for _, tl := range tools {
		m[tl.Name()] = tl
	}
	return m
}

// run calls a tool with the given JSON input.
func run(t *testing.T, tl agent.Tool, input string) (agent.Result, error) {
	t.Helper()
	if tl == nil {
		t.Fatal("tool is missing")
	}
	return tl.Run(context.Background(), json.RawMessage(input))
}

// TestNewFileTools checks that roots give the four tools and that no root
// gives none.
func TestNewFileTools(t *testing.T) {
	if got := NewFileTools(nil); got != nil {
		t.Fatalf("want no tools without roots, got %d", len(got))
	}
	_, tools := testTree(t, nil, 0)
	for _, name := range []string{"list_files", "read_file", "glob", "grep"} {
		tl, ok := tools[name]
		if !ok {
			t.Fatalf("tool %s is missing", name)
		}
		var schema map[string]any
		if err := json.Unmarshal(tl.Schema(), &schema); err != nil {
			t.Errorf("%s has a broken schema: %v", name, err)
		}
		if tl.Description() == "" {
			t.Errorf("%s has no description", name)
		}
	}
}

// TestPathEscape checks that dot dot paths and symlinks cannot reach outside
// a root.
func TestPathEscape(t *testing.T) {
	dir, tools := testTree(t, nil, 0)
	outside := filepath.Join(filepath.Dir(dir), "outside.txt")
	if err := os.WriteFile(outside, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Dir(dir), filepath.Join(dir, "up")); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name  string
		tool  string
		input string
	}{
		{"dot dot", "read_file", `{"path":"../outside.txt"}`},
		{"dot dot in the middle", "read_file", `{"path":"sub/../../outside.txt"}`},
		{"absolute outside", "read_file", `{"path":"` + outside + `"}`},
		{"symlink to a file outside", "read_file", `{"path":"escape.txt"}`},
		{"symlink behind a missing segment", "read_file", `{"path":"` + filepath.Join(dir, "nope", "..", "escape.txt") + `"}`},
		{"symlink to a directory outside", "list_files", `{"path":"up"}`},
		{"directory symlink behind a missing segment", "list_files", `{"path":"` + filepath.Join(dir, "nope", "..", "up") + `"}`},
		{"glob outside", "glob", `{"pattern":"*","path":"../"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := run(t, tools[c.tool], c.input)
			if err == nil {
				t.Fatalf("want an error, got %q", res.Text)
			}
			if strings.Contains(res.Text, "secret") {
				t.Fatalf("the content leaked: %q", res.Text)
			}
		})
	}

	// grep walks the whole tree, so it must not read through a symlink
	res, err := run(t, tools["grep"], `{"pattern":"secret"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "escape.txt") {
		t.Errorf("grep read the target of a symlink: %q", res.Text)
	}
}

// TestExcludeHidesFilesEverywhere checks that excluded paths stay hidden from
// all four tools.
func TestExcludeHidesFilesEverywhere(t *testing.T) {
	_, tools := testTree(t, []string{".git", "vendor", "*.env"}, 0)

	res, err := run(t, tools["list_files"], `{"path":"."}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, hidden := range []string{".git", "vendor", "secret.env"} {
		if strings.Contains(res.Text, hidden) {
			t.Errorf("list_files shows %s:\n%s", hidden, res.Text)
		}
	}
	if !strings.Contains(res.Text, "a.txt") {
		t.Errorf("list_files misses a.txt:\n%s", res.Text)
	}

	for _, p := range []string{"secret.env", "vendor/lib.go", ".git/config"} {
		if _, err := run(t, tools["read_file"], `{"path":"`+p+`"}`); err == nil {
			t.Errorf("read_file reads the excluded %s", p)
		}
	}

	res, err = run(t, tools["glob"], `{"pattern":"**/*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "vendor") {
		t.Errorf("glob shows vendor:\n%s", res.Text)
	}

	res, err = run(t, tools["grep"], `{"pattern":"TODO"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "vendor") {
		t.Errorf("grep searches vendor:\n%s", res.Text)
	}
	if !strings.Contains(res.Text, "sub/b.go") {
		t.Errorf("grep misses sub/b.go:\n%s", res.Text)
	}
}

// TestReadFileOffsetAndLimit checks the line window of read_file and the note
// about the cut.
func TestReadFileOffsetAndLimit(t *testing.T) {
	_, tools := testTree(t, nil, 0)
	cases := []struct {
		name  string
		input string
		want  string
		cut   bool
	}{
		{"whole file", `{"path":"a.txt"}`, "line1\nline2\nline3\nline4\nline5\n", false},
		{"offset", `{"path":"a.txt","offset":4}`, "line4\nline5\n", false},
		{"limit", `{"path":"a.txt","limit":2}`, "line1\nline2\n", true},
		{"offset and limit", `{"path":"a.txt","offset":2,"limit":2}`, "line2\nline3\n", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := run(t, tools["read_file"], c.input)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(res.Text, c.want) {
				t.Fatalf("want %q, got %q", c.want, res.Text)
			}
			if got := strings.Contains(res.Text, "output cut"); got != c.cut {
				t.Fatalf("cut notice is %v, want %v: %q", got, c.cut, res.Text)
			}
		})
	}
}

// TestReadFileSizeCap checks that a file above the byte cap is cut and offers
// the next offset.
func TestReadFileSizeCap(t *testing.T) {
	_, tools := testTree(t, nil, 12)
	res, err := run(t, tools["read_file"], `{"path":"a.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "line1\nline2\n") {
		t.Fatalf("want the first two lines, got %q", res.Text)
	}
	if !strings.Contains(res.Text, "offset 3") {
		t.Fatalf("want a hint to read on at line 3, got %q", res.Text)
	}
}

// TestReadFileImage checks the picture result of an image file and the size
// cap for it.
func TestReadFileImage(t *testing.T) {
	_, tools := testTree(t, nil, 0)
	res, err := run(t, tools["read_file"], `{"path":"pic.png"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Images) != 1 {
		t.Fatalf("want one image, got %d", len(res.Images))
	}
	if res.Images[0].MediaType != "image/png" {
		t.Errorf("want image/png, got %s", res.Images[0].MediaType)
	}
	data, err := base64.StdEncoding.DecodeString(res.Images[0].Data)
	if err != nil {
		t.Fatalf("the data is not base64: %v", err)
	}
	if !strings.HasSuffix(string(data), "fake image data") {
		t.Errorf("the image data is wrong: %q", data)
	}

	_, tooSmall := testTree(t, nil, 4)
	if _, err := run(t, tooSmall["read_file"], `{"path":"pic.png"}`); err == nil {
		t.Error("want an error for an image above the limit")
	}
}

// TestReadFileBinary checks that a file with a NUL byte is refused.
func TestReadFileBinary(t *testing.T) {
	_, tools := testTree(t, nil, 0)
	_, err := run(t, tools["read_file"], `{"path":"bin.dat"}`)
	if err == nil {
		t.Fatal("want an error for a binary file")
	}
	if !strings.Contains(err.Error(), "binary") {
		t.Errorf("want a clear message, got %v", err)
	}
}

// TestGlob checks the pattern matching, including two stars and a start
// directory.
func TestGlob(t *testing.T) {
	_, tools := testTree(t, nil, 0)
	cases := []struct {
		name    string
		input   string
		want    []string
		notWant []string
	}{
		{
			name:    "two stars find every level",
			input:   `{"pattern":"**/*.go"}`,
			want:    []string{"sub/b.go", "sub/deep/c.go", "vendor/lib.go"},
			notWant: []string{"a.txt"},
		},
		{
			name:    "two stars also match the top level",
			input:   `{"pattern":"**/*.txt"}`,
			want:    []string{"a.txt"},
			notWant: []string{"b.go"},
		},
		{
			name:    "one level only",
			input:   `{"pattern":"sub/*.go"}`,
			want:    []string{"sub/b.go"},
			notWant: []string{"sub/deep/c.go"},
		},
		{
			name:    "start directory",
			input:   `{"pattern":"**/*.go","path":"sub/deep"}`,
			want:    []string{"sub/deep/c.go"},
			notWant: []string{"sub/b.go"},
		},
		{
			name:    "plain pattern inside a start directory",
			input:   `{"pattern":"*.go","path":"sub"}`,
			want:    []string{"sub/b.go"},
			notWant: []string{"sub/deep/c.go", "vendor/lib.go"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res, err := run(t, tools["glob"], c.input)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range c.want {
				if !strings.Contains(res.Text, w) {
					t.Errorf("want %s in:\n%s", w, res.Text)
				}
			}
			for _, w := range c.notWant {
				if strings.Contains(res.Text, w) {
					t.Errorf("do not want %s in:\n%s", w, res.Text)
				}
			}
		})
	}
}

// TestGrep checks the glob filter, the case option, an empty result and a
// broken pattern.
func TestGrep(t *testing.T) {
	_, tools := testTree(t, nil, 0)

	res, err := run(t, tools["grep"], `{"pattern":"TODO","glob":"*.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "sub/b.go:3: // TODO think about it") {
		t.Errorf("want the hit with its line number:\n%s", res.Text)
	}
	if strings.Contains(res.Text, "notes.md") {
		t.Errorf("the glob did not limit the search:\n%s", res.Text)
	}

	res, err = run(t, tools["grep"], `{"pattern":"todo","ignore_case":true,"glob":"b.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "sub/b.go") {
		t.Errorf("ignore_case found nothing:\n%s", res.Text)
	}

	res, err = run(t, tools["grep"], `{"pattern":"nothing here"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "no line matches") {
		t.Errorf("want a clear answer without hits, got:\n%s", res.Text)
	}

	if _, err := run(t, tools["grep"], `{"pattern":"("}`); err == nil {
		t.Error("want an error for a broken pattern")
	}
}

// TestGrepCap checks that grep stops at the match cap and says that the
// output was cut.
func TestGrepCap(t *testing.T) {
	dir, tools := testTree(t, nil, 0)
	var b strings.Builder
	for i := 0; i < maxGrepMatches+50; i++ {
		b.WriteString("needle\n")
	}
	if err := os.WriteFile(filepath.Join(dir, "many.txt"), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := run(t, tools["grep"], `{"pattern":"needle"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "the output was cut") {
		t.Fatalf("want a note that the output was cut:\n%s", res.Text)
	}
	hits := strings.Count(res.Text, "many.txt:")
	if hits != maxGrepMatches {
		t.Fatalf("want %d hits, got %d", maxGrepMatches, hits)
	}
}

// TestGrepSkipsBinary checks that grep passes over a file with a NUL byte.
func TestGrepSkipsBinary(t *testing.T) {
	_, tools := testTree(t, nil, 0)
	res, err := run(t, tools["grep"], `{"pattern":"def"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(res.Text, "bin.dat") {
		t.Errorf("grep searched a binary file:\n%s", res.Text)
	}
}

// TestSeveralRoots checks that a relative path is tried against every root.
func TestSeveralRoots(t *testing.T) {
	one := t.TempDir()
	two := t.TempDir()
	if err := os.WriteFile(filepath.Join(one, "only-one.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(two, "only-two.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tools := byName(NewFileTools([]config.FileRoot{{Path: one}, {Path: two}}))

	res, err := run(t, tools["read_file"], `{"path":"only-two.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Text, "two") {
		t.Errorf("want the file from the second root, got %q", res.Text)
	}
	if _, err := run(t, tools["read_file"], `{"path":"only-three.txt"}`); err == nil {
		t.Error("want an error for a file in no root")
	}
}

// TestReadFileLongLine checks that a file made of one long line is cut at the
// byte cap instead of being read whole.
func TestReadFileLongLine(t *testing.T) {
	dir, tools := testTree(t, nil, 1024)
	long := strings.Repeat("x", 100*1024)
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(long), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := run(t, tools["read_file"], `{"path":"long.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Text) > 1024+100 {
		t.Errorf("got %d bytes, want the cap plus the cut note", len(res.Text))
	}
	if !strings.Contains(res.Text, "output cut") {
		t.Errorf("want the cut note in:\n%s", res.Text)
	}
}

// TestGrepAfterLongLine checks that grep reads on after a line longer than its
// line cap.
func TestGrepAfterLongLine(t *testing.T) {
	dir, tools := testTree(t, nil, 4<<20)
	content := strings.Repeat("y", maxGrepLineBytes+10) + "\nneedle here\n"
	if err := os.WriteFile(filepath.Join(dir, "long.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := run(t, tools["grep"], `{"pattern":"needle"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Text, "long.txt:2: needle here") {
		t.Errorf("want the match on line 2 in:\n%s", res.Text)
	}
}
