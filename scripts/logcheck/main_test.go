package main

import (
	"bytes"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRepositoryLogsUseErrorClass keeps raw errors out of production logs.
func TestRepositoryLogsUseErrorClass(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	findings, err := checkTree(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range findings {
		t.Errorf("%s", f)
	}
}

func TestCheckSourceFlagsRawErrors(t *testing.T) {
	src := `package p

import "log/slog"

func f(logger *slog.Logger, err error) {
	logger.Warn("a", "id", 1, "error", err)
	logger.Info("b", "reason", err.Error())
	logger.Error("c", slog.Any("error", err))
	logger.With("error", err).Info("d")
	logger.WarnContext(nil, "e", "detail", wrap(err.Error()))
}
`
	findings, err := checkSource(token.NewFileSet(), "p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, f := range findings {
		lines = append(lines, f.Pos.Line)
	}
	want := []int{6, 7, 8, 9, 10}
	if len(lines) != len(want) {
		t.Fatalf("findings = %v, want lines %v", findings, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("findings = %v, want lines %v", findings, want)
		}
	}
}

func TestCheckSourceAllowsErrorClass(t *testing.T) {
	src := `package p

import (
	"errors"
	"log/slog"
)

func f(logger *slog.Logger, err error, t interface{ Error(...any) }) error {
	logger.Warn("a", "id", 1, "error_class", classOf(err))
	logger.Error("b", "errors", 2)
	t.Error("not a logger key")
	return errors.New(err.Error())
}
`
	findings, err := checkSource(token.NewFileSet(), "p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 0 {
		t.Fatalf("findings = %v, want none", findings)
	}
}

func TestRunReportsFindings(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "a.go"), "package a\n\nfunc f(l interface{ Warn(string, ...any) }, err error) { l.Warn(\"x\", \"error\", err) }\n")
	writeFile(t, filepath.Join(dir, "a_test.go"), "package a\n\nfunc g(l interface{ Warn(string, ...any) }, err error) { l.Warn(\"x\", \"error\", err) }\n")
	var stdout, stderr bytes.Buffer
	if code := run(dir, &stdout, &stderr); code != 1 {
		t.Fatalf("run = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "a.go:3:") || strings.Contains(stderr.String(), "a_test.go") {
		t.Fatalf("stderr = %s", stderr.String())
	}

	clean := t.TempDir()
	writeFile(t, filepath.Join(clean, "b.go"), "package b\n")
	stdout.Reset()
	stderr.Reset()
	if code := run(clean, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), "ok") {
		t.Fatalf("run = %d, stdout = %s, stderr = %s", code, stdout.String(), stderr.String())
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
