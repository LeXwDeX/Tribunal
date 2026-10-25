package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func cleanupSHA(letter string) string { return strings.Repeat(letter, 40) }

func cleanupContextDir(t *testing.T, ws *Workspace, name string) string {
	t.Helper()
	dir := filepath.Join(ws.Root, "context", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func cleanupWriteMeta(t *testing.T, dir, sourceSHA string, locked bool) {
	t.Helper()
	meta := ContextMeta{
		SourceSHA:   sourceSHA,
		Model:       "test",
		GeneratedAt: time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC),
		Locked:      locked,
	}
	encoded, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func cleanupChtimes(t *testing.T, path string, at time.Time) {
	t.Helper()
	if err := os.Chtimes(path, at, at); err != nil {
		t.Fatal(err)
	}
}

func contextCleanupFixture(t *testing.T) (*Workspace, time.Time, time.Time) {
	t.Helper()
	ws := NewManager(t.TempDir(), nil).Get(11)
	now := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	return ws, now.Add(-2 * time.Hour), now.Add(-time.Hour)
}

func TestCleanupContextDirsRemovesOnlyAgedUnprotected(t *testing.T) {
	ws, old, cutoff := contextCleanupFixture(t)

	protected := cleanupContextDir(t, ws, cleanupSHA("a"))
	cleanupWriteMeta(t, protected, cleanupSHA("a"), false)
	cleanupChtimes(t, protected, old)

	orphan := cleanupContextDir(t, ws, cleanupSHA("b"))
	cleanupWriteMeta(t, orphan, cleanupSHA("b"), false)
	cleanupChtimes(t, orphan, old)

	fresh := cleanupContextDir(t, ws, cleanupSHA("c"))
	cleanupWriteMeta(t, fresh, cleanupSHA("c"), false)
	cleanupChtimes(t, fresh, cutoff.Add(time.Minute))

	invalid := cleanupContextDir(t, ws, "not-a-sha")
	cleanupChtimes(t, invalid, old)

	upper := cleanupContextDir(t, ws, strings.ToUpper(cleanupSHA("d")))
	cleanupWriteMeta(t, upper, strings.ToUpper(cleanupSHA("d")), false)
	cleanupChtimes(t, upper, old)

	file := filepath.Join(ws.Root, "context", "note.txt")
	if err := os.WriteFile(file, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	protectedSet := map[string]struct{}{
		cleanupSHA("a"): {},
		cleanupSHA("d"): {},
	}
	removed, err := ws.CleanupContextDirs(protectedSet, cutoff)
	if err != nil {
		t.Fatalf("CleanupContextDirs: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	for _, path := range []string{protected, fresh, invalid, upper, file} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("%s still exists: %v", orphan, err)
	}

	if removed, err := ws.CleanupContextDirs(protectedSet, cutoff); err != nil || removed != 0 {
		t.Fatalf("second CleanupContextDirs = %d, %v; want 0", removed, err)
	}
}

func TestCleanupContextDirsKeepsLockedAndMalformedMetadata(t *testing.T) {
	ws, old, cutoff := contextCleanupFixture(t)

	locked := cleanupContextDir(t, ws, cleanupSHA("a"))
	cleanupWriteMeta(t, locked, cleanupSHA("a"), true)
	cleanupChtimes(t, locked, old)

	malformed := cleanupContextDir(t, ws, cleanupSHA("b"))
	if err := os.WriteFile(filepath.Join(malformed, "meta.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupChtimes(t, malformed, old)

	missing := cleanupContextDir(t, ws, cleanupSHA("c"))
	if err := os.WriteFile(filepath.Join(missing, "AGENTS.md"), []byte("docs"), 0o600); err != nil {
		t.Fatal(err)
	}
	cleanupChtimes(t, missing, old)

	mismatched := cleanupContextDir(t, ws, cleanupSHA("d"))
	cleanupWriteMeta(t, mismatched, cleanupSHA("e"), false)
	cleanupChtimes(t, mismatched, old)

	orphan := cleanupContextDir(t, ws, cleanupSHA("f"))
	cleanupWriteMeta(t, orphan, cleanupSHA("f"), false)
	cleanupChtimes(t, orphan, old)

	removed, err := ws.CleanupContextDirs(map[string]struct{}{}, cutoff)
	if err == nil {
		t.Fatal("malformed metadata was accepted")
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	for _, path := range []string{locked, malformed, missing, mismatched} {
		if _, err := os.Lstat(path); err != nil {
			t.Errorf("%s was removed: %v", path, err)
		}
	}
	if _, err := os.Lstat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("valid orphan still exists: %v", err)
	}
}

func TestCleanupContextDirsRejectsUnsafePaths(t *testing.T) {
	t.Run("missing context root", func(t *testing.T) {
		ws := NewManager(t.TempDir(), nil).Get(12)
		removed, err := ws.CleanupContextDirs(nil, time.Now())
		if err != nil || removed != 0 {
			t.Fatalf("CleanupContextDirs = %d, %v; want 0", removed, err)
		}
		if _, err := os.Lstat(filepath.Join(ws.Root, "context")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("cleanup created the context root: %v", err)
		}
	})

	t.Run("symlinked context root", func(t *testing.T) {
		ws := NewManager(t.TempDir(), nil).Get(13)
		outside := t.TempDir()
		dir := filepath.Join(outside, cleanupSHA("a"))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		cleanupWriteMeta(t, dir, cleanupSHA("a"), false)
		cleanupChtimes(t, dir, time.Now().Add(-2*time.Hour))
		if err := os.MkdirAll(ws.Root, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(ws.Root, "context")); err != nil {
			t.Fatal(err)
		}
		if _, err := ws.CleanupContextDirs(nil, time.Now()); !errors.Is(err, ErrUnsafePath) {
			t.Fatalf("CleanupContextDirs = %v, want ErrUnsafePath", err)
		}
		if _, err := os.Lstat(dir); err != nil {
			t.Fatalf("cleanup followed the symlinked root: %v", err)
		}
	})

	t.Run("symlinked version directory", func(t *testing.T) {
		ws, _, cutoff := contextCleanupFixture(t)
		outside := t.TempDir()
		if err := os.WriteFile(filepath.Join(outside, "keep"), []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(ws.Root, "context"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, filepath.Join(ws.Root, "context", cleanupSHA("a"))); err != nil {
			t.Fatal(err)
		}
		removed, err := ws.CleanupContextDirs(nil, cutoff)
		if err != nil || removed != 0 {
			t.Fatalf("CleanupContextDirs = %d, %v; want 0", removed, err)
		}
		if _, err := os.Lstat(filepath.Join(outside, "keep")); err != nil {
			t.Fatalf("cleanup escaped through a symlink: %v", err)
		}
	})
}
