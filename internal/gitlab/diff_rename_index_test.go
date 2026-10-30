package gitlab

// These tests invert the observed assertions in
// docs/audit-repro-4839248/162-gitlab_rename_index_test.go.txt.

import "testing"

func chainDiff(oldPath, newPath, diff string) FileDiff {
	return FileDiff{OldPath: oldPath, NewPath: newPath, RenamedFile: true, Diff: diff}
}

func TestDiffIndexRenameChainRoutesByRequestedSide(t *testing.T) {
	first := chainDiff("b.go", "c.go", "@@ -1 +1 @@\n-b old\n+c new\n")
	second := chainDiff("a.go", "b.go", "@@ -1 +1 @@\n-a old\n+b new\n")
	for _, tc := range []struct {
		name  string
		diffs []FileDiff
	}{
		{name: "new then old", diffs: []FileDiff{first, second}},
		{name: "old then new", diffs: []FileDiff{second, first}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			version := testVersion()
			index := BuildDiffIndex(version, tc.diffs)
			newSide, ok := index.Locate("b.go", 1, SideNew)
			if !ok {
				t.Fatal("new-side b.go was not located through a.go -> b.go")
			}
			wantNew := Position{
				BaseSHA: version.BaseSHA, StartSHA: version.StartSHA, HeadSHA: version.HeadSHA,
				OldPath: "a.go", NewPath: "b.go", NewLine: 1,
			}
			if newSide != wantNew {
				t.Fatalf("new-side b.go = %+v, want %+v", newSide, wantNew)
			}
			oldSide, ok := index.Locate("b.go", 1, SideOld)
			if !ok {
				t.Fatal("old-side b.go was not located through b.go -> c.go")
			}
			wantOld := Position{
				BaseSHA: version.BaseSHA, StartSHA: version.StartSHA, HeadSHA: version.HeadSHA,
				OldPath: "b.go", NewPath: "c.go", OldLine: 1,
			}
			if oldSide != wantOld {
				t.Fatalf("old-side b.go = %+v, want %+v", oldSide, wantOld)
			}
			if got, ok := index.Locate("a.go", 1, SideNew); ok {
				t.Fatalf("old-side a.go answered from the new side: %+v", got)
			}
			if got, ok := index.Locate("c.go", 1, SideOld); ok {
				t.Fatalf("new-side c.go answered from the old side: %+v", got)
			}
		})
	}
}

func TestDiffIndexRenameChainDoesNotFallThroughMissingLine(t *testing.T) {
	first := chainDiff("b.go", "c.go", "@@ -1 +1 @@\n-b old\n+c new\n")
	second := chainDiff("a.go", "b.go", "@@ -3 +3 @@\n-a old\n+b new\n")
	for _, tc := range []struct {
		name  string
		diffs []FileDiff
	}{
		{name: "new then old", diffs: []FileDiff{first, second}},
		{name: "old then new", diffs: []FileDiff{second, first}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			index := BuildDiffIndex(testVersion(), tc.diffs)
			if pos, ok := index.Locate("b.go", 1, SideNew); ok {
				t.Fatalf("new-side b.go line 1 fell through to another file: %+v", pos)
			}
			if pos, ok := index.Locate("b.go", 3, SideNew); !ok || pos.OldPath != "a.go" || pos.NewPath != "b.go" || pos.NewLine != 3 {
				t.Fatalf("new-side b.go line 3 = %+v, %v", pos, ok)
			}
			if pos, ok := index.Locate("b.go", 1, SideOld); !ok || pos.OldPath != "b.go" || pos.NewPath != "c.go" || pos.OldLine != 1 {
				t.Fatalf("old-side b.go line 1 = %+v, %v", pos, ok)
			}
			if pos, ok := index.Locate("b.go", 3, SideOld); ok {
				t.Fatalf("old-side b.go line 3 fell through to another file: %+v", pos)
			}
		})
	}
}

