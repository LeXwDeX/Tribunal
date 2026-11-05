package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
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

	mu              sync.Mutex
	read            map[string]map[int]map[int]bool
	parts           map[string]map[int]map[int][]readPart
	pendingEvidence map[string][]pendingRead
	delivered       map[string]bool
	totals          map[string]map[int]int
	lineTotals      map[string]map[int]map[int]int
	complete        map[string]map[int]bool
}

type readPart struct {
	id    string
	start int
	end   int
}

type pendingRead struct {
	path  string
	hunk  int
	line  int
	start int
	end   int
	total int
}

// NewChangeSource builds a change source. files are the current package;
// all is the full MR file list used by list_changes with all=true.
func NewChangeSource(files, all []ChangeFile, inline map[string]bool) *ChangeSource {
	return &ChangeSource{
		Files:           append([]ChangeFile(nil), files...),
		AllFiles:        append([]ChangeFile(nil), all...),
		Inline:          inline,
		read:            make(map[string]map[int]map[int]bool),
		parts:           make(map[string]map[int]map[int][]readPart),
		pendingEvidence: make(map[string][]pendingRead),
		delivered:       make(map[string]bool),
		totals:          make(map[string]map[int]int),
		lineTotals:      make(map[string]map[int]map[int]int),
		complete:        make(map[string]map[int]bool),
	}
}

// Flush commits reads that the next model request will see. Calling it after
// a submit ends the run without committing that run's final read output.
func (c *ChangeSource) Flush() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := range c.pendingEvidence {
		c.delivered[id] = true
	}
	c.pendingEvidence = make(map[string][]pendingRead)
	c.recomputeCompleteLocked()
}

// Reset discards every read. A transport retry starts a fresh conversation,
// so evidence from the failed attempt must not cover the next one.
// DropEvidence removes reads whose tool output context compaction replaced
// before the model saw them.
func (c *ChangeSource) DropEvidence(ids []string) {
	if c == nil || len(ids) == 0 {
		return
	}
	drop := make(map[string]bool, len(ids))
	for _, id := range ids {
		if !c.delivered[id] {
			drop[id] = true
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, id := range ids {
		delete(c.pendingEvidence, id)
	}
	for path, hunks := range c.parts {
		for hunk, lines := range hunks {
			for line, parts := range lines {
				kept := parts[:0]
				for _, part := range parts {
					if !drop[part.id] {
						kept = append(kept, part)
					}
				}
				if len(kept) == 0 {
					delete(lines, line)
				} else {
					lines[line] = kept
				}
			}
			if len(lines) == 0 {
				delete(hunks, hunk)
			}
		}
		if len(hunks) == 0 {
			delete(c.parts, path)
		}
	}
	c.recomputeCompleteLocked()
}

func (c *ChangeSource) Reset() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.read = make(map[string]map[int]map[int]bool)
	c.parts = make(map[string]map[int]map[int][]readPart)
	c.pendingEvidence = make(map[string][]pendingRead)
	c.delivered = make(map[string]bool)
	c.totals = make(map[string]map[int]int)
	c.lineTotals = make(map[string]map[int]map[int]int)
	c.complete = make(map[string]map[int]bool)
}

func (c *ChangeSource) recomputeCompleteLocked() {
	c.read = make(map[string]map[int]map[int]bool)
	c.complete = make(map[string]map[int]bool)
	for path, hunks := range c.totals {
		for hunk, total := range hunks {
			if total <= 0 {
				continue
			}
			complete := true
			for line := 1; line <= total; line++ {
				lineTotal, ok := c.lineTotals[path][hunk][line]
				if !ok || !c.lineCoveredLocked(path, hunk, line, lineTotal) {
					complete = false
					break
				}
				if c.read[path] == nil {
					c.read[path] = make(map[int]map[int]bool)
				}
				if c.read[path][hunk] == nil {
					c.read[path][hunk] = make(map[int]bool)
				}
				c.read[path][hunk][line] = true
			}
			if complete {
				if c.complete[path] == nil {
					c.complete[path] = make(map[int]bool)
				}
				c.complete[path][hunk] = true
			}
		}
	}
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
	b.WriteString("path\told_path\tstatus\tadditions\tdeletions\tdiff_runes\thunks\tgenerated\tinlined\n")
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
		fmt.Fprintf(&b, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%t\t%s\n",
			file.Path, file.OldPath, file.Status, file.Additions, file.Deletions,
			utf8.RuneCountInString(file.Diff), countDiffHunks(file.Diff), file.Generated, inlined)
	}
	b.WriteString("\nfiles not inlined in the prompt must be read with read_diff before claiming coverage.\n")
	return Result{Content: strings.TrimSuffix(b.String(), "\n")}, nil
}

type readDiffInput struct {
	Path       string `json:"path"`
	FromHunk   int    `json:"from_hunk,omitempty"`
	MaxHunks   int    `json:"max_hunks,omitempty"`
	FromLine   int    `json:"from_line,omitempty"`
	FromOffset int    `json:"from_offset,omitempty"`
}

