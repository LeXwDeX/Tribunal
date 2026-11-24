package app

// Tests for diff recovery: rebuilding a bounded unified diff from raw
// repository bodies GitLab withheld as too_large. Hunk line numbers must be
// exact because the recovered diff feeds gitlab.BuildDiffIndex for inline
// comments.

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
)

const (
	recoverHeadSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recoverBaseSHA  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	recoverStartSHA = "cccccccccccccccccccccccccccccccccccccccc"
)

func recoverVersion() gitlab.Version {
	return gitlab.Version{BaseSHA: recoverBaseSHA, StartSHA: recoverStartSHA, HeadSHA: recoverHeadSHA}
}

// numberedLines returns n lines of roughly width bytes each, so a file of a
// given size can be built without hand-writing content.
func numberedLines(n, width int) []string {
	lines := make([]string, n)
	for i := range lines {
		body := fmt.Sprintf("%08d", i)
		for len(body) < width {
			body += "x"
		}
		lines[i] = body
	}
	return lines
}

func joinLines(lines []string) []byte {
	return []byte(strings.Join(lines, "\n") + "\n")
}

// indexForDiff builds a DiffIndex for a single recovered diff so line numbers
// can be checked the way inline comments resolve them.
func indexForDiff(t *testing.T, path, diff string) *gitlab.DiffIndex {
	t.Helper()
	return gitlab.BuildDiffIndex(recoverVersion(), []gitlab.FileDiff{{OldPath: path, NewPath: path, Diff: diff}})
}

// A 600KB text file with a single edited line recovers to a tiny bounded diff
// with correct additions/deletions, and the change resolves to the right line.
func TestBuildRecoveredDiffSingleLargeFileOneChange(t *testing.T) {
	lines := numberedLines(7201, 80)
	change := 3600
	oldLines := append([]string(nil), lines...)
	newLines := append([]string(nil), lines...)
	newLines[change] = "changed line at " + fmt.Sprint(change)

	diff, ok, resource := buildRecoveredDiff(joinLines(oldLines), joinLines(newLines))
	if !ok || resource {
		t.Fatalf("ok=%v resource=%v, want a recovered diff", ok, resource)
	}
	if len(diff) > 4096 {
		t.Fatalf("diff is %d bytes, want a small bounded diff", len(diff))
	}
	added, deleted := countDiffLines(diff)
	if added != 1 || deleted != 1 {
		t.Fatalf("additions/deletions = %d/%d, want 1/1:\n%s", added, deleted, diff)
	}
	if strings.Count(diff, "@@ -") != 1 {
		t.Fatalf("want one hunk:\n%s", diff)
	}

	index := indexForDiff(t, "big.txt", diff)
	if pos, ok := index.Locate("big.txt", change+1, gitlab.SideNew); !ok || pos.NewLine != change+1 {
		t.Fatalf("new-side line = %+v, %v; want NewLine %d", pos, ok, change+1)
	}
	if pos, ok := index.Locate("big.txt", change+1, gitlab.SideOld); !ok || pos.OldLine != change+1 {
		t.Fatalf("old-side line = %+v, %v; want OldLine %d", pos, ok, change+1)
	}
}

// A large file with several scattered changes renders multiple hunks whose
// line numbers are exact, so inline comments land on the right lines.
func TestBuildRecoveredDiffMultipleScatteredChanges(t *testing.T) {
	lines := numberedLines(300, 40)
	changes := []int{10, 150, 289}
	oldLines := append([]string(nil), lines...)
	newLines := append([]string(nil), lines...)
	for _, c := range changes {
		newLines[c] = fmt.Sprintf("CHANGED-%d", c)
	}

	diff, ok, resource := buildRecoveredDiff(joinLines(oldLines), joinLines(newLines))
	if !ok || resource {
		t.Fatalf("ok=%v resource=%v, want a recovered diff", ok, resource)
	}
	if got := strings.Count(diff, "@@ -"); got != len(changes) {
		t.Fatalf("hunks = %d, want %d:\n%s", got, len(changes), diff)
	}
	added, deleted := countDiffLines(diff)
	if added != len(changes) || deleted != len(changes) {
		t.Fatalf("additions/deletions = %d/%d, want %d/%d:\n%s", added, deleted, len(changes), len(changes), diff)
	}

	index := indexForDiff(t, "big.txt", diff)
	for _, c := range changes {
		if pos, ok := index.Locate("big.txt", c+1, gitlab.SideNew); !ok || pos.NewLine != c+1 {
			t.Fatalf("new-side line %d = %+v, %v", c+1, pos, ok)
		}
		if pos, ok := index.Locate("big.txt", c+1, gitlab.SideOld); !ok || pos.OldLine != c+1 {
			t.Fatalf("old-side line %d = %+v, %v", c+1, pos, ok)
		}
	}
}

