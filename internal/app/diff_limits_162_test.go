package app

// These tests invert the observed assertions in
// docs/audit-repro-4839248/162-app_diff_limits_test.go.txt. Git fixtures use
// t.TempDir instead of a HOME directory fixture.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
)

func limitDiff(oldPath, newPath, diff string) gitlab.FileDiff {
	return gitlab.FileDiff{OldPath: oldPath, NewPath: newPath, Diff: diff}
}

func renameDiff(oldPath, newPath, diff string) gitlab.FileDiff {
	return gitlab.FileDiff{OldPath: oldPath, NewPath: newPath, RenamedFile: true, Diff: diff}
}

func TestCountDiffLinesCountsHunkContentWithHeaderPrefixes(t *testing.T) {
	full := "diff --git a/old.txt b/new.txt\n" +
		"index 1111111..2222222 100644\n" +
		"--- a/old.txt\n" +
		"+++ b/new.txt\n" +
		"@@ -1,4 +1,4 @@\n" +
		" anchor\n" +
		"---before\n" +
		"-ordinary old\n" +
		"+++after\n" +
		"+ordinary new\n" +
		" finish\n"
	hunk := full[strings.Index(full, "@@ "):]
	for _, tc := range []struct {
		name string
		diff string
	}{
		{name: "full diff", diff: full},
		{name: "hunk only", diff: hunk},
	} {
		t.Run(tc.name, func(t *testing.T) {
			added, deleted := countDiffLines(tc.diff)
			if added != 2 || deleted != 2 {
				t.Fatalf("countDiffLines() = %d,%d, want 2,2:\n%s", added, deleted, tc.diff)
			}
		})
	}
	added, deleted := countDiffLines("--- a/old.txt\n+++ b/new.txt\n")
	if added != 0 || deleted != 0 {
		t.Fatalf("file headers counted as changes: %d,%d", added, deleted)
	}
}

func TestCountDiffLinesMultipleHunksAndNoNewlineMarker(t *testing.T) {
	diff := "@@ -1,1 +1,1 @@\n-old\n+new\n" +
		"@@ -10,0 +11,2 @@\n+one\n+two\n\\ No newline at end of file\n"
	added, deleted := countDiffLines(diff)
	if added != 3 || deleted != 1 {
		t.Fatalf("countDiffLines() = %d,%d, want 3,1", added, deleted)
	}
}

func TestCountDiffLinesCountsExcessAndRawContent(t *testing.T) {
	tests := []struct {
		name      string
		diff      string
		additions int
		deletions int
	}{
		{
			name:      "hunk shorter than content",
			diff:      "@@ -1,1 +1,1 @@\n-old\n+new\n+extra\n",
			additions: 2,
			deletions: 1,
		},
		{
			name:      "empty old hunk with excess additions",
			diff:      "@@ -0,0 +1 @@\n+one\n+two\n+three\n",
			additions: 3,
		},
		{
			name:      "overflowed hunk count",
			diff:      "@@ -0,0 +999999999999999999999 @@\n+one\n+two\n+three\n",
			additions: 3,
		},
		{
			name:      "malformed hunk header",
			diff:      "@@ bad @@\n+one\n+two\n+three\n",
			additions: 3,
		},
		{
			name:      "headerless additions",
			diff:      "+one\n+two\n+three\n",
			additions: 3,
		},
		{
			name:      "headerless mixed content",
			diff:      "-old\n+new\n-gone\n",
			additions: 1,
			deletions: 2,
		},
		{
			name:      "header-like content followed by more content",
			diff:      "--- a/one.go\n+++ b/one.go\n+one\n",
			additions: 2,
			deletions: 1,
		},
		{
			name:      "header-like hunk content with spaces",
			diff:      "@@ -1,1 +1,1 @@\n--- a\n+++ b\n",
			additions: 1,
			deletions: 1,
		},
		{
			name:      "exhausted hunk keeps header-like content",
			diff:      "@@ -1,1 +1,1 @@\n-old\n+new\n--- a\n+++ b\n",
			additions: 2,
			deletions: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			added, deleted := countDiffLines(tc.diff)
			if added != tc.additions || deleted != tc.deletions {
				t.Fatalf("countDiffLines() = %d,%d, want %d,%d:\n%s", added, deleted, tc.additions, tc.deletions, tc.diff)
			}
		})
	}
}

