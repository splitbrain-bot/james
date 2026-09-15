// Package tools holds the tools the agent may call.
package tools

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/dustin/go-humanize"

	"james/internal/agent"
	"james/internal/config"
)

// defaultMaxFileBytes caps one file read when the configuration sets no cap.
const defaultMaxFileBytes = 1 << 20

// maxEntries caps how many entries list_files and glob report.
const maxEntries = 500

// maxGrepMatches caps how many matching lines grep reports.
const maxGrepMatches = 200

// maxGrepBytes caps the size of the grep output.
const maxGrepBytes = 50 * 1024

// maxGrepLine caps the length of one reported match line.
const maxGrepLine = 200

// maxGrepLineBytes caps how much of one line grep reads. A longer line is
// searched up to this point only.
const maxGrepLineBytes = 1 << 20

// binaryProbe is how many leading bytes the tools check for a NUL byte.
const binaryProbe = 8 * 1024

// imageTypes maps a file extension to the media type read_file reports.
var imageTypes = map[string]string{
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
}

// fileRoot is one directory tree the file tools may read.
type fileRoot struct {
	// path is the resolved absolute directory.
	path string
	// exclude holds the patterns hidden from every file tool. They are
	// matched against each segment of a path relative to the root.
	exclude []string
	// maxFileBytes caps the bytes read_file returns and the size of an image
	// it reads.
	maxFileBytes int64
}

// fileSet is the resolved configuration all four file tools share.
type fileSet struct {
	// roots are the configured directories, in configuration order.
	roots []fileRoot
}

// fileTool is one file tool.
type fileTool struct {
	// name is the identifier the model uses.
	name string
	// description tells the model what the tool does.
	description string
	// schema is the JSON Schema of the tool input.
	schema string
	// run executes the tool.
	run func(ctx context.Context, input json.RawMessage) (agent.Result, error)
}

// Name is the identifier the model uses to call the tool.
func (t *fileTool) Name() string { return t.name }

// Description tells the model what the tool does and how to use it.
func (t *fileTool) Description() string { return t.description }

// Schema is the JSON Schema of the tool's input object.
func (t *fileTool) Schema() json.RawMessage { return json.RawMessage(t.schema) }

// Run executes the tool.
func (t *fileTool) Run(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	return t.run(ctx, input)
}

// NewFileTools returns the file tools for the configured roots: list_files,
// read_file, glob and grep. Without roots it returns no tools. A root that
// cannot be resolved is kept as given, so a call reports a plain not found
// error.
func NewFileTools(roots []config.FileRoot) []agent.Tool {
	if len(roots) == 0 {
		return nil
	}
	s := &fileSet{}
	for _, r := range roots {
		p, err := filepath.Abs(r.Path)
		if err != nil {
			p = r.Path
		}
		if resolved, err := filepath.EvalSymlinks(p); err == nil {
			p = resolved
		}
		limit := r.MaxFileBytes
		if limit <= 0 {
			limit = defaultMaxFileBytes
		}
		s.roots = append(s.roots, fileRoot{path: p, exclude: r.Exclude, maxFileBytes: limit})
	}
	hint := s.hint()

	return []agent.Tool{
		&fileTool{
			name: "list_files",
			description: "Lists the entries of one directory. Directories end with a slash, " +
				"files show their size. " + hint,
			schema: `{"type":"object","properties":{"path":{"type":"string",` +
				`"description":"the directory to list, defaults to the root itself"}}}`,
			run: s.listFiles,
		},
		&fileTool{
			name: "read_file",
			description: "Reads one file. Text files come back as text, images (png, jpg, gif, webp) " +
				"as a picture. Large files are cut; read on with offset and limit. " + hint,
			schema: `{"type":"object","properties":{` +
				`"path":{"type":"string","description":"the file to read"},` +
				`"offset":{"type":"integer","description":"first line to read, counted from 1"},` +
				`"limit":{"type":"integer","description":"how many lines to read"}},` +
				`"required":["path"]}`,
			run: s.readFile,
		},
		&fileTool{
			name: "glob",
			description: "Finds files by name pattern. The pattern is matched against the path " +
				"relative to the searched directory, for example src/*.go or **/*.php. Two stars " +
				"stand for any number of directories. " + hint,
			schema: `{"type":"object","properties":{` +
				`"pattern":{"type":"string","description":"the name pattern, for example **/*.php"},` +
				`"path":{"type":"string","description":"directory to search in, defaults to the root itself"}},` +
				`"required":["pattern"]}`,
			run: s.glob,
		},
		&fileTool{
			name: "grep",
			description: "Finds lines by content. The pattern is a regular expression in RE2 syntax. " +
				"Each hit is reported as path:line: text. " + hint,
			schema: `{"type":"object","properties":{` +
				`"pattern":{"type":"string","description":"the regular expression to search for"},` +
				`"path":{"type":"string","description":"directory to search in, defaults to the root itself"},` +
				`"glob":{"type":"string","description":"only search files matching this name pattern, ` +
				`for example *.go, or a pattern with a slash to match the path relative to the searched directory"},` +
				`"ignore_case":{"type":"boolean","description":"search without regard to case"}},` +
				`"required":["pattern"]}`,
			run: s.grep,
		},
	}
}

