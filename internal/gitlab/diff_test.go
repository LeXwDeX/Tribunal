package gitlab

import (
	"testing"
)

func testVersion() Version {
	return Version{ID: 1, BaseSHA: sha("a"), StartSHA: sha("b"), HeadSHA: sha("c")}
}

func TestDiffIndexLocateAddedDeletedContext(t *testing.T) {
	version := testVersion()
	index := BuildDiffIndex(version, []FileDiff{{
		OldPath: "pkg/a.go",
		NewPath: "pkg/a.go",
		Diff: "--- a/pkg/a.go\n+++ b/pkg/a.go\n" +
			"@@ -10,2 +10,3 @@ func x() {\n" +
			" context\n" +
			"+added\n" +
			" context2\n",
	}})

	tests := []struct {
		name    string
		path    string
		line    int
		side    Side
		ok      bool
		oldLine int
		newLine int
	}{
		{name: "context old side", path: "pkg/a.go", line: 10, side: SideOld, ok: true, oldLine: 10, newLine: 10},
		{name: "context new side", path: "pkg/a.go", line: 10, side: SideNew, ok: true, oldLine: 10, newLine: 10},
		{name: "added line", path: "pkg/a.go", line: 11, side: SideNew, ok: true, oldLine: 0, newLine: 11},
		{name: "second context", path: "pkg/a.go", line: 12, side: SideNew, ok: true, oldLine: 11, newLine: 12},
		{name: "line before hunk", path: "pkg/a.go", line: 1, side: SideNew, ok: false},
		{name: "line after hunk", path: "pkg/a.go", line: 14, side: SideNew, ok: false},
		{name: "unknown path", path: "pkg/b.go", line: 11, side: SideNew, ok: false},
		{name: "unknown side", path: "pkg/a.go", line: 11, side: Side("x"), ok: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pos, ok := index.Locate(tc.path, tc.line, tc.side)
			if ok != tc.ok {
				t.Fatalf("Locate ok = %v, want %v", ok, tc.ok)
			}
			if !ok {
				return
			}
			if pos.OldLine != tc.oldLine || pos.NewLine != tc.newLine || pos.OldPath != "pkg/a.go" ||
				pos.NewPath != "pkg/a.go" || pos.BaseSHA != version.BaseSHA {
				t.Fatalf("position = %+v", pos)
			}
		})
	}
}

func TestDiffIndexAddedLineNotOnOldSide(t *testing.T) {
	index := BuildDiffIndex(testVersion(), []FileDiff{{
		OldPath: "add.go",
		NewPath: "add.go",
		Diff:    "@@ -1,1 +1,2 @@\n context\n+added\n",
	}})
	if pos, ok := index.Locate("add.go", 2, SideNew); !ok || pos.NewLine != 2 || pos.OldLine != 0 {
		t.Fatalf("added locate = %+v, %v", pos, ok)
	}
	if _, ok := index.Locate("add.go", 2, SideOld); ok {
		t.Fatal("added line was reachable from the old side")
	}
}

func TestDiffIndexDeletedLines(t *testing.T) {
	index := BuildDiffIndex(testVersion(), []FileDiff{{
		OldPath: "gone.go",
		NewPath: "gone.go",
		Diff:    "@@ -1,2 +0,0 @@\n-old one\n-old two\n",
	}})
	if pos, ok := index.Locate("gone.go", 2, SideOld); !ok || pos.OldLine != 2 || pos.NewLine != 0 {
		t.Fatalf("deleted locate = %+v, %v", pos, ok)
	}
	if _, ok := index.Locate("gone.go", 2, SideNew); ok {
		t.Fatal("deleted line was reachable from the new side")
	}
	if _, ok := index.Locate("gone.go", 3, SideOld); ok {
		t.Fatal("line outside the hunk was indexed")
	}
}

