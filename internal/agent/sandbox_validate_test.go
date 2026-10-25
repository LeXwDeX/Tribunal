package agent

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestSandboxValidateLineRangeAcceptsExistingRange(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	if err := sb.ValidateLineRange("head", "notes.txt", 1, 2); err != nil {
		t.Fatalf("valid range: %v", err)
	}
	if err := sb.ValidateLineRange("base", "main.go", 1, 1); err != nil {
		t.Fatalf("base range: %v", err)
	}
	// A file without a trailing newline still has a final line.
	if err := sb.ValidateLineRange("head", "main.go", 3, 3); err != nil {
		t.Fatalf("final line: %v", err)
	}
}

func TestSandboxValidateLineRangeRejectsMissingAndOutOfRange(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	cases := []struct {
		name string
		root string
		path string
		line int
	}{
		{"missing", "head", "missing.go", 1},
		{"beyond eof", "head", "notes.txt", 3},
		{"unknown root", "nope", "notes.txt", 1},
		{"git metadata", "head", ".git/config", 1},
		{"symlink escape", "head", "link", 1},
		{"symlink directory escape", "head", "linkdir/secret.txt", 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := sb.ValidateLineRange(tc.root, tc.path, tc.line, tc.line); err == nil {
				t.Fatalf("%s was accepted", tc.path)
			}
		})
	}
}

func TestSandboxValidateLineRangeRejectsInvalidBounds(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	if err := sb.ValidateLineRange("head", "notes.txt", 0, 1); err == nil {
		t.Fatal("zero start line was accepted")
	}
	if err := sb.ValidateLineRange("head", "notes.txt", 2, 1); err == nil {
		t.Fatal("reversed range was accepted")
	}
	if err := sb.ValidateLineRange("head", "notes.txt", 1, 2); err != nil {
		t.Fatalf("valid range: %v", err)
	}
	if err := sb.ValidateLineRange("head", "pkg", 1, 1); err == nil ||
		!strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory error = %v", err)
	}
}

func TestSandboxValidateLineRangeUsesRootBoundary(t *testing.T) {
	parent := t.TempDir()
	head := filepath.Join(parent, "head")
	writeFile(t, filepath.Join(head, "empty.txt"), "")
	sb, err := NewSandbox(head, "")
	if err != nil {
		t.Fatalf("NewSandbox: %v", err)
	}
	defer sb.Close()
	if err := sb.ValidateLineRange("head", "empty.txt", 1, 1); err == nil {
		t.Fatal("empty file accepted line 1")
	}
	if err := sb.ValidateLineRange("head", "../outside", 1, 1); err == nil ||
		!errors.Is(err, ErrUnsafePath) {
		t.Fatalf("escape error = %v", err)
	}
}