func TestBuildReviewFilesCountsHeaderLikeRawContentConservatively(t *testing.T) {
	diff := "--- a/gap.go\n+++ b/gap.go\n+one\n+two\n+three\n"
	files, _, summary, _ := buildReviewFiles([]gitlab.FileDiff{limitDiff("gap.go", "gap.go", diff)}, nil, 10, 4)
	if len(files) != 0 || !summary.Truncated || len(summary.Omitted) != 1 || summary.Omitted[0].Reason != "max_diff_lines" {
		t.Fatalf("limit 4 included header-like raw content: files=%+v summary=%+v", files, summary)
	}
	files, _, summary, warnings := buildReviewFiles([]gitlab.FileDiff{limitDiff("gap.go", "gap.go", diff)}, nil, 10, 5)
	if len(files) != 1 || summary.Additions != 4 || summary.Deletions != 1 || summary.Truncated || len(warnings) != 0 {
		t.Fatalf("limit 5 = files=%+v summary=%+v warnings=%v", files, summary, warnings)
	}
}

func TestCountDiffLinesIgnoresFileHeadersBetweenHunks(t *testing.T) {
	diff := "diff --git a/one.go b/one.go\n" +
		"--- a/one.go\n+++ b/one.go\n" +
		"@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/two.go b/two.go\n" +
		"--- a/two.go\n+++ b/two.go\n" +
		"@@ -1 +1 @@\n-old2\n+new2\n"
	added, deleted := countDiffLines(diff)
	if added != 2 || deleted != 2 {
		t.Fatalf("countDiffLines() = %d,%d, want 2,2:\n%s", added, deleted, diff)
	}
}

func TestBuildReviewFilesMaxDiffLinesCannotBeBypassedByParsingGaps(t *testing.T) {
	tests := []struct {
		name      string
		diff      string
		additions int
		deletions int
	}{
		{
			name:      "hunk shorter than content",
			diff:      "@@ -1,1 +1,1 @@\n-old\n+new\n+extra\n",
			additions: 2,
			deletions: 1,
		},
		{
			name:      "empty old hunk with excess additions",
			diff:      "@@ -0,0 +1 @@\n+one\n+two\n+three\n",
			additions: 3,
		},
		{
			name:      "overflowed hunk count",
			diff:      "@@ -0,0 +999999999999999999999 @@\n+one\n+two\n+three\n",
			additions: 3,
		},
		{
			name:      "malformed hunk header",
			diff:      "@@ bad @@\n+one\n+two\n+three\n",
			additions: 3,
		},
		{
			name:      "headerless additions",
			diff:      "+one\n+two\n+three\n",
			additions: 3,
		},
		{
			name:      "headerless mixed content",
			diff:      "-old\n+new\n-gone\n",
			additions: 1,
			deletions: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files, _, summary, warnings := buildReviewFiles([]gitlab.FileDiff{limitDiff("gap.go", "gap.go", tc.diff)}, nil, 10, 2)
			if len(files) != 0 || !summary.Truncated || len(summary.Omitted) != 1 ||
				summary.Omitted[0].Path != "gap.go" || summary.Omitted[0].Reason != "max_diff_lines" ||
				len(warnings) != 1 || !strings.Contains(warnings[0], "max_diff_lines") {
				t.Fatalf("limit 2 was bypassed: files=%+v summary=%+v warnings=%v", files, summary, warnings)
			}
			files, _, summary, warnings = buildReviewFiles([]gitlab.FileDiff{limitDiff("gap.go", "gap.go", tc.diff)}, nil, 10, 3)
			if len(files) != 1 || len(warnings) != 0 || summary.Truncated ||
				summary.Additions != tc.additions || summary.Deletions != tc.deletions {
				t.Fatalf("exact limit = files=%+v summary=%+v warnings=%v", files, summary, warnings)
			}
		})
	}
}