// Added and deleted files collapse to the canonical zero-side hunk header.
func TestBuildRecoveredDiffAddedAndDeletedHunkHeaders(t *testing.T) {
	added, ok, resource := buildRecoveredDiff(nil, []byte("one\ntwo\n"))
	if !ok || resource || !strings.HasPrefix(added, "@@ -0,0 +1,2 @@\n") {
		t.Fatalf("added = %q, ok=%v resource=%v, want @@ -0,0 +1,2 @@", added, ok, resource)
	}
	deleted, ok, resource := buildRecoveredDiff([]byte("one\ntwo\n"), nil)
	if !ok || resource || !strings.HasPrefix(deleted, "@@ -1,2 +0,0 @@\n") {
		t.Fatalf("deleted = %q, ok=%v resource=%v, want @@ -1,2 +0,0 @@", deleted, ok, resource)
	}
}

// A body above the recovery bound is an honest omission, never a partial diff.
func TestBuildRecoveredDiffOversizedBodyIsOmitted(t *testing.T) {
	big := append([]byte("head\n"), make([]byte, maxRecoveredRawFileBytes)...)
	diff, ok, resource := buildRecoveredDiff([]byte("head\n"), big)
	if ok || resource || diff != "" {
		t.Fatalf("oversized body diff=%q ok=%v resource=%v, want an omission", diff, ok, resource)
	}
}

// A multi-megabyte single-line base64 body is a non-diffable resource, not a
// too_large omission.
func TestBuildRecoveredDiffSingleLineResource(t *testing.T) {
	blob := []byte(strings.Repeat("A", 3<<20))
	diff, ok, resource := buildRecoveredDiff([]byte("small\n"), blob)
	if ok || !resource || diff != "" {
		t.Fatalf("resource diff=%q ok=%v resource=%v, want resource=true", diff, ok, resource)
	}
}

func TestBuildRecoveredDiffBinaryAndInvalidUTF8(t *testing.T) {
	if _, ok, resource := buildRecoveredDiff([]byte("a\x00b\n"), []byte("a\n")); ok || resource {
		t.Fatal("NUL body was recovered")
	}
	if _, ok, resource := buildRecoveredDiff([]byte("a\n"), []byte("a\xffb\n")); ok || resource {
		t.Fatal("invalid UTF-8 body was recovered")
	}
}

// The app-level recovery caps the number of attempts so a stream of withheld
// files cannot trigger one request each past the bound.
func TestApplyDiffScopeCapsRecoveryAttempts(t *testing.T) {
	const files = maxRecoveredFiles + 3
	fake := &rawFileGitLab{reviewGitLab: newReviewGitLab(recoverHeadSHA)}
	service := newScopeService(fake)
	diffs := make([]gitlab.FileDiff, files)
	for i := range diffs {
		path := fmt.Sprintf("new-%d.css", i)
		diffs[i] = gitlab.FileDiff{NewPath: path, NewFile: true, TooLarge: true}
		fake.bodies = ensureBodies(fake.bodies)
		setBody(fake, path, recoverHeadSHA, "one\ntwo\n")
	}
	version := recoverVersion()
	version.State, version.RealSize = "collected", fmt.Sprint(files)
	if err := service.applyDiffScope(context.Background(), 42, "token", &version, fmt.Sprint(files), diffs); err != nil {
		t.Fatalf("applyDiffScope: %v", err)
	}
	if fake.calls != maxRecoveredFiles {
		t.Fatalf("recovery attempts = %d, want %d", fake.calls, maxRecoveredFiles)
	}
	if !diffs[maxRecoveredFiles].TooLarge || diffs[maxRecoveredFiles].Undiffable {
		t.Fatalf("file past the attempt cap was processed: %+v", diffs[maxRecoveredFiles])
	}
	if diffs[0].TooLarge || strings.TrimSpace(diffs[0].Diff) == "" {
		t.Fatalf("first file was not recovered: %+v", diffs[0])
	}
}

