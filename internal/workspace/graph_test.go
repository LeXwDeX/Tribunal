package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func graphTestWorkspace(t *testing.T) *Workspace {
	t.Helper()
	return NewManager(t.TempDir(), nil).Get(21)
}

// graphTestSeed creates graphs/<runID> with a nested file, the way a stdio
// server's HOME fills the directory, and stamps the directory mtime.
func graphTestSeed(t *testing.T, ws *Workspace, runID string, mtime time.Time) string {
	t.Helper()
	dir := filepath.Join(ws.GraphsDir(), runID)
	home := filepath.Join(dir, ".mcp", "graph", "home")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "index.db"), []byte("graph"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatal(err)
	}
	return dir
}

func graphTestMissing(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("%s still exists: %v", path, err)
	}
}

func graphTestExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err != nil {
		t.Fatalf("%s is missing: %v", path, err)
	}
}

// graphTestOutside returns a directory outside the workspace with a canary
// file that no graph operation may touch.
func graphTestOutside(t *testing.T) (dir, canary string) {
	t.Helper()
	dir = t.TempDir()
	canary = filepath.Join(dir, "keep")
	if err := os.WriteFile(canary, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, canary
}

func TestGraphsDirIsDirectChildOfProjectRoot(t *testing.T) {
	ws := graphTestWorkspace(t)
	if got, want := ws.GraphsDir(), filepath.Join(ws.Root, "graphs"); got != want {
		t.Fatalf("GraphsDir = %q, want %q", got, want)
	}
}

func TestEnsureGraphDirCreatesPrivateEmptyDirectory(t *testing.T) {
	ws := graphTestWorkspace(t)
	dir, err := ws.EnsureGraphDir("run-1")
	if err != nil {
		t.Fatalf("EnsureGraphDir: %v", err)
	}
	if want := filepath.Join(ws.GraphsDir(), "run-1"); dir != want {
		t.Fatalf("dir = %q, want %q", dir, want)
	}
	for _, path := range []string{ws.GraphsDir(), dir} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != dirMode {
			t.Fatalf("%s: info=%v err=%v, want a 0700 directory", path, info, err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("fresh graph dir entries = %d, err=%v, want empty", len(entries), err)
	}
	// The project root is created the same way the other workspace
	// directories create it, marker included.
	if err := ws.verifyManagedMarker(); err != nil {
		t.Fatalf("managed marker: %v", err)
	}
}

func TestEnsureGraphDirDiscardsExistingContent(t *testing.T) {
	ws := graphTestWorkspace(t)
	stale := graphTestSeed(t, ws, "run-1", time.Now())
	sibling := graphTestSeed(t, ws, "run-2", time.Now())

	dir, err := ws.EnsureGraphDir("run-1")
	if err != nil {
		t.Fatalf("EnsureGraphDir: %v", err)
	}
	if dir != stale {
		t.Fatalf("dir = %q, want %q", dir, stale)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 0 {
		t.Fatalf("graph dir entries = %d, err=%v, want a fresh empty directory", len(entries), err)
	}
	graphTestExists(t, filepath.Join(sibling, ".mcp", "graph", "home", "index.db"))
}

func TestEnsureGraphDirRejectsInvalidRunIDs(t *testing.T) {
	ws := graphTestWorkspace(t)
	for _, id := range []string{"", ".", "..", "../evil", "a/b", `a\b`, "a b", "-leading", ".hidden"} {
		if dir, err := ws.EnsureGraphDir(id); !errors.Is(err, ErrInvalidRunID) || dir != "" {
			t.Errorf("EnsureGraphDir(%q) = %q, %v; want ErrInvalidRunID", id, dir, err)
		}
	}
	graphTestMissing(t, ws.GraphsDir())
}

func TestEnsureGraphDirRefusesSymlinks(t *testing.T) {
	t.Run("graphs directory", func(t *testing.T) {
		ws := graphTestWorkspace(t)
		if err := ws.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		outside, canary := graphTestOutside(t)
		if err := os.Symlink(outside, ws.GraphsDir()); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.EnsureGraphDir("run-1"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("EnsureGraphDir = %v, want ErrUnsafePath", err)
		}
		graphTestExists(t, canary)
		graphTestMissing(t, filepath.Join(outside, "run-1"))
	})
	t.Run("run directory", func(t *testing.T) {
		ws := graphTestWorkspace(t)
		if err := os.MkdirAll(ws.GraphsDir(), dirMode); err != nil {
			t.Fatal(err)
		}
		outside, canary := graphTestOutside(t)
		link := filepath.Join(ws.GraphsDir(), "run-1")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.EnsureGraphDir("run-1"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("EnsureGraphDir = %v, want ErrUnsafePath", err)
		}
		graphTestExists(t, canary)
		if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("symlink was replaced: info=%v err=%v", info, err)
		}
	})
	t.Run("run path is a file", func(t *testing.T) {
		ws := graphTestWorkspace(t)
		if err := os.MkdirAll(ws.GraphsDir(), dirMode); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(ws.GraphsDir(), "run-1")
		if err := os.WriteFile(file, []byte("not a directory"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.EnsureGraphDir("run-1"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("EnsureGraphDir = %v, want ErrUnsafePath", err)
		}
		graphTestExists(t, file)
	})
}

func TestRemoveGraphDirIsIdempotentAndScopedToOneRun(t *testing.T) {
	ws := graphTestWorkspace(t)
	if err := ws.RemoveGraphDir("run-1"); err != nil {
		t.Fatalf("RemoveGraphDir without a workspace root: %v", err)
	}
	target := graphTestSeed(t, ws, "run-1", time.Now())
	sibling := graphTestSeed(t, ws, "run-2", time.Now())

	for range 2 {
		if err := ws.RemoveGraphDir("run-1"); err != nil {
			t.Fatalf("RemoveGraphDir: %v", err)
		}
	}
	graphTestMissing(t, target)
	graphTestExists(t, filepath.Join(sibling, ".mcp", "graph", "home", "index.db"))
	graphTestExists(t, ws.GraphsDir())
}

func TestRemoveGraphDirRejectsInvalidRunIDs(t *testing.T) {
	ws := graphTestWorkspace(t)
	dir := graphTestSeed(t, ws, "run-1", time.Now())
	for _, id := range []string{"", "..", "../graphs/run-1", "run-1/..", "a/b"} {
		if err := ws.RemoveGraphDir(id); !errors.Is(err, ErrInvalidRunID) {
			t.Errorf("RemoveGraphDir(%q) = %v, want ErrInvalidRunID", id, err)
		}
	}
	graphTestExists(t, filepath.Join(dir, ".mcp", "graph", "home", "index.db"))
}

func TestRemoveGraphDirRefusesSymlinks(t *testing.T) {
	t.Run("run directory", func(t *testing.T) {
		ws := graphTestWorkspace(t)
		if err := os.MkdirAll(ws.GraphsDir(), dirMode); err != nil {
			t.Fatal(err)
		}
		outside, canary := graphTestOutside(t)
		link := filepath.Join(ws.GraphsDir(), "run-1")
		if err := os.Symlink(outside, link); err != nil {
			t.Fatal(err)
		}
		if err := ws.RemoveGraphDir("run-1"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("RemoveGraphDir = %v, want ErrUnsafePath", err)
		}
		graphTestExists(t, canary)
		graphTestExists(t, link)
	})
	t.Run("graphs directory", func(t *testing.T) {
		ws := graphTestWorkspace(t)
		if err := ws.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		outside, canary := graphTestOutside(t)
		if err := os.Mkdir(filepath.Join(outside, "run-1"), dirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, ws.GraphsDir()); err != nil {
			t.Fatal(err)
		}
		if err := ws.RemoveGraphDir("run-1"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("RemoveGraphDir = %v, want ErrUnsafePath", err)
		}
		graphTestExists(t, canary)
		graphTestExists(t, filepath.Join(outside, "run-1"))
	})
	t.Run("project root", func(t *testing.T) {
		manager := NewManager(t.TempDir(), nil)
		ws := manager.Get(22)
		outside, canary := graphTestOutside(t)
		if err := os.Mkdir(filepath.Join(outside, "graphs"), dirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(filepath.Join(outside, "graphs", "run-1"), dirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, ws.Root); err != nil {
			t.Fatal(err)
		}
		if err := ws.RemoveGraphDir("run-1"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("RemoveGraphDir = %v, want ErrUnsafePath", err)
		}
		graphTestExists(t, canary)
		graphTestExists(t, filepath.Join(outside, "graphs", "run-1"))
	})
}

func TestCleanupGraphDirsRemovesOnlyAgedUnprotected(t *testing.T) {
	ws := graphTestWorkspace(t)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	old, cutoff := now.Add(-2*time.Hour), now.Add(-time.Hour)

	stale := graphTestSeed(t, ws, "run-stale", old)
	protected := graphTestSeed(t, ws, "run-protected", old)
	fresh := graphTestSeed(t, ws, "run-fresh", cutoff.Add(time.Minute))
	atCutoff := graphTestSeed(t, ws, "run-at-cutoff", cutoff)
	// Directories that are not a valid run ID are never this package's.
	invalid := graphTestSeed(t, ws, "not a run", old)
	hidden := graphTestSeed(t, ws, ".hidden", old)
	file := filepath.Join(ws.GraphsDir(), "note.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(file, old, old); err != nil {
		t.Fatal(err)
	}
	outside, canary := graphTestOutside(t)
	link := filepath.Join(ws.GraphsDir(), "run-linked")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	removed, err := ws.CleanupGraphDirs(map[string]struct{}{"run-protected": {}}, cutoff)
	if err != nil || removed != 1 {
		t.Fatalf("CleanupGraphDirs = %d, %v; want 1 removal", removed, err)
	}
	graphTestMissing(t, stale)
	for _, path := range []string{protected, fresh, atCutoff, invalid, hidden, file, link, canary} {
		graphTestExists(t, path)
	}

	removed, err = ws.CleanupGraphDirs(map[string]struct{}{"run-protected": {}}, cutoff)
	if err != nil || removed != 0 {
		t.Fatalf("second CleanupGraphDirs = %d, %v; want an idempotent no-op", removed, err)
	}
}

func TestCleanupGraphDirsWithoutGraphsDirectory(t *testing.T) {
	ws := graphTestWorkspace(t)
	if removed, err := ws.CleanupGraphDirs(nil, time.Now()); err != nil || removed != 0 {
		t.Fatalf("missing project root: CleanupGraphDirs = %d, %v; want 0, nil", removed, err)
	}
	if err := ws.EnsureRoot(); err != nil {
		t.Fatal(err)
	}
	if removed, err := ws.CleanupGraphDirs(nil, time.Now()); err != nil || removed != 0 {
		t.Fatalf("missing graphs directory: CleanupGraphDirs = %d, %v; want 0, nil", removed, err)
	}
}

func TestCleanupGraphDirsRefusesUnsafeParents(t *testing.T) {
	t.Run("graphs directory is a symlink", func(t *testing.T) {
		ws := graphTestWorkspace(t)
		if err := ws.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		outside, canary := graphTestOutside(t)
		old := time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)
		child := filepath.Join(outside, "run-1")
		if err := os.Mkdir(child, dirMode); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(child, old, old); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, ws.GraphsDir()); err != nil {
			t.Fatal(err)
		}
		removed, err := ws.CleanupGraphDirs(nil, old.Add(time.Hour))
		if !errors.Is(err, ErrUnsafePath) || removed != 0 {
			t.Fatalf("CleanupGraphDirs = %d, %v; want 0, ErrUnsafePath", removed, err)
		}
		graphTestExists(t, canary)
		graphTestExists(t, child)
	})
	t.Run("graphs path is a file", func(t *testing.T) {
		ws := graphTestWorkspace(t)
		if err := ws.EnsureRoot(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ws.GraphsDir(), []byte("file"), 0o600); err != nil {
			t.Fatal(err)
		}
		if removed, err := ws.CleanupGraphDirs(nil, time.Now()); !errors.Is(err, ErrUnsafePath) || removed != 0 {
			t.Fatalf("CleanupGraphDirs = %d, %v; want 0, ErrUnsafePath", removed, err)
		}
	})
	t.Run("nil workspace", func(t *testing.T) {
		var ws *Workspace
		if removed, err := ws.CleanupGraphDirs(nil, time.Now()); !errors.Is(err, ErrUnsafePath) || removed != 0 {
			t.Fatalf("CleanupGraphDirs = %d, %v; want 0, ErrUnsafePath", removed, err)
		}
		if _, err := ws.EnsureGraphDir("run-1"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("EnsureGraphDir = %v, want ErrUnsafePath", err)
		}
		if err := ws.RemoveGraphDir("run-1"); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("RemoveGraphDir = %v, want ErrUnsafePath", err)
		}
	})
}