func TestBuildReviewFilesAppliesMaxDiffLinesToFirstFile(t *testing.T) {
	large := limitDiff("large.go", "large.go", "@@ -0,0 +1,3 @@\n+one\n+two\n+three\n")
	files, _, summary, warnings := buildReviewFiles([]gitlab.FileDiff{large}, nil, 10, 2)
	if len(files) != 0 || !summary.Truncated || summary.Files != 0 ||
		len(summary.Omitted) != 1 || summary.Omitted[0].Path != "large.go" ||
		summary.Omitted[0].Reason != "max_diff_lines" {
		t.Fatalf("first oversized file was included: files=%+v summary=%+v warnings=%v", files, summary, warnings)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "max_diff_lines") {
		t.Fatalf("warnings = %v, want a max_diff_lines warning", warnings)
	}

	files, _, summary, warnings = buildReviewFiles([]gitlab.FileDiff{large}, nil, 10, 3)
	if len(files) != 1 || summary.Files != 1 || summary.Additions != 3 ||
		summary.Truncated || len(summary.Omitted) != 0 || len(warnings) != 0 {
		t.Fatalf("exact limit was rejected: files=%+v summary=%+v warnings=%v", files, summary, warnings)
	}
	for _, maxLines := range []int{0, -1} {
		files, _, summary, warnings = buildReviewFiles([]gitlab.FileDiff{large}, nil, 10, maxLines)
		if len(files) != 1 || summary.Truncated || len(warnings) != 0 {
			t.Fatalf("maxLines=%d limited the first file: files=%+v summary=%+v warnings=%v", maxLines, files, summary, warnings)
		}
	}
}

func TestBuildReviewFilesAppliesMaxDiffLinesToNewAndDeletedLines(t *testing.T) {
	diff := limitDiff("change.go", "change.go", "@@ -1,2 +1,2 @@\n-old one\n-old two\n+new one\n+new two\n")
	tests := []struct {
		name     string
		maxLines int
		want     int
	}{
		{name: "exact", maxLines: 4, want: 1},
		{name: "over", maxLines: 3, want: 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files, _, summary, _ := buildReviewFiles([]gitlab.FileDiff{diff}, nil, 10, tc.maxLines)
			if len(files) != tc.want {
				t.Fatalf("files = %+v, want %d", files, tc.want)
			}
			if tc.want == 1 && (summary.Additions != 2 || summary.Deletions != 2) {
				t.Fatalf("summary = %+v, want 2 additions and 2 deletions", summary)
			}
		})
	}
}

func TestBuildReviewFilesKeepsSmallFileAfterOversizedFirstFile(t *testing.T) {
	large := limitDiff("large.go", "large.go", "@@ -0,0 +1,3 @@\n+one\n+two\n+three\n")
	small := limitDiff("small.go", "small.go", "@@ -0,0 +1 @@\n+one\n")
	files, _, summary, warnings := buildReviewFiles([]gitlab.FileDiff{large, small}, nil, 10, 2)
	if len(files) != 1 || files[0].Path != "small.go" || summary.Additions != 1 ||
		!summary.Truncated || len(summary.Omitted) != 1 ||
		summary.Omitted[0].Path != "large.go" || summary.Omitted[0].Reason != "max_diff_lines" {
		t.Fatalf("files=%+v summary=%+v warnings=%v", files, summary, warnings)
	}
}

