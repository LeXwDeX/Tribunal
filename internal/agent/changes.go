package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

// ChangeFile is the diff view a review tool may read. It is a copy of the
// fixed dossier entry so package agent never imports internal/review.
type ChangeFile struct {
	Path      string
	OldPath   string
	Status    string
	Additions int
	Deletions int
	Diff      string
	Generated bool
}

// ChangeSource is the immutable change snapshot plus the mutable record of
// hunks the model has read. Review nodes create one per run and copy its
// DiffReads into the node output so coverage is judged from real reads.
type ChangeSource struct {
	Files    []ChangeFile
	AllFiles []ChangeFile
	Inline   map[string]bool

	mu       sync.Mutex
	read     map[string]map[int]map[int]bool
	complete map[string]map[int]bool
}

// NewChangeSource builds a change source. files are the current package;
// all is the full MR file list used by list_changes with all=true.
func NewChangeSource(files, all []ChangeFile, inline map[string]bool) *ChangeSource {
	return &ChangeSource{
		Files:    append([]ChangeFile(nil), files...),
		AllFiles: append([]ChangeFile(nil), all...),
		Inline:   inline,
		read:     make(map[string]map[int]map[int]bool),
		complete: make(map[string]map[int]bool),
	}
}

// RecordLine records one hunk line as read.
func (c *ChangeSource) RecordLine(path string, hunk, line int) {
	if c == nil || hunk < 1 || line < 1 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.read == nil {
		c.read = make(map[string]map[int]map[int]bool)
	}
	byHunk := c.read[path]
	if byHunk == nil {
		byHunk = make(map[int]map[int]bool)
		c.read[path] = byHunk
	}
	lines := byHunk[hunk]
	if lines == nil {
		lines = make(map[int]bool)
		byHunk[hunk] = lines
	}
	lines[line] = true
}

// completeHunk reports whether every line 1..total has been read.
func (c *ChangeSource) completeHunk(path string, hunk, total int) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if total <= 0 {
		return true
	}
	lines := c.read[path][hunk]
	for line := 1; line <= total; line++ {
		if !lines[line] {
			return false
		}
	}
	return true
}

// DiffReads returns fully read hunk indexes per path in ascending order.
func (c *ChangeSource) DiffReads() map[string][]int {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string][]int, len(c.read))
	for path, hunks := range c.read {
		indexes := make([]int, 0, len(hunks))
		for hunk := range c.complete[path] {
			indexes = append(indexes, hunk)
		}
		if len(indexes) > 0 {
			sort.Ints(indexes)
			out[path] = indexes
		}
	}
	return out
}

type listChangesInput struct {
	All bool `json:"all,omitempty"`
}

type listChangesTool struct {
	sb      *Sandbox
	changes *ChangeSource
}

func (t listChangesTool) Def() llm.ToolDef {
	return def(t.sb, "list_changes",
		"List changed files in this review package or the whole merge request, including diff size, hunk count and whether the diff was inlined in the prompt.",
		`{"type":"object","properties":{"all":{"type":"boolean"}},"additionalProperties":false}`)
}

func (t listChangesTool) Call(_ context.Context, raw json.RawMessage) (Result, error) {
	var in listChangesInput
	if err := decodeInput(raw, &in); err != nil {
		return Result{}, err
	}
	if t.changes == nil {
		return Result{Content: "(no change snapshot is available for this node)"}, nil
	}
	files := t.changes.Files
	if in.All && len(t.changes.AllFiles) > 0 {
		files = t.changes.AllFiles
	}
	if len(files) == 0 {
		return Result{Content: "(no changed files)"}, nil
	}
	var b strings.Builder
	b.WriteString("path\tstatus\tadditions\tdeletions\tdiff_runes\thunks\tgenerated\tinlined\n")
	for _, file := range files {
		inlined := "unknown"
		if t.changes.Inline != nil {
			if value, ok := t.changes.Inline[file.Path]; ok {
				if value {
					inlined = "yes"
				} else {
					inlined = "no"
				}
			}
		}
		fmt.Fprintf(&b, "%s\t%s\t%d\t%d\t%d\t%d\t%t\t%s\n",
			file.Path, file.Status, file.Additions, file.Deletions,
			utf8.RuneCountInString(file.Diff), countDiffHunks(file.Diff), file.Generated, inlined)
	}
	b.WriteString("\nfiles not inlined in the prompt must be read with read_diff before claiming coverage.\n")
	return Result{Content: strings.TrimSuffix(b.String(), "\n")}, nil
}

type readDiffInput struct {
	Path     string `json:"path"`
	FromHunk int    `json:"from_hunk,omitempty"`
	MaxHunks int    `json:"max_hunks,omitempty"`
	FromLine int    `json:"from_line,omitempty"`
}

const (
	maxDiffHunksPerCall = 20
	maxDiffLinesPerCall = 400
	maxDiffRunesPerCall = 12000
)

type diffHunk struct {
	header string
	lines  []string
}

type readDiffTool struct {
	sb      *Sandbox
	changes *ChangeSource
}

func (t readDiffTool) Def() llm.ToolDef {
	return def(t.sb, "read_diff",
		"Read one file's diff by hunk. Returns hunk headers, a next_hunk/next_line cursor and records the lines read for coverage.",
		`{"type":"object","required":["path"],"properties":{
			"path":{"type":"string"},
			"from_hunk":{"type":"integer","minimum":1},
			"max_hunks":{"type":"integer","minimum":1,"maximum":20},
			"from_line":{"type":"integer","minimum":1}},
			"additionalProperties":false}`)
}