func TestDiffIndexWithheldCanonicalPathFailsClosed(t *testing.T) {
	withheld := []struct {
		name string
		diff FileDiff
	}{
		{name: "collapsed", diff: FileDiff{OldPath: "b.go", NewPath: "c.go", RenamedFile: true, Collapsed: true, Diff: "@@ -1 +1 @@\n-b old\n+c new\n"}},
		{name: "too large", diff: FileDiff{OldPath: "b.go", NewPath: "c.go", RenamedFile: true, TooLarge: true, Diff: "@@ -1 +1 @@\n-b old\n+c new\n"}},
		{name: "malformed", diff: FileDiff{OldPath: "b.go", NewPath: "c.go", RenamedFile: true, Diff: "@@ malformed @@\n-b old\n+c new\n"}},
		{name: "empty", diff: FileDiff{OldPath: "b.go", NewPath: "c.go", RenamedFile: true}},
		{name: "header only", diff: FileDiff{OldPath: "b.go", NewPath: "c.go", RenamedFile: true, Diff: "--- a/b.go\n+++ b/c.go\n"}},
		{name: "binary", diff: FileDiff{OldPath: "b.go", NewPath: "c.go", RenamedFile: true, Diff: "Binary files a/b.go and b/c.go differ\n"}},
	}
	valid := chainDiff("a.go", "b.go", "@@ -1 +1 @@\n-a old\n+b new\n")
	for _, tc := range withheld {
		t.Run(tc.name+" withheld first", func(t *testing.T) {
			index := BuildDiffIndex(testVersion(), []FileDiff{tc.diff, valid})
			if pos, ok := index.Locate("b.go", 1, SideOld); ok {
				t.Fatalf("withheld old-side b.go produced a position: %+v", pos)
			}
			if pos, ok := index.Locate("b.go", 1, SideNew); !ok || pos.OldPath != "a.go" || pos.NewPath != "b.go" {
				t.Fatalf("new-side b.go lost its canonical file: %+v, %v", pos, ok)
			}
		})
		t.Run(tc.name+" withheld second", func(t *testing.T) {
			validFirst := chainDiff("b.go", "c.go", "@@ -1 +1 @@\n-b old\n+c new\n")
			other := tc.diff
			other.OldPath, other.NewPath = "a.go", "b.go"
			index := BuildDiffIndex(testVersion(), []FileDiff{validFirst, other})
			if pos, ok := index.Locate("b.go", 1, SideNew); ok {
				t.Fatalf("withheld new-side b.go produced a position: %+v", pos)
			}
			if pos, ok := index.Locate("b.go", 1, SideOld); !ok || pos.OldPath != "b.go" || pos.NewPath != "c.go" {
				t.Fatalf("old-side b.go lost its canonical file: %+v, %v", pos, ok)
			}
		})
	}
}

func TestDiffIndexHeaderOnlyDiffDoesNotCreatePositions(t *testing.T) {
	index := BuildDiffIndex(testVersion(), []FileDiff{
		chainDiff("b.go", "c.go", "--- a/b.go\n+++ b/c.go\n"),
	})
	for _, tc := range []struct {
		path string
		side Side
	}{
		{path: "b.go", side: SideNew},
		{path: "c.go", side: SideOld},
		{path: "b.go", side: SideOld},
		{path: "c.go", side: SideNew},
	} {
		if pos, ok := index.Locate(tc.path, 1, tc.side); ok {
			t.Fatalf("header-only diff located %s side %s at %+v", tc.path, tc.side, pos)
		}
	}
}

func TestDiffIndexSharedOldPathDoesNotAliasAcrossSides(t *testing.T) {
	first := chainDiff("b.go", "c.go", "@@ -1 +1 @@\n-b old\n+c new\n")
	second := chainDiff("b.go", "d.go", "@@ -1 +1 @@\n-b old\n+d new\n")
	index := BuildDiffIndex(testVersion(), []FileDiff{first, second})
	if pos, ok := index.Locate("b.go", 1, SideNew); ok {
		t.Fatalf("old path b.go answered from the new side: %+v", pos)
	}
	if pos, ok := index.Locate("b.go", 1, SideOld); !ok || pos.OldPath != "b.go" || pos.NewPath != "c.go" {
		t.Fatalf("old-side b.go canonical file = %+v, %v", pos, ok)
	}
	for _, path := range []string{"c.go", "d.go"} {
		if pos, ok := index.Locate(path, 1, SideOld); ok {
			t.Fatalf("new path %s answered from the old side: %+v", path, pos)
		}
	}
}

func TestDiffIndexDuplicateRenameEntryKeepsCanonicalSides(t *testing.T) {
	diff := chainDiff("b.go", "c.go", "@@ -1 +1 @@\n-b old\n+c new\n")
	index := BuildDiffIndex(testVersion(), []FileDiff{diff, diff})
	oldPos, oldOK := index.Locate("b.go", 1, SideOld)
	newPos, newOK := index.Locate("c.go", 1, SideNew)
	if !oldOK || oldPos.OldPath != "b.go" || oldPos.NewPath != "c.go" || oldPos.OldLine != 1 {
		t.Fatalf("duplicate old side = %+v, %v", oldPos, oldOK)
	}
	if !newOK || newPos.OldPath != "b.go" || newPos.NewPath != "c.go" || newPos.NewLine != 1 {
		t.Fatalf("duplicate new side = %+v, %v", newPos, newOK)
	}
}

