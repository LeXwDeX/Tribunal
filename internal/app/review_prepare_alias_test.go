package app

import (
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
)

const aliasDiffBody = "@@ -1 +1 @@\n-old\n+new\n"

func TestIgnoredOldPathsRecordsAndFlagsIncomplete(t *testing.T) {
	// Ignore matched through the old path only: the old path is recorded.
	old, complete := ignoredOldPaths([]gitlab.FileDiff{
		renameDiff("gen.lock", "kept.go", aliasDiffBody),
		{NewPath: "a.go", Diff: aliasDiffBody},
	}, []string{"*.lock"})
	if !complete || !reflect.DeepEqual(old, []string{"gen.lock"}) {
		t.Fatalf("old=%v complete=%v, want [gen.lock] complete", old, complete)
	}
	for _, diff := range []gitlab.FileDiff{
		{NewPath: "x.lock", RenamedFile: true, Diff: aliasDiffBody},
		{OldPath: "x.lock", RenamedFile: true, Diff: aliasDiffBody},
	} {
		if _, complete := ignoredOldPaths([]gitlab.FileDiff{diff}, []string{"*.lock"}); complete {
			t.Fatalf("ignored rename missing a side must be incomplete: %+v", diff)
		}
	}
}

func TestBuildReviewFilesOmissionsRecordOldPath(t *testing.T) {
	for _, tc := range []struct {
		name        string
		diffs       []gitlab.FileDiff
		maxFiles    int
		maxLines    int
		wantReason  string
		wantOldPath string
	}{
		{"max_files", []gitlab.FileDiff{{NewPath: "a.go", Diff: aliasDiffBody}, renameDiff("o.go", "n.go", aliasDiffBody)}, 1, 100, "max_files", "o.go"},
		{"too_large", []gitlab.FileDiff{func() gitlab.FileDiff { d := renameDiff("o.go", "n.go", aliasDiffBody); d.TooLarge = true; return d }()}, 10, 100, "too_large", "o.go"},
		{"collapsed", []gitlab.FileDiff{func() gitlab.FileDiff { d := renameDiff("o.go", "n.go", aliasDiffBody); d.Collapsed = true; return d }()}, 10, 100, "collapsed", "o.go"},
		{"max_diff_lines", []gitlab.FileDiff{renameDiff("o.go", "n.go", aliasDiffBody)}, 10, 1, "max_diff_lines", "o.go"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, summary, _ := buildReviewFiles(tc.diffs, nil, tc.maxFiles, tc.maxLines)
			if len(summary.Omitted) != 1 {
				t.Fatalf("omitted = %+v", summary.Omitted)
			}
			got := summary.Omitted[0]
			if got.Reason != tc.wantReason || got.Path != "n.go" || got.OldPath != tc.wantOldPath {
				t.Fatalf("omitted = %+v, want %s n.go old %s", got, tc.wantReason, tc.wantOldPath)
			}
		})
	}
}

func TestMissingRenamePathOmissionWithholdsEverything(t *testing.T) {
	files, _, summary, _ := buildReviewFiles([]gitlab.FileDiff{
		{NewPath: "n.go", RenamedFile: true, Diff: aliasDiffBody},
		{NewPath: "keep.go", Diff: aliasDiffBody},
	}, nil, 10, 100)
	d := &review.Dossier{Files: files, DiffSummary: summary, PathAliasInfo: true}
	if all, _ := d.WithheldPaths(); !all {
		t.Fatalf("missing_rename_path omission must withhold every path: %+v", summary.Omitted)
	}
}

func TestIncompleteIgnoredRenameDropsAliasInfo(t *testing.T) {
	diffs := []gitlab.FileDiff{
		{NewPath: "x.lock", RenamedFile: true, Diff: aliasDiffBody},
		{NewPath: "keep.go", Diff: aliasDiffBody},
	}
	files, ignored, summary, _ := buildReviewFiles(diffs, []string{"*.lock"}, 10, 100)
	oldPaths, complete := ignoredOldPaths(diffs, []string{"*.lock"})
	d := &review.Dossier{Files: files, Ignored: ignored, IgnoredOldPaths: oldPaths, DiffSummary: summary, PathAliasInfo: complete}
	if all, _ := d.WithheldPaths(); !all {
		t.Fatal("ignored rename missing a side must fall back to full withholding")
	}
}