func (t readDiffTool) Call(_ context.Context, raw json.RawMessage) (Result, error) {
	var in readDiffInput
	if err := decodeInput(raw, &in); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return Result{}, inputFeedback("path", "required", errors.New("path is required"))
	}
	if t.changes == nil {
		return Result{Content: "(no change snapshot is available for this node)"}, nil
	}
	file, ok := t.changes.find(in.Path)
	if !ok {
		return Result{}, inputFeedback("path", "missing", fmt.Errorf("%s is not a changed file in this review", in.Path))
	}
	preamble, hunks := splitDiffHunks(file.Diff)
	if len(hunks) == 0 {
		return Result{Content: fmt.Sprintf("%s has no diff hunks", file.Path)}, nil
	}
	fromHunk := in.FromHunk
	if fromHunk == 0 {
		fromHunk = 1
	}
	if fromHunk < 1 || fromHunk > len(hunks) {
		return Result{}, inputFeedback("from_hunk", "range", fmt.Errorf("from_hunk must be between 1 and %d", len(hunks)))
	}
	maxHunks := in.MaxHunks
	if maxHunks == 0 {
		maxHunks = 1
	}
	if maxHunks < 1 || maxHunks > maxDiffHunksPerCall {
		return Result{}, inputFeedback("max_hunks", "range", fmt.Errorf("max_hunks must be between 1 and %d", maxDiffHunksPerCall))
	}
	fromLine := in.FromLine
	if fromLine < 0 {
		return Result{}, inputFeedback("from_line", "range", errors.New("from_line must not be negative"))
	}
	if fromLine == 0 {
		fromLine = 1
	}

	var b strings.Builder
	runes := 0
	lines := 0
	if fromHunk == 1 && fromLine == 1 && len(preamble) > 0 {
		for _, line := range preamble {
			b.WriteString(line)
			b.WriteByte('\n')
			runes += utf8.RuneCountInString(line) + 1
			lines++
		}
	}
	nextHunk, nextLine := 0, 0
	stopped := false
	lastHunk := fromHunk - 1
	for hunk := fromHunk; hunk <= len(hunks) && hunk < fromHunk+maxHunks; hunk++ {
		lastHunk = hunk
		content := hunks[hunk-1]
		start := 0
		if hunk == fromHunk {
			start = fromLine - 1
			if start < 0 {
				start = 0
			}
			if start > len(content.lines) {
				return Result{}, inputFeedback("from_line", "range", fmt.Errorf("from_line exceeds hunk %d line count %d", hunk, len(content.lines)))
			}
		}
		header := content.header
		if start == 0 {
			if runes+utf8.RuneCountInString(header)+1 > maxDiffRunesPerCall || lines >= maxDiffLinesPerCall {
				nextHunk, nextLine = hunk, 1
				stopped = true
				break
			}
			b.WriteString(header)
			b.WriteByte('\n')
			runes += utf8.RuneCountInString(header) + 1
			lines++
		}
		wrote := 0
		for i := start; i < len(content.lines); i++ {
			line := content.lines[i]
			if lines >= maxDiffLinesPerCall || runes+utf8.RuneCountInString(line)+1 > maxDiffRunesPerCall {
				nextHunk, nextLine = hunk, i+1
				stopped = true
				break
			}
			b.WriteString(line)
			b.WriteByte('\n')
			runes += utf8.RuneCountInString(line) + 1
			lines++
			wrote++
		}
		if stopped {
			if wrote > 0 {
				// Only the complete prefix of this hunk is recorded.
				t.changes.recordHunkLines(file.Path, hunk, start+1, start+wrote, len(content.lines))
			}
			break
		}
		t.changes.recordHunkLines(file.Path, hunk, start+1, len(content.lines), len(content.lines))
	}
	if !stopped && lastHunk < len(hunks) {
		nextHunk, nextLine = lastHunk+1, 1
	}
	if nextHunk > 0 {
		fmt.Fprintf(&b, "\n[read_diff] next_hunk: %d, next_line: %d\n", nextHunk, nextLine)
	} else {
		b.WriteString("\n[read_diff] end of diff\n")
	}
	return Result{Content: strings.TrimSuffix(b.String(), "\n"), Truncated: stopped}, nil
}

func (c *ChangeSource) find(path string) (ChangeFile, bool) {
	for _, file := range c.Files {
		if file.Path == path {
			return file, true
		}
	}
	for _, file := range c.AllFiles {
		if file.Path == path {
			return file, true
		}
	}
	return ChangeFile{}, false
}

func (c *ChangeSource) recordHunkLines(path string, hunk, first, last, total int) {
	for line := first; line <= last && line <= total; line++ {
		c.RecordLine(path, hunk, line)
	}
	if c.completeHunk(path, hunk, total) {
		c.mu.Lock()
		if c.complete[path] == nil {
			c.complete[path] = make(map[int]bool)
		}
		c.complete[path][hunk] = true
		c.mu.Unlock()
	}
}

func countDiffHunks(diff string) int {
	_, hunks := splitDiffHunks(diff)
	return len(hunks)
}

// splitDiffHunks keeps the file preamble separate from @@ hunks.
func splitDiffHunks(diff string) ([]string, []diffHunk) {
	diff = strings.TrimSuffix(diff, "\n")
	if diff == "" {
		return nil, nil
	}
	lines := strings.Split(diff, "\n")
	var preamble []string
	var hunks []diffHunk
	for _, line := range lines {
		if strings.HasPrefix(line, "@@") {
			hunks = append(hunks, diffHunk{header: line})
			continue
		}
		if len(hunks) == 0 {
			preamble = append(preamble, line)
		} else {
			hunks[len(hunks)-1].lines = append(hunks[len(hunks)-1].lines, line)
		}
	}
	return preamble, hunks
}