func TestBuildReviewFilesCountsHeaderPrefixedHunkContent(t *testing.T) {
	diff := gitDiffContentPrefixes(t)
	files, _, summary, warnings := buildReviewFiles([]gitlab.FileDiff{
		{OldPath: "old.txt", NewPath: "new.txt", Diff: diff},
	}, nil, 10, 4)
	if len(files) != 1 || len(warnings) != 0 || summary.Truncated {
		t.Fatalf("files=%+v summary=%+v warnings=%v", files, summary, warnings)
	}
	if files[0].Additions != 2 || files[0].Deletions != 2 || summary.Additions != 2 || summary.Deletions != 2 {
		t.Fatalf("file=%+v summary=%+v, want 2 additions and 2 deletions", files[0], summary)
	}
	files, _, summary, _ = buildReviewFiles([]gitlab.FileDiff{
		{OldPath: "old.txt", NewPath: "new.txt", Diff: diff},
	}, nil, 10, 3)
	if len(files) != 0 || !summary.Truncated || len(summary.Omitted) != 1 || summary.Omitted[0].Reason != "max_diff_lines" {
		t.Fatalf("limit 3 included the diff: files=%+v summary=%+v", files, summary)
	}
	headerOnly := limitDiff("first.txt", "first.txt", "--- a/first.txt\n+++ b/first.txt\n")
	files, _, summary, warnings = buildReviewFiles([]gitlab.FileDiff{headerOnly}, nil, 10, 1)
	if len(files) != 1 || summary.Additions != 0 || summary.Deletions != 0 || summary.Truncated || len(warnings) != 0 {
		t.Fatalf("header-only diff = files=%+v summary=%+v warnings=%v", files, summary, warnings)
	}
}

func gitDiffContentPrefixes(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("anchor\n--before\nordinary old\nfinish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "new.txt"), []byte("anchor\n++after\nordinary new\nfinish\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("git", "-c", "color.ui=false", "diff", "--no-index", "--no-ext-diff", "--no-textconv", "--", "old.txt", "new.txt")
	cmd.Dir = dir
	env := make([]string, 0, len(os.Environ())+3)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = append(env,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL="+os.DevNull,
		"GIT_TERMINAL_PROMPT=0",
	)
	out, err := cmd.CombinedOutput()
	if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 1 {
		t.Fatalf("git diff: %v\n%s", err, out)
	}
	diff := string(out)
	for _, want := range []string{"\n--- a/old.txt\n", "\n+++ b/new.txt\n", "\n---before\n", "\n+++after\n"} {
		if !strings.Contains(diff, want) {
			t.Fatalf("git diff missing %q:\n%s", want, diff)
		}
	}
	return diff
}

func TestRenameChainThroughDossierUsesNewSideTarget(t *testing.T) {
	version := gitlab.Version{
		ID:       7,
		BaseSHA:  strings.Repeat("1", 40),
		StartSHA: strings.Repeat("2", 40),
		HeadSHA:  strings.Repeat("3", 40),
	}
	diffs := []gitlab.FileDiff{
		renameDiff("b.go", "c.go", "@@ -1 +1 @@\n-b old\n+c new\n"),
		renameDiff("a.go", "b.go", "@@ -1 +1 @@\n-a old\n+b new\n"),
	}
	files, ignored, summary, warnings := buildReviewFiles(diffs, nil, 10, 100)
	if len(files) != 2 || len(ignored) != 0 || summary.Truncated || len(warnings) != 0 {
		t.Fatalf("files=%+v ignored=%v summary=%+v warnings=%v", files, ignored, summary, warnings)
	}
	dossier, err := review.BuildDossier(review.DossierInput{
		Project:     review.ProjectRef{ID: 42, Path: "g/p"},
		MR:          review.MRMeta{IID: 7, Title: "rename chain", SourceBranch: "feature", TargetBranch: "main"},
		DiffVersion: version,
		DiffSummary: summary,
		Files:       files,
	})
	if err != nil {
		t.Fatalf("BuildDossier: %v", err)
	}
	index := gitlab.BuildDiffIndex(dossier.DiffVersion, dossierDiffs(dossier))
	got, ok := index.Locate("b.go", 1, gitlab.SideNew)
	want := gitlab.Position{
		BaseSHA: version.BaseSHA, StartSHA: version.StartSHA, HeadSHA: version.HeadSHA,
		OldPath: "a.go", NewPath: "b.go", NewLine: 1,
	}
	if !ok || got != want {
		t.Fatalf("new-side b.go = %+v, %v; want %+v", got, ok, want)
	}
	if got, ok := index.Locate("b.go", 1, gitlab.SideOld); !ok || got.OldPath != "b.go" || got.NewPath != "c.go" || got.OldLine != 1 {
		t.Fatalf("old-side b.go = %+v, %v", got, ok)
	}
	if !locateFunc(index)("b.go", 1, review.SideNew) {
		t.Fatal("executor locator rejected the new-side target")
	}
	groups := markPositions([]review.Group{{Path: "b.go", Line: 1, Side: review.SideNew}}, index)
	if len(groups) != 1 || !groups[0].PositionOK {
		t.Fatalf("publisher rejected the new-side target: %+v", groups)
	}
}

func TestCountDiffLinesCountsTrailingHeaderLikeContent(t *testing.T) {
	tests := []struct {
		name      string
		diff      string
		additions int
		deletions int
	}{
		{
			name:      "after malformed hunk",
			diff:      "@@ bad @@\n--- before\n+++ after\n",
			additions: 1,
			deletions: 1,
		},
		{
			name:      "after raw addition",
			diff:      "+one\n--- before\n+++ after\n",
			additions: 2,
			deletions: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			added, deleted := countDiffLines(tc.diff)
			if added != tc.additions || deleted != tc.deletions {
				t.Fatalf("countDiffLines() = %d,%d, want %d,%d:\n%s", added, deleted, tc.additions, tc.deletions, tc.diff)
			}
		})
	}
}