func TestDiffIndexRenameUsesBothPaths(t *testing.T) {
	index := BuildDiffIndex(testVersion(), []FileDiff{{
		OldPath:     "old.go",
		NewPath:     "new.go",
		RenamedFile: true,
		Diff:        "@@ -1 +1 @@\n-before\n+after\n",
	}})
	for _, tc := range []struct {
		path string
		side Side
	}{
		{path: "old.go", side: SideOld},
		{path: "new.go", side: SideNew},
	} {
		pos, ok := index.Locate(tc.path, 1, tc.side)
		if !ok {
			t.Fatalf("rename path %q side %s was not indexed", tc.path, tc.side)
		}
		if pos.OldPath != "old.go" || pos.NewPath != "new.go" {
			t.Fatalf("position paths = %+v", pos)
		}
	}
	if _, ok := index.Locate("old.go", 1, SideNew); ok {
		t.Fatal("old rename path answered from the new side")
	}
	if _, ok := index.Locate("new.go", 1, SideOld); ok {
		t.Fatal("new rename path answered from the old side")
	}
}

func TestDiffIndexMultipleHunks(t *testing.T) {
	index := BuildDiffIndex(testVersion(), []FileDiff{{
		OldPath: "multi.go",
		NewPath: "multi.go",
		Diff: "@@ -1,1 +1,1 @@\n-one\n+ONE\n" +
			"@@ -50,1 +50,1 @@\n-fifty\n+FIFTY\n",
	}})
	if pos, ok := index.Locate("multi.go", 1, SideNew); !ok || pos.NewLine != 1 {
		t.Fatalf("first hunk = %+v, %v", pos, ok)
	}
	if pos, ok := index.Locate("multi.go", 50, SideNew); !ok || pos.NewLine != 50 {
		t.Fatalf("second hunk = %+v, %v", pos, ok)
	}
	if _, ok := index.Locate("multi.go", 20, SideNew); ok {
		t.Fatal("gap between hunks was indexed")
	}
}

func TestDiffIndexSkipsUnsafeDiffs(t *testing.T) {
	version := testVersion()
	cases := []struct {
		name string
		diff FileDiff
	}{
		{name: "binary", diff: FileDiff{OldPath: "a.bin", NewPath: "a.bin", Diff: "Binary files a/a.bin and b/a.bin differ\n"}},
		{name: "git binary patch", diff: FileDiff{OldPath: "a.bin", NewPath: "a.bin", Diff: "GIT binary patch\nliteral 0\n"}},
		{name: "collapsed", diff: FileDiff{OldPath: "a.go", NewPath: "a.go", Collapsed: true, Diff: "@@ -1 +1 @@\n-a\n+b\n"}},
		{name: "too large", diff: FileDiff{OldPath: "a.go", NewPath: "a.go", TooLarge: true, Diff: "@@ -1 +1 @@\n-a\n+b\n"}},
		{name: "empty diff", diff: FileDiff{OldPath: "a.go", NewPath: "a.go"}},
		{name: "bad header", diff: FileDiff{OldPath: "a.go", NewPath: "a.go", Diff: "@@ bad @@\n+x\n"}},
		{name: "short hunk", diff: FileDiff{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1,2 +1,2 @@\n+x\n"}},
		{name: "extra hunk line", diff: FileDiff{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1,1 +1,1 @@\n+x\n+y\n"}},
		{name: "unknown prefix", diff: FileDiff{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1,1 +1,1 @@\n?bad\n"}},
		{name: "overflow start", diff: FileDiff{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -9999999999,1 +1,1 @@\n+x\n"}},
		{name: "overflow sum", diff: FileDiff{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -2147483647,2 +1,1 @@\n+x\n"}},
		{name: "invalid path", diff: FileDiff{OldPath: "../a.go", NewPath: "../a.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var index *DiffIndex
			func() {
				defer func() {
					if recovered := recover(); recovered != nil {
						t.Fatalf("BuildDiffIndex panicked: %v", recovered)
					}
				}()
				index = BuildDiffIndex(version, []FileDiff{tc.diff})
			}()
			for _, side := range []Side{SideNew, SideOld} {
				for line := 0; line <= 3; line++ {
					if _, ok := index.Locate(tc.diff.NewPath, line, side); ok {
						t.Fatalf("unsafe diff was indexed at line %d side %s", line, side)
					}
				}
			}
		})
	}
}

func TestDiffIndexRejectsInvalidVersion(t *testing.T) {
	index := BuildDiffIndex(Version{}, []FileDiff{{
		OldPath: "a.go",
		NewPath: "a.go",
		Diff:    "@@ -1 +1 @@\n-a\n+b\n",
	}})
	if _, ok := index.Locate("a.go", 1, SideNew); ok {
		t.Fatal("position was produced without valid version SHAs")
	}
}
