package app

import (
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

// plusDiff builds a diff whose body counts as exactly plus added lines.
func plusDiff(path string, plus int) gitlab.FileDiff {
	return gitlab.FileDiff{
		NewPath: path,
		OldPath: path,
		Diff:    "@@ -0,0 +1 @@\n" + strings.Repeat("+x\n", plus),
	}
}

func orderPaths(ordered []reviewDiffOrder) []string {
	paths := make([]string, len(ordered))
	for i, item := range ordered {
		paths[i] = reviewDiffPath(item.diff)
	}
	return paths
}

// #MR coverage: a truncated MR must keep the highest-risk files, and the
// ordering must be reproducible for a given input.
func TestOrderDiffsByRiskDeterministic(t *testing.T) {
	base := []gitlab.FileDiff{
		plusDiff("z.go", 1),
		plusDiff("internal/auth/login.go", 2),
		plusDiff("a.go", 3),
		plusDiff("db/migration_001.sql", 4),
		plusDiff("m.go", 5),
	}
	want := orderPaths(orderDiffsByRisk(append([]gitlab.FileDiff(nil), base...)))
	if again := orderPaths(orderDiffsByRisk(append([]gitlab.FileDiff(nil), base...))); !reflect.DeepEqual(again, want) {
		t.Fatalf("risk order is not reproducible: %v vs %v", want, again)
	}
	// Critical files first (largest first, path breaks ties); non-critical files
	// keep their input order.
	expected := []string{"db/migration_001.sql", "internal/auth/login.go", "z.go", "a.go", "m.go"}
	if !reflect.DeepEqual(want, expected) {
		t.Fatalf("risk order = %v, want %v", want, expected)
	}
}

// Shuffling the critical files in the input must not change their relative
// output order: it is decided by size then path.
func TestOrderDiffsByRiskCriticalOrderIndependentOfInputOrder(t *testing.T) {
	critical := []gitlab.FileDiff{
		plusDiff("internal/auth/login.go", 2),
		plusDiff("config/app.yaml", 5),
		plusDiff("db/migration_001.sql", 1),
	}
	nonCritical := []gitlab.FileDiff{plusDiff("web/bundle.js", 50), plusDiff("docs/readme.md", 40)}
	merge := func(order []int) []gitlab.FileDiff {
		out := make([]gitlab.FileDiff, 0, len(critical)+len(nonCritical))
		for _, idx := range order {
			out = append(out, critical[idx])
		}
		return append(out, nonCritical...)
	}
	want := orderPaths(orderDiffsByRisk(merge([]int{0, 1, 2})))
	// Every permutation of the critical files yields the same critical prefix.
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}, {2, 0, 1}} {
		got := orderPaths(orderDiffsByRisk(merge(order)))
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("critical order changed with input order %v: %v vs %v", order, got, want)
		}
	}
}

// A critical file must sort ahead of a larger, non-critical one so the max_files
// truncation keeps it.
func TestOrderDiffsByRiskPrioritizesCriticalPaths(t *testing.T) {
	ordered := orderPaths(orderDiffsByRisk([]gitlab.FileDiff{
		plusDiff("web/bundle.js", 50),
		plusDiff("internal/auth/login.go", 1),
		plusDiff("db/migration_002.sql", 1),
		plusDiff("docs/readme.md", 40),
	}))
	want := []string{"db/migration_002.sql", "internal/auth/login.go", "web/bundle.js", "docs/readme.md"}
	if !reflect.DeepEqual(ordered, want) {
		t.Fatalf("risk order = %v, want %v", ordered, want)
	}
}

// The omitted list is reported in path order regardless of the risk ordering
// used to pick which files are reviewed.
func TestBuildReviewFilesOmittedListSortedByPath(t *testing.T) {
	diffs := []gitlab.FileDiff{
		plusDiff("z.go", 5),
		plusDiff("internal/auth/x.go", 1),
		plusDiff("a.go", 2),
	}
	files, _, summary, _ := buildReviewFiles(diffs, nil, 1, 1000)
	if len(files) != 1 || files[0].Path != "internal/auth/x.go" {
		t.Fatalf("files = %+v, want the critical file reviewed", files)
	}
	paths := make([]string, len(summary.Omitted))
	for i, file := range summary.Omitted {
		paths[i] = file.Path
	}
	if !reflect.DeepEqual(paths, []string{"a.go", "z.go"}) {
		t.Fatalf("omitted paths = %v, want [a.go z.go]", paths)
	}
}