func TestBuildReviewFilesCountsTrailingHeaderLikeContent(t *testing.T) {
	tests := []struct {
		name      string
		diff      string
		additions int
		deletions int
	}{
		{
			name:      "after malformed hunk",
			diff:      "@@ bad @@\n--- before\n+++ after\n",
			additions: 1,
			deletions: 1,
		},
		{
			name:      "after raw addition",
			diff:      "+one\n--- before\n+++ after\n",
			additions: 2,
			deletions: 1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			total := tc.additions + tc.deletions
			files, _, summary, warnings := buildReviewFiles([]gitlab.FileDiff{limitDiff("gap.go", "gap.go", tc.diff)}, nil, 10, total-1)
			if len(files) != 0 || !summary.Truncated || len(summary.Omitted) != 1 || summary.Omitted[0].Reason != "max_diff_lines" || len(warnings) != 1 {
				t.Fatalf("limit %d was bypassed: files=%+v summary=%+v warnings=%v", total-1, files, summary, warnings)
			}
			files, _, summary, warnings = buildReviewFiles([]gitlab.FileDiff{limitDiff("gap.go", "gap.go", tc.diff)}, nil, 10, total)
			if len(files) != 1 || summary.Additions != tc.additions || summary.Deletions != tc.deletions || summary.Truncated || len(warnings) != 0 {
				t.Fatalf("limit %d = files=%+v summary=%+v warnings=%v", total, files, summary, warnings)
			}
		})
	}
}

