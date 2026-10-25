package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

const (
	maxReadBytes       = 2 << 20
	maxReadLines       = 5000
	maxSearchFiles     = 5000
	maxSearchFileBytes = 512 << 10
	maxMatches         = 500
	maxGlobEntries     = 20000
	maxGlobResults     = 1000
	maxDirEntries      = 1000
	maxRegexpLen       = 512
	maxPatternLen      = 512
	maxLineBytes       = 64 << 10
	maxMatchLineBytes  = 4 << 10
	maxGitBlameLines   = 400
)

// BuiltinTools returns the read-only tool set confined to sb. The order is
// stable: read_file, grep, glob, list_dir, git_log, git_blame.
func BuiltinTools(sb *Sandbox) []Tool {
	if sb == nil {
		return nil
	}
	return []Tool{
		readFileTool{sb: sb},
		grepTool{sb: sb},
		globTool{sb: sb},
		listDirTool{sb: sb},
		gitLogTool{sb: sb},
		gitBlameTool{sb: sb},
	}
}

type readFileInput struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line,omitempty"`
	EndLine   int    `json:"end_line,omitempty"`
	Root      string `json:"root,omitempty"`
}

type readFileTool struct{ sb *Sandbox }

func (t readFileTool) Def() llm.ToolDef {
	return def("read_file",
		"Read a text file with line numbers. Paths are relative to the selected root.",
		`{"type":"object","required":["path"],"properties":{
			"path":{"type":"string"},
			"start_line":{"type":"integer","minimum":1},
			"end_line":{"type":"integer","minimum":0},
			"root":{"type":"string"}},
			"additionalProperties":false}`)
}

func (t readFileTool) Call(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in readFileInput
	if err := decodeInput(raw, &in); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return Result{}, errors.New("path is required")
	}
	start := in.StartLine
	if start == 0 {
		start = 1
	}
	if start < 1 {
		return Result{}, errors.New("start_line must be at least 1")
	}
	if in.EndLine < 0 {
		return Result{}, errors.New("end_line must not be negative")
	}
	if in.EndLine > 0 && in.EndLine < start {
		return Result{}, errors.New("end_line must be greater than or equal to start_line")
	}

	file, clean, err := t.sb.open(in.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	defer file.Close()

	data, truncated, err := readBounded(file, maxReadBytes)
	if err != nil {
		return Result{}, err
	}
	if bytes.IndexByte(data, 0) >= 0 {
		return Result{}, fmt.Errorf("%s is a binary file", clean)
	}
	if !utf8.Valid(data) {
		return Result{}, fmt.Errorf("%s is not valid UTF-8 text", clean)
	}

	lines := splitLines(data)
	end := in.EndLine
	if end == 0 || end > len(lines) {
		end = len(lines)
	}
	if start > len(lines) {
		return Result{Content: fmt.Sprintf("(no lines: file has %d lines)", len(lines))}, nil
	}
	if end > start+maxReadLines-1 {
		end = start + maxReadLines - 1
		truncated = true
	}

	var b strings.Builder
	for i := start; i <= end; i++ {
		fmt.Fprintf(&b, "%6d\t%s\n", i, lines[i-1])
	}
	if truncated {
		b.WriteString("[truncated: read limit reached]\n")
	}
	return Result{Content: strings.TrimSuffix(b.String(), "\n"), Truncated: truncated}, nil
}

func readBounded(r io.Reader, max int) ([]byte, bool, error) {
	data, err := io.ReadAll(io.LimitReader(r, int64(max)+1))
	if err != nil {
		return nil, false, err
	}
	if len(data) > max {
		return data[:max], true, nil
	}
	return data, false, nil
}