func TestDiffIndexRenamePathsAnswerOnlyTheirSide(t *testing.T) {
	index := BuildDiffIndex(testVersion(), []FileDiff{
		chainDiff("old.go", "new.go", "@@ -1 +1 @@\n-before\n+after\n"),
	})
	for _, tc := range []struct {
		path string
		side Side
		ok   bool
	}{
		{path: "old.go", side: SideOld, ok: true},
		{path: "new.go", side: SideNew, ok: true},
		{path: "old.go", side: SideNew, ok: false},
		{path: "new.go", side: SideOld, ok: false},
	} {
		pos, ok := index.Locate(tc.path, 1, tc.side)
		if ok != tc.ok {
			t.Fatalf("rename path %s side %s = %+v, %v; want ok=%v", tc.path, tc.side, pos, ok, tc.ok)
		}
		if ok && (pos.OldPath != "old.go" || pos.NewPath != "new.go") {
			t.Fatalf("position paths = %+v", pos)
		}
	}
}

func TestDiffIndexDoesNotAliasRenamePathsAcrossSides(t *testing.T) {
	index := BuildDiffIndex(testVersion(), []FileDiff{
		chainDiff("b.go", "c.go", "@@ -1 +1 @@\n-b old\n+c new\n"),
	})
	if pos, ok := index.Locate("b.go", 1, SideNew); ok {
		t.Fatalf("old path answered from the new side: %+v", pos)
	}
	if pos, ok := index.Locate("c.go", 1, SideOld); ok {
		t.Fatalf("new path answered from the old side: %+v", pos)
	}
	if pos, ok := index.Locate("b.go", 1, SideOld); !ok || pos.OldPath != "b.go" || pos.NewPath != "c.go" || pos.OldLine != 1 {
		t.Fatalf("canonical old path = %+v, %v", pos, ok)
	}
	if pos, ok := index.Locate("c.go", 1, SideNew); !ok || pos.OldPath != "b.go" || pos.NewPath != "c.go" || pos.NewLine != 1 {
		t.Fatalf("canonical new path = %+v, %v", pos, ok)
	}
}

func TestDiffIndexRejectsExtraContentOutsideHunk(t *testing.T) {
	tests := []struct {
		name string
		diff string
	}{
		{name: "extra addition", diff: "@@ -1 +1 @@\n-old\n+new\n+extra\n"},
		{name: "extra deletion", diff: "@@ -1 +1 @@\n-old\n+new\n-extra\n"},
		{name: "extra context", diff: "@@ -1 +1 @@\n-old\n+new\n context\n"},
		{name: "plain text", diff: "@@ -1 +1 @@\n-old\n+new\nunexpected\n"},
		{name: "extra header-like addition", diff: "@@ -1 +1 @@\n-old\n+new\n+++ extra\n"},
		{name: "extra header-like deletion", diff: "@@ -1 +1 @@\n-old\n+new\n--- extra\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			index := BuildDiffIndex(testVersion(), []FileDiff{{
				OldPath: "a.go", NewPath: "a.go", Diff: tc.diff,
			}})
			for _, side := range []Side{SideNew, SideOld} {
				for line := 1; line <= 4; line++ {
					if pos, ok := index.Locate("a.go", line, side); ok {
						t.Fatalf("malformed diff produced position %+v at line %d side %s", pos, line, side)
					}
				}
			}
		})
	}
}

func TestDiffIndexAllowsFileMetadataAndMultipleHunks(t *testing.T) {
	version := testVersion()
	index := BuildDiffIndex(version, []FileDiff{{
		OldPath: "a.go", NewPath: "a.go",
		Diff: "diff --git a/a.go b/a.go\n" +
			"index 1111111..2222222 100644\n" +
			"--- a/a.go\n" +
			"+++ b/a.go\n" +
			"@@ -1 +1 @@\n" +
			"-one\n" +
			"+ONE\n" +
			"@@ -10 +10 @@\n" +
			"-ten\n" +
			"+TEN\n",
	}})
	for _, tc := range []struct {
		line    int
		oldLine int
		newLine int
	}{
		{line: 1, oldLine: 1, newLine: 1},
		{line: 10, oldLine: 10, newLine: 10},
	} {
		oldPos, oldOK := index.Locate("a.go", tc.line, SideOld)
		newPos, newOK := index.Locate("a.go", tc.line, SideNew)
		if !oldOK || !newOK || oldPos.OldLine != tc.oldLine || newPos.NewLine != tc.newLine {
			t.Fatalf("line %d: old=%+v/%v new=%+v/%v", tc.line, oldPos, oldOK, newPos, newOK)
		}
	}
}

func TestDiffIndexAllowsNoNewlineMarkerAfterHunk(t *testing.T) {
	index := BuildDiffIndex(testVersion(), []FileDiff{{
		OldPath: "a.go", NewPath: "a.go",
		Diff: "@@ -1 +1 @@\n-old\n+new\n\\ No newline at end of file\n",
	}})
	if pos, ok := index.Locate("a.go", 1, SideNew); !ok || pos.NewLine != 1 {
		t.Fatalf("position = %+v, %v", pos, ok)
	}
}