// The app-level recovery caps total fetched bytes, so an unending run of large
// withheld bodies cannot be read in full.
func TestApplyDiffScopeCapsFetchedBytes(t *testing.T) {
	// Each added file yields one 4MB head body; the 32MB fetch bound stops the
	// loop after eight files, before the ninth is read.
	bigLine := strings.Repeat("x", 4<<20)
	const files = 9
	fake := &rawFileGitLab{reviewGitLab: newReviewGitLab(recoverHeadSHA)}
	service := newScopeService(fake)
	diffs := make([]gitlab.FileDiff, files)
	for i := range diffs {
		path := fmt.Sprintf("blob-%d.css", i)
		diffs[i] = gitlab.FileDiff{NewPath: path, NewFile: true, TooLarge: true}
		fake.bodies = ensureBodies(fake.bodies)
		setBody(fake, path, recoverHeadSHA, bigLine)
	}
	version := recoverVersion()
	version.State, version.RealSize = "collected", fmt.Sprint(files)
	if err := service.applyDiffScope(context.Background(), 42, "token", &version, fmt.Sprint(files), diffs); err != nil {
		t.Fatalf("applyDiffScope: %v", err)
	}
	if fake.calls >= files {
		t.Fatalf("fetched every file (%d), want the fetch bound to stop earlier", fake.calls)
	}
	last := diffs[files-1]
	if !last.TooLarge || last.Undiffable {
		t.Fatalf("file past the fetch bound was processed: %+v", last)
	}
}

// A withheld resource is recorded as undiffable_resource, not too_large, so the
// omission reason is honest.
func TestApplyDiffScopeMarksUndiffableResource(t *testing.T) {
	bigLine := strings.Repeat("A", 3<<20)
	fake := &rawFileGitLab{reviewGitLab: newReviewGitLab(recoverHeadSHA)}
	service := newScopeService(fake)
	fake.bodies = ensureBodies(fake.bodies)
	setBody(fake, "banner.svg", recoverHeadSHA, bigLine)
	setBody(fake, "banner.svg", recoverStartSHA, "small\n")
	diffs := []gitlab.FileDiff{{OldPath: "banner.svg", NewPath: "banner.svg", TooLarge: true}}
	version := recoverVersion()
	version.State, version.RealSize = "collected", "1"
	if err := service.applyDiffScope(context.Background(), 42, "token", &version, "1", diffs); err != nil {
		t.Fatalf("applyDiffScope: %v", err)
	}
	if !diffs[0].Undiffable {
		t.Fatalf("resource not marked undiffable: %+v", diffs[0])
	}
	files, _, summary, warnings := buildReviewFiles(diffs, nil, 200, 1000)
	if len(files) != 0 || len(summary.Omitted) != 1 || summary.Omitted[0].Reason != "undiffable_resource" {
		t.Fatalf("files=%+v omitted=%+v", files, summary.Omitted)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "non-diffable resource") {
		t.Fatalf("warnings = %v, want a non-diffable resource warning", warnings)
	}
}

// ensureBodies lazily initializes the per-path body map on a fake.
func ensureBodies(m map[string]map[string]string) map[string]map[string]string {
	if m == nil {
		return map[string]map[string]string{}
	}
	return m
}

func setBody(fake *rawFileGitLab, path, ref, body string) {
	if fake.bodies == nil {
		fake.bodies = map[string]map[string]string{}
	}
	if fake.bodies[path] == nil {
		fake.bodies[path] = map[string]string{}
	}
	fake.bodies[path][ref] = body
}

// The recovered diff is ordinary review content: it builds a dossier and the
// changed line is reachable for an inline comment.
func TestBuildRecoveredDiffProducesReviewableContent(t *testing.T) {
	lines := numberedLines(50, 30)
	newLines := append([]string(nil), lines...)
	newLines[20] = "edited"
	diff, ok, _ := buildRecoveredDiff(joinLines(lines), joinLines(newLines))
	if !ok {
		t.Fatal("diff not recovered")
	}
	files, _, summary, warnings := buildReviewFiles([]gitlab.FileDiff{
		{OldPath: "f.txt", NewPath: "f.txt", Diff: diff},
	}, nil, 10, 1000)
	if len(files) != 1 || summary.Additions != 1 || summary.Deletions != 1 || len(warnings) != 0 {
		t.Fatalf("files=%+v summary=%+v warnings=%v", files, summary, warnings)
	}
	dossier, err := review.BuildDossier(review.DossierInput{
		Project:     review.ProjectRef{ID: 42, Path: "g/p"},
		MR:          review.MRMeta{IID: 7, Title: "recovered", SourceBranch: "feature", TargetBranch: "main"},
		DiffVersion: recoverVersion(),
		DiffSummary: summary,
		Files:       files,
	})
	if err != nil {
		t.Fatalf("BuildDossier: %v", err)
	}
	index := gitlab.BuildDiffIndex(dossier.DiffVersion, dossierDiffs(dossier))
	if pos, ok := index.Locate("f.txt", 21, gitlab.SideNew); !ok || pos.NewLine != 21 {
		t.Fatalf("dossier new-side line = %+v, %v; want NewLine 21", pos, ok)
	}
}
