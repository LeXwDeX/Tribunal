package agent

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
)

func changeSandbox(t *testing.T) *Sandbox {
	t.Helper()
	sb, _, _ := newTestSandbox(t)
	t.Cleanup(func() { sb.Close() })
	return sb
}

func TestBuiltinToolsWithChangesAddsReviewTools(t *testing.T) {
	sb := changeSandbox(t)
	base := BuiltinTools(sb)
	for _, name := range []string{"list_changes", "read_diff"} {
		if hasTool(base, name) {
			t.Fatalf("base builtins unexpectedly contain %s", name)
		}
	}
	changes := NewChangeSource(nil, nil, nil)
	withChanges := BuiltinToolsWithChanges(sb, changes)
	for _, name := range []string{"list_changes", "read_diff"} {
		if !hasTool(withChanges, name) {
			t.Fatalf("change builtins are missing %s", name)
		}
	}
}

func hasTool(tools []Tool, name string) bool {
	for _, tool := range tools {
		if tool.Def().Name == name {
			return true
		}
	}
	return false
}

func TestReadDiffPagesAndRecordsCompleteHunks(t *testing.T) {
	diff := "@@ -1 +1 @@\n+one\n@@ -10 +10 @@\n+two\n"
	source := NewChangeSource([]ChangeFile{{Path: "a.go", Status: "modified", Diff: diff}}, nil, nil)
	sb := changeSandbox(t)
	read := toolByName(t, BuiltinToolsWithChanges(sb, source), "read_diff")

	first, err := invoke(t, read, `{"path":"a.go","from_hunk":1,"max_hunks":1}`)
	if err != nil {
		t.Fatalf("read first hunk: %v", err)
	}
	if !strings.Contains(first.Content, "+one") || !strings.Contains(first.Content, "next_hunk: 2") {
		t.Fatalf("first page = %q", first.Content)
	}
	source.Flush()
	if got := source.DiffReads()["a.go"]; len(got) != 1 || got[0] != 1 {
		t.Fatalf("diff reads after first page = %v, want [1]", got)
	}

	second, err := invoke(t, read, `{"path":"a.go","from_hunk":2,"max_hunks":1}`)
	if err != nil {
		t.Fatalf("read second hunk: %v", err)
	}
	if !strings.Contains(second.Content, "+two") || strings.Contains(second.Content, "next_hunk") {
		t.Fatalf("second page = %q", second.Content)
	}
	source.Flush()
	if got := source.DiffReads()["a.go"]; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("diff reads after second page = %v, want [1 2]", got)
	}
}

func TestReadDiffNeedsEveryLineOfALargeHunk(t *testing.T) {
	var lines []string
	lines = append(lines, "@@ -1 +1 @@")
	for i := 0; i < 500; i++ {
		lines = append(lines, "+line")
	}
	source := NewChangeSource([]ChangeFile{{Path: "big.go", Status: "modified", Diff: strings.Join(lines, "\n")}}, nil, nil)
	sb := changeSandbox(t)
	read := toolByName(t, BuiltinToolsWithChanges(sb, source), "read_diff")

	first, err := invoke(t, read, `{"path":"big.go","from_hunk":1,"max_hunks":1}`)
	if err != nil {
		t.Fatalf("read first page: %v", err)
	}
	if !strings.Contains(first.Content, "next_hunk: 1") || !strings.Contains(first.Content, "next_line:") {
		t.Fatalf("large hunk was not paginated by line: %q", first.Content)
	}
	if got := source.DiffReads()["big.go"]; len(got) != 0 {
		t.Fatalf("partial hunk was counted as read: %v", got)
	}
	var nextLine int
	for _, line := range strings.Split(first.Content, "\n") {
		if strings.HasPrefix(line, "[read_diff] next_hunk:") {
			var hunk int
			if _, err := fmt.Sscanf(line, "[read_diff] next_hunk: %d, next_line: %d", &hunk, &nextLine); err != nil {
				t.Fatalf("parse cursor %q: %v", line, err)
			}
		}
	}
	if nextLine < 2 || nextLine > 500 {
		t.Fatalf("next_line = %d, want a line inside the hunk", nextLine)
	}
	rest, err := invoke(t, read, `{"path":"big.go","from_hunk":1,"from_line":`+itoa(nextLine)+`}`)
	if err != nil {
		t.Fatalf("read second page: %v", err)
	}
	if strings.Contains(rest.Content, "next_hunk") {
		t.Fatalf("hunk still incomplete after reading the rest: %q", rest.Content)
	}
	source.Flush()
	if got := source.DiffReads()["big.go"]; len(got) != 1 || got[0] != 1 {
		t.Fatalf("diff reads after full hunk = %v, want [1]", got)
	}
}

