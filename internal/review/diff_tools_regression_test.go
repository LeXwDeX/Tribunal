package review

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
)

func TestReviewTaskIncludesCompleteLargeDiffWithinBudget(t *testing.T) {
	diff := "@@ -1 +1 @@\n-old\n+" + strings.Repeat("界", 17000) + "\n+END-OF-DIFF\n"
	dossier := &Dossier{Files: []FileChange{{Path: "large.go", Status: "modified", Diff: diff}}}
	layout, ok := reviewTaskLayoutFor(KindCodeReview, dossier, reviewNodeParams{Files: []string{"large.go"}})
	if !ok {
		t.Fatal("review task layout unavailable")
	}
	if !strings.Contains(layout.task, "END-OF-DIFF") {
		t.Fatal("diff that fits the package budget was truncated before its end")
	}
}

func TestReviewTaskIndexesFilesBeyondInlineBudget(t *testing.T) {
	dossier := &Dossier{Files: []FileChange{
		{Path: "first.go", Status: "modified", Diff: "@@ -1 +1 @@\n+" + strings.Repeat("x", MaxTaskRunes+1000) + "\n"},
		{Path: "second.go", Status: "modified", Diff: "@@ -1 +1 @@\n+second\n"},
	}}
	layout, ok := reviewTaskLayoutFor(KindCodeReview, dossier, reviewNodeParams{Files: []string{"first.go", "second.go"}})
	if !ok {
		t.Fatal("review task layout unavailable")
	}
	if !strings.Contains(layout.task, "first.go") || !strings.Contains(layout.task, "second.go") {
		t.Fatal("change index omitted a file beyond the inline budget")
	}
	if !strings.Contains(layout.task, "read_diff") {
		t.Fatal("change index does not direct the agent to read omitted diffs")
	}
}

func TestCoverageAcceptsCompleteDiffReadsForOmittedFile(t *testing.T) {
	const fileCount = 20
	diff := "@@ -1 +1 @@\n+" + strings.Repeat("x", maxFileDiffRunes)
	files := make([]string, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		files = append(files, fmt.Sprintf("f%02d.go", i))
	}
	d := promptBudgetDossier(t, "short description", diff, files...)
	params := promptBudgetParams(KindCodeReview, files, "focus")
	layout, ok := reviewTaskLayoutFor(KindCodeReview, d, params)
	if !ok {
		t.Fatal("no task layout")
	}
	omitted := ""
	for _, file := range files {
		if !layout.inline[file] {
			omitted = file
			break
		}
	}
	if omitted == "" {
		t.Fatal("test setup inlined every file")
	}
	reads := make(map[string][]int)
	for _, file := range files {
		if !layout.inline[file] {
			reads[file] = []int{1}
		}
	}
	executor := newTestExecutor(t, ExecutorDeps{Dossier: d})
	output := mustJSON(t, map[string]any{"diff_reads": reads})
	gaps, _ := executor.builtPromptDiffGaps(d, KindCodeReview, params, nil, output)
	if len(gaps) != 0 {
		t.Fatalf("complete read_diff evidence left a coverage gap: %v", gaps)
	}
}

func TestCoverageReportsUnreadHunksForOmittedFile(t *testing.T) {
	diff := "@@ -1 +1 @@\n+one\n@@ -10 +10 @@\n+" + strings.Repeat("x", MaxTaskRunes+1000)
	d := promptBudgetDossier(t, "short description", diff, "a.go")
	params := promptBudgetParams(KindCodeReview, []string{"a.go"}, "focus")
	executor := newTestExecutor(t, ExecutorDeps{Dossier: d})

	partial := mustJSON(t, map[string]any{"diff_reads": map[string][]int{"a.go": {1}}})
	gaps, _ := executor.builtPromptDiffGaps(d, KindCodeReview, params, nil, partial)
	if len(gaps) == 0 || !strings.Contains(strings.Join(gaps, "\n"), "unread hunks") || !strings.Contains(strings.Join(gaps, "\n"), "2") {
		t.Fatalf("partial read evidence = %v, want the unread hunk named", gaps)
	}

	complete := mustJSON(t, map[string]any{"diff_reads": map[string][]int{"a.go": {1, 2}}})
	gaps, _ = executor.builtPromptDiffGaps(d, KindCodeReview, params, nil, complete)
	if len(gaps) != 0 {
		t.Fatalf("complete read evidence left a coverage gap: %v", gaps)
	}
}

func TestReviewTaskKeepsSixtyFileDiffIndexed(t *testing.T) {
	const fileCount = 60
	files := make([]string, 0, fileCount)
	changes := make([]FileChange, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("pkg/file%02d.go", i)
		files = append(files, name)
		changes = append(changes, FileChange{
			Path: name, Status: "modified", Additions: 2, Deletions: 1,
			Diff: "@@ -1,2 +1,2 @@\n-old\n+new\n context\n",
		})
	}
	d := &Dossier{Files: changes}
	params := promptBudgetParams(KindCodeReview, files, "focus")
	layout, ok := reviewTaskLayoutFor(KindCodeReview, d, params)
	if !ok {
		t.Fatal("no task layout")
	}
	for _, name := range files {
		if !strings.Contains(layout.task, name) {
			t.Fatalf("change index omitted %s", name)
		}
		if !layout.inline[name] {
			t.Fatalf("%s was not inlined despite the small diff", name)
		}
	}
	executor := newTestExecutor(t, ExecutorDeps{Dossier: d})
	gaps, _ := executor.builtPromptDiffGaps(d, KindCodeReview, params, nil, nil)
	if len(gaps) != 0 {
		t.Fatalf("complete 60-file package produced coverage gaps: %v", gaps)
	}
}

func TestCoverageClearsAfterRealReadDiffCalls(t *testing.T) {
	const fileCount = 20
	diff := "@@ -1 +1 @@\n+" + strings.Repeat("x", 9000)
	files := make([]string, 0, fileCount)
	changes := make([]agent.ChangeFile, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		name := fmt.Sprintf("real%02d.go", i)
		files = append(files, name)
		changes = append(changes, agent.ChangeFile{Path: name, Status: "modified", Diff: diff, Additions: 1})
	}
	d := promptBudgetDossier(t, "short description", diff, files...)
	params := promptBudgetParams(KindCodeReview, files, "focus")
	layout, ok := reviewTaskLayoutFor(KindCodeReview, d, params)
	if !ok {
		t.Fatal("no task layout")
	}
	source := agent.NewChangeSource(changes, changes, layout.inline)
	sb, err := agent.NewSandbox(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	tools := agent.BuiltinToolsWithChanges(sb, source)
	var read agent.Tool
	for _, tool := range tools {
		if tool.Def().Name == "read_diff" {
			read = tool
		}
	}
	if read == nil {
		t.Fatal("read_diff is missing")
	}
	for _, file := range files {
		if layout.inline[file] {
			continue
		}
		input := fmt.Sprintf(`{"path":%q,"from_hunk":1}`, file)
		if _, err := read.Call(context.Background(), json.RawMessage(input)); err != nil {
			t.Fatalf("read_diff %s: %v", file, err)
		}
	}
	source.Flush()
	output := mustJSON(t, map[string]any{"diff_reads": source.DiffReads()})
	executor := newTestExecutor(t, ExecutorDeps{Dossier: d})
	gaps, _ := executor.builtPromptDiffGaps(d, KindCodeReview, params, nil, output)
	if len(gaps) != 0 {
		t.Fatalf("real read_diff evidence left coverage gaps: %v", gaps)
	}
}
