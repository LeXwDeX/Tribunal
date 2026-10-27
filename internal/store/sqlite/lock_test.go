package sqlite

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestLockDataDirIsExclusiveUntilReleased(t *testing.T) {
	if !dataDirLockSupported {
		t.Skip("data directory lock is not supported on this platform")
	}
	dir := t.TempDir()
	first, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("first lock: %v", err)
	}
	if got, want := first.Path(), filepath.Join(dir, LockFileName); got != want {
		t.Fatalf("lock path = %q, want %q", got, want)
	}
	if _, err := LockDataDir(dir); !errors.Is(err, ErrDataDirLocked) {
		t.Fatalf("second lock error = %v, want ErrDataDirLocked", err)
	}
	if err := first.Release(); err != nil {
		t.Fatalf("release: %v", err)
	}
	again, err := LockDataDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("second release: %v", err)
	}
	if err := again.Release(); err != nil {
		t.Fatalf("repeated release: %v", err)
	}
}

func TestLockDataDirMissingDirectory(t *testing.T) {
	if _, err := LockDataDir(filepath.Join(t.TempDir(), "missing")); err == nil || errors.Is(err, ErrDataDirLocked) {
		t.Fatalf("missing directory error = %v, want open failure", err)
	}
}