var readDiffSequence atomic.Int64

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
			"from_line":{"type":"integer","minimum":1},
			"from_offset":{"type":"integer","minimum":0}},
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
	evidenceID := fmt.Sprintf("read-%d", readDiffSequence.Add(1))
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
	if in.FromOffset < 0 {
		return Result{}, inputFeedback("from_offset", "range", errors.New("from_offset must not be negative"))
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
	nextHunk, nextLine, nextOffset := 0, 0, 0
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
		t.changes.setHunkTotal(file.Path, hunk, len(content.lines))
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
		for i := start; i < len(content.lines); i++ {
			line := content.lines[i]
			lineRunes := utf8.RuneCountInString(line)
			offset := 0
			if hunk == fromHunk && i == start && in.FromOffset > 0 {
				offset = in.FromOffset
				if offset > lineRunes {
					offset = lineRunes
				}
			}
			remainingRunes := maxDiffRunesPerCall - runes
			remainingLines := maxDiffLinesPerCall - lines
			if remainingRunes <= 0 || remainingLines <= 0 {
				nextHunk, nextLine, nextOffset = hunk, i+1, offset
				stopped = true
				break
			}
			available := lineRunes - offset
			if available+1 > remainingRunes {
				// A single line larger than the read budget is paged by rune
				// offset so the cursor always advances.
				partRunes := remainingRunes - 1
				if partRunes < 1 {
					partRunes = 1
				}
				end := offset + partRunes
				if end > lineRunes {
					end = lineRunes
				}
				part := sliceRunes(line, offset, end)
				b.WriteString(part)
				runes += utf8.RuneCountInString(part)
				t.changes.recordLinePart(evidenceID, file.Path, hunk, i+1, offset, end, lineRunes)
				nextHunk, nextLine, nextOffset = hunk, i+1, end
				stopped = true
				break
			}
			part := sliceRunes(line, offset, lineRunes)
			b.WriteString(part)
			b.WriteByte('\n')
			runes += utf8.RuneCountInString(part) + 1
			lines++
			t.changes.recordLinePart(evidenceID, file.Path, hunk, i+1, offset, lineRunes, lineRunes)
		}
		if stopped {
			break
		}
	}
	if !stopped && lastHunk < len(hunks) {
		nextHunk, nextLine = lastHunk+1, 1
	}
	if nextHunk > 0 {
		if nextOffset > 0 {
			fmt.Fprintf(&b, "\n[read_diff] next_hunk: %d, next_line: %d, next_offset: %d\n", nextHunk, nextLine, nextOffset)
		} else {
			fmt.Fprintf(&b, "\n[read_diff] next_hunk: %d, next_line: %d\n", nextHunk, nextLine)
		}
	} else {
		b.WriteString("\n[read_diff] end of diff\n")
	}
	return Result{Content: strings.TrimSuffix(b.String(), "\n"), Truncated: stopped, EvidenceID: evidenceID}, nil
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

func (c *ChangeSource) setHunkTotal(path string, hunk, total int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.totals[path] == nil {
		c.totals[path] = make(map[int]int)
	}
	c.totals[path][hunk] = total
}

func (c *ChangeSource) recordLinePart(id, path string, hunk, line, start, end, total int) {
	if start < 0 {
		start = 0
	}
	if end > total {
		end = total
	}
	if start >= end {
		if total == 0 {
			c.mu.Lock()
			if c.lineTotals[path] == nil {
				c.lineTotals[path] = make(map[int]map[int]int)
			}
			if c.lineTotals[path][hunk] == nil {
				c.lineTotals[path][hunk] = make(map[int]int)
			}
			c.lineTotals[path][hunk][line] = 0
			if c.parts[path] == nil {
				c.parts[path] = make(map[int]map[int][]readPart)
			}
			if c.parts[path][hunk] == nil {
				c.parts[path][hunk] = make(map[int][]readPart)
			}
			c.parts[path][hunk][line] = append(c.parts[path][hunk][line], readPart{id: id, start: 0, end: 0})
			c.pendingEvidence[id] = append(c.pendingEvidence[id], pendingRead{
				path: path, hunk: hunk, line: line, start: 0, end: 0, total: 0,
			})
			c.mu.Unlock()
		}
		return
	}
	c.mu.Lock()
	if c.parts[path] == nil {
		c.parts[path] = make(map[int]map[int][]readPart)
	}
	if c.parts[path][hunk] == nil {
		c.parts[path][hunk] = make(map[int][]readPart)
	}
	c.parts[path][hunk][line] = append(c.parts[path][hunk][line], readPart{id: id, start: start, end: end})
	if c.lineTotals[path] == nil {
		c.lineTotals[path] = make(map[int]map[int]int)
	}
	if c.lineTotals[path][hunk] == nil {
		c.lineTotals[path][hunk] = make(map[int]int)
	}
	c.lineTotals[path][hunk][line] = total
	c.pendingEvidence[id] = append(c.pendingEvidence[id], pendingRead{
		path: path, hunk: hunk, line: line, start: start, end: end, total: total,
	})
	c.mu.Unlock()
}

func (c *ChangeSource) lineCoveredLocked(path string, hunk, line, total int) bool {
	if total <= 0 {
		return len(c.parts[path][hunk][line]) > 0
	}
	intervals := append([]readPart(nil), c.parts[path][hunk][line]...)
	sort.Slice(intervals, func(i, j int) bool { return intervals[i].start < intervals[j].start })
	covered := 0
	for _, interval := range intervals {
		if interval.start > covered {
			break
		}
		if interval.end > covered {
			covered = interval.end
		}
	}
	return covered >= total
}

func sliceRunes(value string, start, end int) string {
	runes := []rune(value)
	if start < 0 {
		start = 0
	}
	if end > len(runes) {
		end = len(runes)
	}
	if start > end {
		start = end
	}
	return string(runes[start:end])
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