func TestListChangesShowsInlineStateAndHunks(t *testing.T) {
	diff := "@@ -1 +1 @@\n+one\n@@ -10 +10 @@\n+two\n"
	source := NewChangeSource([]ChangeFile{
		{Path: "a.go", OldPath: "old.go", Status: "renamed", Diff: diff, Additions: 2},
		{Path: "b.go", Status: "added", Diff: "@@ -0 +1 @@\n+b\n"},
	}, nil, map[string]bool{"a.go": true, "b.go": false})
	sb := changeSandbox(t)
	list := toolByName(t, BuiltinToolsWithChanges(sb, source), "list_changes")
	result, err := invoke(t, list, `{}`)
	if err != nil {
		t.Fatalf("list changes: %v", err)
	}
	if !strings.Contains(result.Content, "old_path") || !strings.Contains(result.Content, "a.go\told.go\trenamed") || !strings.Contains(result.Content, "inlined") {
		t.Fatalf("list output = %q", result.Content)
	}
	for _, want := range []string{"a.go", "old.go", "yes", "b.go", "no"} {
		if !strings.Contains(result.Content, want) {
			t.Fatalf("list output missing %q: %q", want, result.Content)
		}
	}
}

func TestReadDiffRejectsUnknownPath(t *testing.T) {
	source := NewChangeSource(nil, nil, nil)
	sb := changeSandbox(t)
	read := toolByName(t, BuiltinToolsWithChanges(sb, source), "read_diff")
	if _, err := invoke(t, read, `{"path":"missing.go"}`); err == nil {
		t.Fatal("read_diff accepted a path outside the change snapshot")
	}
}

func itoa(value int) string {
	return strconv.Itoa(value)
}

func TestFlushDefersReadsUntilModelTurn(t *testing.T) {
	source := NewChangeSource([]ChangeFile{{Path: "a.go", Status: "modified", Diff: "@@ -1 +1 @@\n+one\n"}}, nil, nil)
	sb := changeSandbox(t)
	read := toolByName(t, BuiltinToolsWithChanges(sb, source), "read_diff")
	if _, err := invoke(t, read, `{"path":"a.go"}`); err != nil {
		t.Fatal(err)
	}
	if got := source.DiffReads(); len(got) != 0 {
		t.Fatalf("read_diff committed before the next model turn: %v", got)
	}
	source.Flush()
	if got := source.DiffReads()["a.go"]; len(got) != 1 || got[0] != 1 {
		t.Fatalf("flush did not commit the read: %v", got)
	}
	source.Reset()
	if got := source.DiffReads(); len(got) != 0 {
		t.Fatalf("reset kept reads: %v", got)
	}
}

func TestReadDiffPaginatesAnOversizedLine(t *testing.T) {
	long := "+" + strings.Repeat("x", maxDiffRunesPerCall*2)
	source := NewChangeSource([]ChangeFile{{Path: "long.go", Status: "modified", Diff: "@@ -1 +1 @@\n" + long + "\n"}}, nil, nil)
	sb := changeSandbox(t)
	read := toolByName(t, BuiltinToolsWithChanges(sb, source), "read_diff")

	fromLine, fromOffset := 1, 0
	for round := 0; round < 10; round++ {
		input := fmt.Sprintf(`{"path":"long.go","from_line":%d,"from_offset":%d}`, fromLine, fromOffset)
		result, err := invoke(t, read, input)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		if strings.Contains(result.Content, "end of diff") {
			source.Flush()
			if got := source.DiffReads()["long.go"]; len(got) != 1 || got[0] != 1 {
				t.Fatalf("oversized line was not recorded after all pages: %v", got)
			}
			return
		}
		nextLine, nextOffset := 0, 0
		for _, line := range strings.Split(result.Content, "\n") {
			if strings.HasPrefix(line, "[read_diff] next_hunk:") {
				var hunk int
				if _, err := fmt.Sscanf(line, "[read_diff] next_hunk: %d, next_line: %d, next_offset: %d", &hunk, &nextLine, &nextOffset); err != nil {
					t.Fatalf("parse cursor %q: %v", line, err)
				}
			}
		}
		if nextLine == 0 || (nextLine == fromLine && nextOffset <= fromOffset) {
			t.Fatalf("cursor did not advance: line=%d offset=%d from=%d/%d content=%q", nextLine, nextOffset, fromLine, fromOffset, result.Content)
		}
		fromLine, fromOffset = nextLine, nextOffset
	}
	t.Fatal("oversized line did not finish within 10 pages")
}

func TestDropEvidenceRemovesCompactedReads(t *testing.T) {
	diff := "@@ -1 +1 @@\n+one\n"
	source := NewChangeSource([]ChangeFile{{Path: "a.go", Status: "modified", Diff: diff}}, nil, nil)
	sb := changeSandbox(t)
	read := toolByName(t, BuiltinToolsWithChanges(sb, source), "read_diff")
	result, err := invoke(t, read, `{"path":"a.go"}`)
	if err != nil {
		t.Fatal(err)
	}
	if result.EvidenceID == "" {
		t.Fatal("read_diff did not return an evidence id")
	}
	source.DropEvidence([]string{result.EvidenceID})
	source.Flush()
	if got := source.DiffReads(); len(got) != 0 {
		t.Fatalf("dropped read still counted as reviewed: %v", got)
	}
}