func TestFilteredDossierCannotRouteRenameChainAcrossSides(t *testing.T) {
	version := gitlab.Version{
		ID:       7,
		BaseSHA:  strings.Repeat("1", 40),
		StartSHA: strings.Repeat("2", 40),
		HeadSHA:  strings.Repeat("3", 40),
	}
	first := renameDiff("b.go", "c.go", "@@ -1 +1 @@\n-b old\n+c new\n")
	second := renameDiff("a.go", "b.go", "@@ -1 +1 @@\n-a old\n+b new\n")
	oversized := renameDiff("a.go", "b.go", "@@ -0,0 +1,3 @@\n+one\n+two\n+three\n")
	oversizedOld := renameDiff("b.go", "c.go", "@@ -0,0 +1,3 @@\n+one\n+two\n+three\n")
	collapsedFirst := first
	collapsedFirst.Collapsed = true
	collapsedSecond := second
	collapsedSecond.Collapsed = true
	largeFirst := first
	largeFirst.TooLarge = true
	largeSecond := second
	largeSecond.TooLarge = true
	tests := []struct {
		name      string
		diffs     []gitlab.FileDiff
		ignores   []string
		maxFiles  int
		maxLines  int
		queryPath string
		querySide gitlab.Side
		otherSide gitlab.Side
	}{
		{name: "new side collapsed canonical", diffs: []gitlab.FileDiff{first, collapsedSecond}, maxFiles: 10, maxLines: 100, queryPath: "b.go", querySide: gitlab.SideNew, otherSide: gitlab.SideOld},
		{name: "new side too large canonical", diffs: []gitlab.FileDiff{first, largeSecond}, maxFiles: 10, maxLines: 100, queryPath: "b.go", querySide: gitlab.SideNew, otherSide: gitlab.SideOld},
		{name: "new side ignored canonical", diffs: []gitlab.FileDiff{first, second}, ignores: []string{"a.go"}, maxFiles: 10, maxLines: 100, queryPath: "b.go", querySide: gitlab.SideNew, otherSide: gitlab.SideOld},
		{name: "new side max files canonical", diffs: []gitlab.FileDiff{first, second}, maxFiles: 1, maxLines: 100, queryPath: "b.go", querySide: gitlab.SideNew, otherSide: gitlab.SideOld},
		{name: "new side max diff lines canonical", diffs: []gitlab.FileDiff{first, oversized}, maxFiles: 10, maxLines: 3, queryPath: "b.go", querySide: gitlab.SideNew, otherSide: gitlab.SideOld},
		{name: "old side collapsed canonical", diffs: []gitlab.FileDiff{second, collapsedFirst}, maxFiles: 10, maxLines: 100, queryPath: "b.go", querySide: gitlab.SideOld, otherSide: gitlab.SideNew},
		{name: "old side too large canonical", diffs: []gitlab.FileDiff{second, largeFirst}, maxFiles: 10, maxLines: 100, queryPath: "b.go", querySide: gitlab.SideOld, otherSide: gitlab.SideNew},
		{name: "old side ignored canonical", diffs: []gitlab.FileDiff{second, first}, ignores: []string{"c.go"}, maxFiles: 10, maxLines: 100, queryPath: "b.go", querySide: gitlab.SideOld, otherSide: gitlab.SideNew},
		{name: "old side max files canonical", diffs: []gitlab.FileDiff{second, first}, maxFiles: 1, maxLines: 100, queryPath: "b.go", querySide: gitlab.SideOld, otherSide: gitlab.SideNew},
		{name: "old side max diff lines canonical", diffs: []gitlab.FileDiff{second, oversizedOld}, maxFiles: 10, maxLines: 3, queryPath: "b.go", querySide: gitlab.SideOld, otherSide: gitlab.SideNew},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			files, ignored, summary, warnings := buildReviewFiles(tc.diffs, tc.ignores, tc.maxFiles, tc.maxLines)
			if len(files) != 1 {
				t.Fatalf("expected one included file, got files=%+v ignored=%v summary=%+v warnings=%v", files, ignored, summary, warnings)
			}
			dossier, err := review.BuildDossier(review.DossierInput{
				Project:     review.ProjectRef{ID: 42, Path: "g/p"},
				MR:          review.MRMeta{IID: 7, Title: "filtered rename chain", SourceBranch: "feature", TargetBranch: "main"},
				DiffVersion: version,
				DiffSummary: summary,
				Files:       files,
				Ignored:     ignored,
			})
			if err != nil {
				t.Fatalf("BuildDossier: %v", err)
			}
			if got := dossierDiffs(dossier); len(got) != 1 {
				t.Fatalf("dossier diffs = %+v, want one included diff", got)
			}
			index := gitlab.BuildDiffIndex(dossier.DiffVersion, dossierDiffs(dossier))
			if pos, ok := index.Locate(tc.queryPath, 1, tc.querySide); ok {
				t.Fatalf("omitted canonical path answered from a different file: %+v", pos)
			}
			groups := markPositions([]review.Group{{Path: tc.queryPath, Line: 1, Side: review.Side(tc.querySide)}}, index)
			if len(groups) != 1 || groups[0].PositionOK {
				t.Fatalf("publisher accepted a position for the omitted path: %+v", groups)
			}
			if pos, ok := index.Locate(tc.queryPath, 1, tc.otherSide); !ok {
				t.Fatalf("included canonical path side %s was not indexed: %+v", tc.otherSide, pos)
			}
		})
	}
}
