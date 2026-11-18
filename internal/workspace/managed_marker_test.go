package workspace

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureDirClaimsOnlyNewProjectRoot(t *testing.T) {
	root := t.TempDir()
	w := NewManager(root, nil).Get(7)
	if err := os.MkdirAll(filepath.Join(root, "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ensureDir("runs"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "7", ManagedMarker)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("existing directory was claimed by ensureDir: %v", err)
	}
}

func TestEnsureDirKeepsConflictingMarker(t *testing.T) {
	root := t.TempDir()
	w := NewManager(root, nil).Get(7)
	if err := os.MkdirAll(filepath.Join(root, "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "7", ManagedMarker)
	const original = "tribunal-workspace-v1 8\n"
	if err := os.WriteFile(marker, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ensureDir("runs"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ensureDir = %v, want ErrUnsafePath for a conflicting marker", err)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != original {
		t.Fatalf("conflicting marker changed: %q, %v", data, err)
	}
}

func TestEnsureDirWritesMarkerOnCreate(t *testing.T) {
	root := t.TempDir()
	w := NewManager(root, nil).Get(7)
	if _, err := w.ensureDir("runs"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "7", ManagedMarker))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(data), ManagedMarkerValue(7)+"\n"; got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
}

func TestEnsureDirFailureDoesNotClaimLegacyDirectory(t *testing.T) {
	root := t.TempDir()
	w := NewManager(root, nil).Get(7)
	legacy := filepath.Join(root, "7")
	if err := os.MkdirAll(legacy, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(legacy, "context")); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ensureDir("context"); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("ensureDir = %v, want ErrUnsafePath", err)
	}
	if _, err := os.Stat(filepath.Join(legacy, ManagedMarker)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("failed operation claimed the legacy directory: %v", err)
	}
}

func TestEnsureRootWritesMarkerOnCreate(t *testing.T) {
	root := t.TempDir()
	w := NewManager(root, nil).Get(7)
	if err := w.EnsureRoot(); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "7", ManagedMarker))
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	if got, want := string(data), ManagedMarkerValue(7)+"\n"; got != want {
		t.Fatalf("marker = %q, want %q", got, want)
	}
}

func TestEnsureRootKeepsLegacyDirectoryUnclaimed(t *testing.T) {
	root := t.TempDir()
	w := NewManager(root, nil).Get(7)
	if err := os.MkdirAll(filepath.Join(root, "7"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := w.EnsureRoot(); err != nil {
		t.Fatalf("EnsureRoot: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "7", ManagedMarker)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("existing directory was claimed by EnsureRoot: %v", err)
	}
}

func TestEnsureRootRejectsSymlinkedRoot(t *testing.T) {
	root := t.TempDir()
	w := NewManager(root, nil).Get(7)
	outside := t.TempDir()
	if err := os.Symlink(outside, w.Root); err != nil {
		t.Fatal(err)
	}
	if err := w.EnsureRoot(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("EnsureRoot = %v, want ErrUnsafePath", err)
	}
	if _, err := os.Stat(filepath.Join(outside, ManagedMarker)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("marker written outside the workspace: %v", err)
	}
}

func TestEnsureRootRejectsSymlinkedParent(t *testing.T) {
	outside := t.TempDir()
	parent := filepath.Join(t.TempDir(), "workspaces")
	if err := os.Symlink(outside, parent); err != nil {
		t.Fatal(err)
	}
	w := NewManager(parent, nil).Get(7)
	if err := w.EnsureRoot(); !errors.Is(err, ErrUnsafePath) {
		t.Fatalf("EnsureRoot = %v, want ErrUnsafePath", err)
	}
	if _, err := os.Lstat(filepath.Join(outside, "7")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("project root was created outside the workspace: %v", err)
	}
}