// hint returns the text that tells the model how paths are resolved.
func (s *fileSet) hint() string {
	if len(s.roots) == 1 {
		return "Paths are absolute inside " + s.roots[0].path + ", or relative to it."
	}
	names := make([]string, 0, len(s.roots))
	for _, r := range s.roots {
		names = append(names, r.path)
	}
	return "Paths are absolute inside one of the roots, or relative. A relative path is tried " +
		"against each root in this order: " + strings.Join(names, ", ") + "."
}

// excluded reports whether any segment of the root-relative path rel matches
// one of the root's exclude patterns.
func (r *fileRoot) excluded(rel string) bool {
	if rel == "" || rel == "." {
		return false
	}
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		for _, pat := range r.exclude {
			if ok, err := path.Match(pat, seg); err == nil && ok {
				return true
			}
		}
	}
	return false
}

// evalDeepest resolves the symlinks of the longest existing prefix of p and
// appends the part that does not exist yet unchanged.
func evalDeepest(p string) (string, error) {
	rest := ""
	cur := p
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, rest), nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", err
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// within returns the path of abs relative to dir and reports whether abs is
// dir itself or lies below it.
func within(dir, abs string) (string, bool) {
	rel, err := filepath.Rel(dir, abs)
	if err != nil {
		return "", false
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return rel, true
}

// resolve turns a path from the model into a root, the absolute path below it
// and the path relative to that root. An absolute path has to lie inside a
// root, so no path escapes the configured trees. A relative path is tried
// against each root in order and the first existing hit wins. When nothing
// exists, the first root the path resolves in is used, so the error names a
// real path. Excluded paths are reported as not found.
func (s *fileSet) resolve(p string) (*fileRoot, string, string, error) {
	if p == "" {
		p = "."
	}
	if filepath.IsAbs(p) {
		// Clean first, so a missing segment before .. cannot hide a symlink
		// from the check below.
		abs, err := evalDeepest(filepath.Clean(p))
		if err != nil {
			return nil, "", "", fmt.Errorf("cannot resolve %s: %w", p, err)
		}
		for i := range s.roots {
			r := &s.roots[i]
			rel, ok := within(r.path, abs)
			if !ok {
				continue
			}
			if r.excluded(rel) {
				return nil, "", "", fmt.Errorf("no such file or directory: %s", p)
			}
			return r, abs, rel, nil
		}
		return nil, "", "", fmt.Errorf("%s is outside the configured roots", p)
	}

	var first *fileRoot
	var firstAbs, firstRel string
	for i := range s.roots {
		r := &s.roots[i]
		abs, err := evalDeepest(filepath.Join(r.path, p))
		if err != nil {
			continue
		}
		rel, ok := within(r.path, abs)
		if !ok {
			continue
		}
		if r.excluded(rel) {
			continue
		}
		if first == nil {
			first, firstAbs, firstRel = r, abs, rel
		}
		if _, err := os.Lstat(abs); err == nil {
			return r, abs, rel, nil
		}
	}
	if first != nil {
		return first, firstAbs, firstRel, nil
	}
	return nil, "", "", fmt.Errorf("no such file or directory: %s", p)
}

// humanSize formats a byte count for the model, in binary units such as
// 12 B, 4.1 KiB or 1.5 MiB. A count below zero, which no file and no limit
// has, reads as zero rather than as a huge number.
func humanSize(n int64) string {
	if n < 0 {
		n = 0
	}
	return humanize.IBytes(uint64(n))
}

// listFilesInput is the input of the list_files tool.
type listFilesInput struct {
	// Path is the directory to list.
	Path string `json:"path"`
}

// listFiles reports the entries of one directory.
func (s *fileSet) listFiles(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	var in listFilesInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agent.Result{}, fmt.Errorf("bad input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return agent.Result{}, err
	}
	root, abs, rel, err := s.resolve(in.Path)
	if err != nil {
		return agent.Result{}, err
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return agent.Result{}, err
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%s:\n", filepath.ToSlash(abs))
	shown := 0
	for _, e := range entries {
		if root.excluded(filepath.Join(rel, e.Name())) {
			continue
		}
		if shown >= maxEntries {
			fmt.Fprintf(&b, "... only the first %d entries are shown\n", maxEntries)
			break
		}
		shown++
		if e.IsDir() {
			fmt.Fprintf(&b, "%s/\n", e.Name())
			continue
		}
		info, err := e.Info()
		if err != nil {
			fmt.Fprintf(&b, "%s\n", e.Name())
			continue
		}
		fmt.Fprintf(&b, "%s (%s)\n", e.Name(), humanSize(info.Size()))
	}
	if shown == 0 {
		b.WriteString("the directory is empty\n")
	}
	return agent.Result{Text: b.String()}, nil
}

// readFileInput is the input of the read_file tool.
type readFileInput struct {
	// Path is the file to read.
	Path string `json:"path"`
	// Offset is the first line to read, counted from 1.
	Offset int `json:"offset"`
	// Limit is how many lines to read.
	Limit int `json:"limit"`
}

// readFile returns the content of one file, as text or as a picture.
func (s *fileSet) readFile(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	var in readFileInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agent.Result{}, fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(in.Path) == "" {
		return agent.Result{}, errors.New("path is required")
	}
	if err := ctx.Err(); err != nil {
		return agent.Result{}, err
	}
	root, abs, rel, err := s.resolve(in.Path)
	if err != nil {
		return agent.Result{}, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return agent.Result{}, err
	}
	relSlash := filepath.ToSlash(rel)
	if info.IsDir() {
		return agent.Result{}, fmt.Errorf("%s is a directory, use list_files", relSlash)
	}

	if media, ok := imageTypes[strings.ToLower(filepath.Ext(abs))]; ok {
		if info.Size() > root.maxFileBytes {
			return agent.Result{}, fmt.Errorf("the image %s is %s, the limit is %s",
				relSlash, humanSize(info.Size()), humanSize(root.maxFileBytes))
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			return agent.Result{}, err
		}
		return agent.Result{
			Text:   fmt.Sprintf("%s (%s, %s)", relSlash, media, humanSize(info.Size())),
			Images: []agent.Image{{MediaType: media, Data: base64.StdEncoding.EncodeToString(data)}},
		}, nil
	}

	f, err := os.Open(abs)
	if err != nil {
		return agent.Result{}, err
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64*1024)
	head, _ := br.Peek(binaryProbe)
	if bytes.IndexByte(head, 0) >= 0 {
		return agent.Result{}, fmt.Errorf("%s is a binary file and cannot be read as text", relSlash)
	}

	text, next, cut, err := readLines(br, in.Offset, in.Limit, int(root.maxFileBytes))
	if err != nil {
		return agent.Result{}, err
	}
	if text == "" && !cut {
		if in.Offset > 1 {
			return agent.Result{Text: fmt.Sprintf("%s has no line %d", relSlash, in.Offset)}, nil
		}
		return agent.Result{Text: fmt.Sprintf("%s is empty", relSlash)}, nil
	}
	if cut {
		text += fmt.Sprintf("\n[output cut, read on with offset %d]\n", next)
	}
	return agent.Result{Text: text}, nil
}

// readLines returns the lines of r from offset on, at most limit lines and at
// most maxBytes bytes. Offset counts from 1, and zero means the first line. A
// limit of zero means no line limit. It also returns the number of the next
// unread line and whether the output stopped before the end of the file. No
// line is held in memory beyond maxBytes, whatever its length in the file.
func readLines(r *bufio.Reader, offset, limit, maxBytes int) (string, int, bool, error) {
	start := max(offset, 1)
	var b strings.Builder
	line := 0
	kept := 0
	cut := false
	for !cut {
		s, err := nextLine(r, maxBytes)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", 0, false, err
		}
		if s != "" {
			line++
			if line >= start {
				switch {
				case limit > 0 && kept >= limit:
					cut = true
				case b.Len()+len(s) > maxBytes:
					if kept == 0 {
						head, _ := capText(s, maxBytes)
						b.WriteString(head)
						kept++
					}
					cut = true
				default:
					b.WriteString(s)
					kept++
				}
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	return b.String(), start + kept, cut, nil
}

// nextLine reads one line of r and returns it with a line break, keeping at
// most maxBytes of it. The rest of a longer line is skipped. At the end of the
// input it returns what it read and io.EOF.
func nextLine(r *bufio.Reader, maxBytes int) (string, error) {
	var b []byte
	for {
		part, isPrefix, err := r.ReadLine()
		if err != nil {
			return string(b), err
		}
		if room := maxBytes - len(b); room > 0 {
			b = append(b, part[:min(len(part), room)]...)
		}
		if !isPrefix {
			return string(append(b, '\n')), nil
		}
	}
}

// matchPath reports whether the slash separated path rel matches the pattern.
// A ** segment stands for any number of path segments, including none. A
// pattern that cannot be parsed matches nothing.
func matchPath(pattern, rel string) bool {
	ok, err := doublestar.Match(pattern, rel)
	return err == nil && ok
}

// globInput is the input of the glob tool.
type globInput struct {
	// Pattern is the name pattern to match.
	Pattern string `json:"pattern"`
	// Path is the directory to search in.
	Path string `json:"path"`
}

// glob lists the files whose root-relative path matches the pattern.
func (s *fileSet) glob(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	var in globInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agent.Result{}, fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return agent.Result{}, errors.New("pattern is required")
	}
	root, abs, _, err := s.resolve(in.Path)
	if err != nil {
		return agent.Result{}, err
	}

	var found []string
	capped := false
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, ok := within(root.path, p)
		if !ok {
			return nil
		}
		if root.excluded(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// a symlink may point outside the root, so never list it
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		if !matchPath(in.Pattern, relativeSlash(abs, p)) {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if len(found) >= maxEntries {
			capped = true
			return fs.SkipAll
		}
		found = append(found, relSlash)
		return nil
	})
	if err != nil {
		return agent.Result{}, err
	}
	if len(found) == 0 {
		return agent.Result{Text: "no file matches " + in.Pattern}, nil
	}
	text := strings.Join(found, "\n")
	if capped {
		text += fmt.Sprintf("\n... only the first %d files are shown", maxEntries)
	}
	return agent.Result{Text: text}, nil
}

// grepInput is the input of the grep tool.
type grepInput struct {
	// Pattern is the regular expression to search for.
	Pattern string `json:"pattern"`
	// Path is the directory to search in.
	Path string `json:"path"`
	// Glob limits the search to files whose name matches this pattern.
	Glob string `json:"glob"`
	// IgnoreCase searches without regard to case.
	IgnoreCase bool `json:"ignore_case"`
}

// grep reports the lines that match a regular expression.
func (s *fileSet) grep(ctx context.Context, input json.RawMessage) (agent.Result, error) {
	var in grepInput
	if err := json.Unmarshal(input, &in); err != nil {
		return agent.Result{}, fmt.Errorf("bad input: %w", err)
	}
	if strings.TrimSpace(in.Pattern) == "" {
		return agent.Result{}, errors.New("pattern is required")
	}
	expr := in.Pattern
	if in.IgnoreCase {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return agent.Result{}, fmt.Errorf("bad pattern: %w", err)
	}
	root, abs, _, err := s.resolve(in.Path)
	if err != nil {
		return agent.Result{}, err
	}

	var b strings.Builder
	matches := 0
	capped := false
	err = filepath.WalkDir(abs, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		rel, ok := within(root.path, p)
		if !ok {
			return nil
		}
		if root.excluded(rel) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		// a symlink may point outside the root, so never read it
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		relSlash := filepath.ToSlash(rel)
		if in.Glob != "" && !matchGlobName(in.Glob, relativeSlash(abs, p)) {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > root.maxFileBytes {
			return nil
		}
		n, stop := grepFile(p, relSlash, re, &b, maxGrepMatches-matches)
		matches += n
		if stop || matches >= maxGrepMatches || b.Len() >= maxGrepBytes {
			capped = true
			return fs.SkipAll
		}
		return nil
	})
	if err != nil {
		return agent.Result{}, err
	}
	if matches == 0 {
		return agent.Result{Text: "no line matches " + in.Pattern}, nil
	}
	text := b.String()
	if capped {
		text += "... the output was cut, please narrow the search\n"
	}
	return agent.Result{Text: text}, nil
}

// relativeSlash returns the path of p relative to dir with slashes.
func relativeSlash(dir, p string) string {
	rel, err := filepath.Rel(dir, p)
	if err != nil {
		return filepath.ToSlash(p)
	}
	return filepath.ToSlash(rel)
}

// matchGlobName reports whether the path rel, relative to the searched
// directory, matches the file pattern. A pattern with a slash is matched
// against the whole path, a pattern without one against the file name alone.
func matchGlobName(pattern, rel string) bool {
	if strings.Contains(pattern, "/") {
		return matchPath(pattern, rel)
	}
	ok, err := path.Match(pattern, path.Base(rel))
	return err == nil && ok
}

// grepFile writes the matching lines of one file to b, at most left of them.
// It returns how many lines matched and whether the output limit was reached.
// A line longer than maxGrepLineBytes is searched up to that point only.
func grepFile(abs, rel string, re *regexp.Regexp, b *strings.Builder, left int) (int, bool) {
	if left <= 0 {
		return 0, true
	}
	f, err := os.Open(abs)
	if err != nil {
		return 0, false
	}
	defer f.Close()

	br := bufio.NewReaderSize(f, 64*1024)
	head, _ := br.Peek(binaryProbe)
	if bytes.IndexByte(head, 0) >= 0 {
		return 0, false
	}

	found := 0
	line := 0
	for {
		s, err := nextLine(br, maxGrepLineBytes)
		if s == "" && err != nil {
			return found, false
		}
		line++
		if re.MatchString(s) {
			text := strings.TrimSpace(s)
			if len(text) > maxGrepLine {
				text, _ = capText(text, maxGrepLine)
				text += "..."
			}
			fmt.Fprintf(b, "%s:%d: %s\n", rel, line, text)
			found++
			if found >= left || b.Len() >= maxGrepBytes {
				return found, true
			}
		}
		if err != nil {
			return found, false
		}
	}
}