func splitLines(data []byte) []string {
	text := string(data)
	text = strings.TrimSuffix(text, "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

type grepInput struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path,omitempty"`
	Glob       string `json:"glob,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
	Root       string `json:"root,omitempty"`
}

type grepTool struct{ sb *Sandbox }

func (t grepTool) Def() llm.ToolDef {
	return def("grep",
		"Search text files with a Go regular expression. Returns path:line: text.",
		`{"type":"object","required":["pattern"],"properties":{
			"pattern":{"type":"string"},
			"path":{"type":"string"},
			"glob":{"type":"string"},
			"max_results":{"type":"integer","minimum":1,"maximum":500},
			"root":{"type":"string"}},
			"additionalProperties":false}`)
}

func (t grepTool) Call(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in grepInput
	if err := decodeInput(raw, &in); err != nil {
		return Result{}, err
	}
	if in.Pattern == "" {
		return Result{}, errors.New("pattern is required")
	}
	if len(in.Pattern) > maxRegexpLen {
		return Result{}, fmt.Errorf("pattern exceeds %d bytes", maxRegexpLen)
	}
	re, err := regexp.Compile(in.Pattern)
	if err != nil {
		return Result{}, fmt.Errorf("invalid regular expression: %w", err)
	}
	limit := in.MaxResults
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > maxMatches {
		return Result{}, fmt.Errorf("max_results must be between 1 and %d", maxMatches)
	}
	var glob *globPattern
	if in.Glob != "" {
		glob, err = compileGlob(in.Glob)
		if err != nil {
			return Result{}, err
		}
	}

	root, _, clean, err := t.sb.resolve(in.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	info, err := root.Stat(filepathFromSlash(clean))
	if err != nil {
		return Result{}, fmt.Errorf("agent: stat %s: %w", clean, err)
	}

	var out []string
	scanned := 0
	visited := 0
	truncated := false
	addMatches := func(name string, file fs.File) error {
		scanned++
		content, err := io.ReadAll(io.LimitReader(file, maxSearchFileBytes+1))
		if err != nil {
			return nil
		}
		if len(content) > maxSearchFileBytes {
			content = content[:maxSearchFileBytes]
			truncated = true
		}
		if bytes.IndexByte(content, 0) >= 0 {
			return nil
		}
		scanner := bufio.NewScanner(bytes.NewReader(content))
		scanner.Buffer(make([]byte, 0, 64*1024), maxLineBytes)
		lineNo := 0
		for scanner.Scan() {
			lineNo++
			line := scanner.Text()
			if !re.MatchString(line) {
				continue
			}
			if len(out) >= limit {
				truncated = true
				break
			}
			if len(line) > maxMatchLineBytes {
				line = line[:maxMatchLineBytes] + " [line truncated]"
			}
			out = append(out, fmt.Sprintf("%s:%d: %s", name, lineNo, line))
		}
		if err := scanner.Err(); err != nil {
			truncated = true
		}
		return nil
	}

	if info.Mode().IsRegular() {
		if glob != nil && !glob.match(clean) {
			return Result{Content: "(no matches)"}, nil
		}
		file, openErr := root.Open(filepathFromSlash(clean))
		if openErr != nil {
			return Result{}, openErr
		}
		defer file.Close()
		if err := addMatches(clean, file); err != nil {
			return Result{}, err
		}
	} else if info.IsDir() {
		fsys := root.FS()
		walkErr := fs.WalkDir(fsys, filepathFromSlash(clean), func(name string, d fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return nil
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			visited++
			if visited > maxGlobEntries {
				truncated = true
				return fs.SkipAll
			}
			if pathWithin(name) {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if d.Type()&fs.ModeSymlink != 0 {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil || !info.Mode().IsRegular() || info.Size() > maxSearchFileBytes {
				return nil
			}
			name = filepath.ToSlash(name)
			if glob != nil && !glob.match(name) {
				return nil
			}
			if len(out) >= limit {
				truncated = true
				return fs.SkipAll
			}
			if scanned >= maxSearchFiles {
				truncated = true
				return fs.SkipAll
			}
			file, err := root.Open(filepathFromSlash(name))
			if err != nil {
				return nil
			}
			scanErr := addMatches(name, file)
			file.Close()
			return scanErr
		})
		if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
			return Result{}, walkErr
		}
	} else {
		return Result{}, fmt.Errorf("%s is not a regular file or directory", clean)
	}

	if len(out) == 0 && !truncated {
		return Result{Content: "(no matches)"}, nil
	}
	content := strings.Join(out, "\n")
	if truncated {
		content += "\n[truncated: search limit reached]"
	}
	return Result{Content: content, Truncated: truncated}, nil
}

type globInput struct {
	Pattern    string `json:"pattern"`
	MaxResults int    `json:"max_results,omitempty"`
	Root       string `json:"root,omitempty"`
}

type globTool struct{ sb *Sandbox }

func (t globTool) Def() llm.ToolDef {
	return def("glob",
		"List files matching a glob. Supports *, ?, [...] and ** across path segments.",
		`{"type":"object","required":["pattern"],"properties":{
			"pattern":{"type":"string"},
			"max_results":{"type":"integer","minimum":1,"maximum":1000},
			"root":{"type":"string"}},
			"additionalProperties":false}`)
}

func (t globTool) Call(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in globInput
	if err := decodeInput(raw, &in); err != nil {
		return Result{}, err
	}
	pattern, err := compileGlob(in.Pattern)
	if err != nil {
		return Result{}, err
	}
	limit := in.MaxResults
	if limit == 0 {
		limit = 200
	}
	if limit < 1 || limit > maxGlobResults {
		return Result{}, fmt.Errorf("max_results must be between 1 and %d", maxGlobResults)
	}
	root, _, _, err := t.sb.resolve(in.Root, ".")
	if err != nil {
		return Result{}, err
	}

	var results []string
	walked := 0
	truncated := false
	if pattern.base {
		dir, err := root.Open(".")
		if err != nil {
			return Result{}, err
		}
		defer dir.Close()
		entries, err := dir.ReadDir(maxGlobEntries + 1)
		if err != nil && !errors.Is(err, io.EOF) {
			return Result{}, err
		}
		if len(entries) > maxGlobEntries {
			entries = entries[:maxGlobEntries]
			truncated = true
		}
		for _, entry := range entries {
			if err := ctx.Err(); err != nil {
				return Result{}, err
			}
			if pathWithin(entry.Name()) || entry.IsDir() || entry.Type()&fs.ModeSymlink != 0 {
				continue
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				continue
			}
			if !pattern.match(entry.Name()) {
				continue
			}
			if len(results) >= limit {
				truncated = true
				break
			}
			results = append(results, entry.Name())
		}
		if len(results) == 0 && !truncated {
			return Result{Content: "(no matches)"}, nil
		}
		sort.Strings(results)
		content := strings.Join(results, "\n")
		if truncated {
			content += "\n[truncated: glob limit reached]"
		}
		return Result{Content: content, Truncated: truncated}, nil
	}
	walkErr := fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		walked++
		if walked > maxGlobEntries {
			truncated = true
			return fs.SkipAll
		}
		if pathWithin(name) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil
		}
		if !pattern.match(name) {
			return nil
		}
		if len(results) >= limit {
			truncated = true
			return fs.SkipAll
		}
		results = append(results, filepath.ToSlash(name))
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, fs.SkipAll) {
		return Result{}, walkErr
	}
	if len(results) == 0 && !truncated {
		return Result{Content: "(no matches)"}, nil
	}
	sort.Strings(results)
	content := strings.Join(results, "\n")
	if truncated {
		content += "\n[truncated: glob limit reached]"
	}
	return Result{Content: content, Truncated: truncated}, nil
}

type listDirInput struct {
	Path string `json:"path,omitempty"`
	Root string `json:"root,omitempty"`
}

type listDirTool struct{ sb *Sandbox }

func (t listDirTool) Def() llm.ToolDef {
	return def("list_dir",
		"List one directory level. Directories end with /, symlinks with @.",
		`{"type":"object","properties":{
			"path":{"type":"string"},
			"root":{"type":"string"}},
			"additionalProperties":false}`)
}

func (t listDirTool) Call(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in listDirInput
	if err := decodeInput(raw, &in); err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	root, _, clean, err := t.sb.resolve(in.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	dir, err := root.Open(filepathFromSlash(clean))
	if err != nil {
		return Result{}, fmt.Errorf("agent: open %s: %w", clean, err)
	}
	defer dir.Close()
	info, err := dir.Stat()
	if err != nil {
		return Result{}, err
	}
	if !info.IsDir() {
		return Result{}, fmt.Errorf("%s is not a directory", clean)
	}
	entries, err := dir.ReadDir(maxDirEntries + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return Result{}, err
	}
	truncated := len(entries) > maxDirEntries
	if truncated {
		entries = entries[:maxDirEntries]
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), ".git") {
			continue
		}
		name := entry.Name()
		switch {
		case entry.Type()&fs.ModeSymlink != 0:
			name += "@"
		case entry.IsDir():
			name += "/"
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return Result{Content: "(empty directory)"}, nil
	}
	sort.Strings(names)
	content := strings.Join(names, "\n")
	if truncated {
		content += "\n[truncated: directory entry limit reached]"
	}
	return Result{Content: content, Truncated: truncated}, nil
}

func def(name, description, schema string) llm.ToolDef {
	return llm.ToolDef{Name: name, Description: description, InputSchema: json.RawMessage(schema)}
}

func decodeInput(raw json.RawMessage, out any) error {
	if len(raw) == 0 {
		raw = json.RawMessage(`{}`)
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("invalid input: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("invalid input: trailing JSON")
	}
	return nil
}

func filepathFromSlash(name string) string {
	return strings.ReplaceAll(name, "/", string(os.PathSeparator))
}

type globPattern struct {
	pattern string
	re      *regexp.Regexp
	base    bool
}

func compileGlob(pattern string) (*globPattern, error) {
	if pattern == "" {
		return nil, errors.New("pattern is required")
	}
	if len(pattern) > maxPatternLen {
		return nil, fmt.Errorf("pattern exceeds %d bytes", maxPatternLen)
	}
	if strings.ContainsAny(pattern, "\x00\r\n") {
		return nil, fmt.Errorf("%w: pattern contains a control character", ErrUnsafePath)
	}
	if strings.HasPrefix(pattern, "/") || filepath.IsAbs(pattern) {
		return nil, fmt.Errorf("%w: absolute patterns are not allowed", ErrUnsafePath)
	}
	for _, segment := range strings.Split(filepath.ToSlash(pattern), "/") {
		if segment == ".." {
			return nil, fmt.Errorf("%w: pattern escapes the sandbox", ErrUnsafePath)
		}
		if strings.EqualFold(segment, ".git") {
			return nil, fmt.Errorf("%w: .git internals are not searchable", ErrUnsafePath)
		}
	}
	expr, err := globRegexp(pattern)
	if err != nil {
		return nil, err
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return nil, fmt.Errorf("invalid glob: %w", err)
	}
	return &globPattern{pattern: pattern, re: re, base: !strings.Contains(pattern, "/")}, nil
}

func (p *globPattern) match(name string) bool {
	name = filepath.ToSlash(name)
	if p.base {
		return p.re.MatchString(path.Base(name))
	}
	return p.re.MatchString(name)
}

func globRegexp(pattern string) (string, error) {
	var b strings.Builder
	b.WriteString("^")
	runes := []rune(pattern)
	for i := 0; i < len(runes); i++ {
		switch runes[i] {
		case '*':
			if i+1 < len(runes) && runes[i+1] == '*' {
				i++
				if i+1 < len(runes) && runes[i+1] == '/' {
					i++
					b.WriteString("(?:[^/]+/)*")
				} else {
					b.WriteString(".*")
				}
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		case '[':
			end := i + 1
			for end < len(runes) && runes[end] != ']' {
				end++
			}
			if end >= len(runes) {
				return "", errors.New("invalid glob: unterminated character class")
			}
			class := runes[i+1 : end]
			i = end
			negate := len(class) > 0 && class[0] == '!'
			if negate {
				class = class[1:]
			}
			if len(class) == 0 {
				return "", errors.New("invalid glob: empty character class")
			}
			b.WriteByte('[')
			if negate {
				b.WriteByte('^')
			}
			for j, r := range class {
				switch {
				case r == '\\' || r == ']':
					b.WriteByte('\\')
					b.WriteRune(r)
				case r == '^' && j == 0:
					b.WriteString(`\^`)
				default:
					b.WriteRune(r)
				}
			}
			b.WriteByte(']')
		case '\\':
			if i+1 < len(runes) {
				i++
				b.WriteString(regexp.QuoteMeta(string(runes[i])))
			} else {
				b.WriteString(`\\`)
			}
		default:
			b.WriteString(regexp.QuoteMeta(string(runes[i])))
		}
	}
	b.WriteString("$")
	return b.String(), nil
}
