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

func TestCheckSourceFlagsRawErrorsUnderOtherKeys(t *testing.T) {
	cases := map[string]string{
		"detail":            `logger.Warn("x", "detail", err)`,
		"reason":            `logger.Warn("x", "reason", err)`,
		"class key":         `logger.Warn("x", "error_class", err)`,
		"named cause":       `logger.Warn("x", "detail", cause)`,
		"declared variable": `var failure error; logger.Warn("x", "detail", failure)`,
		"parenthesized":     `logger.Warn("x", "detail", (err))`,
		"attribute":         `logger.Warn("x", slog.Any("detail", err))`,
		"attribute cause":   `logger.Warn("x", slog.Any("reason", cause))`,
		"attribute class":   `logger.Warn("x", slog.Any("error_class", err))`,
		"nested group":      `logger.Warn("x", slog.Group("group", slog.Any("detail", err)))`,
		"with receiver":     `logger.With("detail", err).Info("x")`,
	}
	for name, statement := range cases {
		t.Run(name, func(t *testing.T) {
			src := `package p
import "log/slog"
func f(logger *slog.Logger, err, cause error) {
` + statement + `
}`
			findings, err := checkSource(token.NewFileSet(), "p.go", []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != 1 {
				t.Fatalf("findings = %v, want one", findings)
			}
		})
	}
}

func TestCheckSourceFlagsInferredErrors(t *testing.T) {
	src := `package p
import "log/slog"
func f(logger *slog.Logger) {
	err := operation()
	closeErr := operation()
	logger.Warn("x", "detail", err)
	logger.Warn("x", slog.Any("reason", closeErr))
}`
	findings, err := checkSource(token.NewFileSet(), "p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 || findings[0].Pos.Line != 6 || findings[1].Pos.Line != 7 {
		t.Fatalf("findings = %v, want lines 6 and 7", findings)
	}
}

func TestCheckSourceStandardErrorConstructors(t *testing.T) {
	src := `package p
import (
  standarderrors "errors"
  format "fmt"
  "log/slog"
)
func f(logger *slog.Logger) {
  problem := standarderrors.New("fake-test-error")
  logger.Warn("failed", "detail", problem)
  failure := format.Errorf("failed: %s", "example")
  logger.Warn("failed", slog.Any("detail", failure))
  copied := problem
  logger.Warn("failed", "detail", copied)
  var assigned error
  assigned = standarderrors.New("fake-test-error")
  logger.Warn("failed", "detail", assigned)
  logger.Warn("failed", "detail", format.Errorf("bad"))
  var another = standarderrors.New("bad")
  another = format.Errorf("bad again")
  logger.Warn("failed", slog.Any("detail", another))
}`
	findings, err := checkSource(token.NewFileSet(), "p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []int{9, 11, 13, 16, 17, 20}
	if len(findings) != len(want) {
		t.Fatalf("findings = %v, want lines %v", findings, want)
	}
	for i, line := range want {
		if findings[i].Pos.Line != line {
			t.Fatalf("findings = %v, want lines %v", findings, want)
		}
	}
}

func TestCheckSourceInferredOrdinaryValuesAndScopes(t *testing.T) {
	src := `package p
import (
  "errors"
  "fmt"
  "log/slog"
  "github.com/LeXwDeX/tribunal/internal/errclass"
)
func f(logger *slog.Logger, err error) {
  problem := errors.New("fake-test-error")
  {
    problem := fmt.Sprintf("safe")
    problem = "safe again"
    logger.Warn("x", "detail", problem)
  }
  logger.Warn("x", "detail", problem)
  classErr := errclass.Of(err)
  logger.Warn("x", "error_class", classErr)
  textErr := fmt.Sprintf("safe")
  textErr = "still safe"
  logger.Warn("x", "detail", textErr)
  var categorizedErr = errclass.Of(err)
  logger.Warn("x", "detail", categorizedErr)
}`
	findings, err := checkSource(token.NewFileSet(), "p.go", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Pos.Line != 15 {
		t.Fatalf("findings = %v, want line 15", findings)
	}
}

func TestCheckSourceTrustedErrorClassImport(t *testing.T) {
	cases := []struct {
		name, imports, statement string
		want                     int
	}{
		{"alias", `ec "github.com/LeXwDeX/tribunal/internal/errclass"`, `classErr := ec.Of(cause); logger.Warn("failed", "error_class", classErr)`, 0},
		{"default", `"github.com/LeXwDeX/tribunal/internal/errclass"`, `var classErr = errclass.Of(cause); logger.Warn("failed", "error_class", classErr)`, 0},
		{"other import", `errclass "example.com/other"`, `classErr := errclass.Of(cause); logger.Warn("failed", "detail", classErr)`, 1},
		{"local shadow", `ec "github.com/LeXwDeX/tribunal/internal/errclass"`, `ec := struct{ Of func(error) error }{}; classErr := ec.Of(cause); logger.Warn("failed", "detail", classErr)`, 1},
		{"local same name", ``, `errclass := struct{ Of func(error) error }{}; classErr := errclass.Of(cause); logger.Warn("failed", "detail", classErr)`, 1},
		{"local unresolved", ``, `errclass := struct{ Of func(error) missing }{}; classErr := errclass.Of(cause); logger.Warn("failed", "detail", classErr)`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := `package p
import (` + tc.imports + `)
func f(logger interface{ Warn(string, ...any) }, cause error) { ` + tc.statement + ` }`
			findings, err := checkSource(token.NewFileSet(), "p.go", []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != tc.want {
				t.Fatalf("findings = %v, want %d", findings, tc.want)
			}
		})
	}
}

func TestCheckSourceLogArgumentPositions(t *testing.T) {
	cases := []struct {
		statement string
		want      int
	}{
		{`logger.Info("error", "count", 1)`, 0},
		{`logger.WarnContext(nil, "error", "count", 1)`, 0},
		{`logger.Log(nil, 0, "error", "count", 1)`, 0},
		{`logger.LogAttrs(nil, 0, "error", slog.String("count", "1"))`, 0},
		{`logger.Info("x", "status", "error", "count", 1)`, 0},
		{`logger.Info("x", "status", "error", "detail", err)`, 1},
		{`logger.Info("x", slog.String("status", "ok"), "status", "error", "count", 1)`, 0},
		{`logger.With("status", "error").Info("x")`, 0},
		{`logger.Warn("msg", "error", err)`, 1},
		{`logger.WarnContext(nil, "msg", "error", err)`, 1},
		{`logger.Log(nil, 0, "msg", "error", err)`, 1},
		{`logger.With("error", err).Info("x")`, 1},
		{`logger.Info("x", slog.String("status", "ok"), "error", err)`, 1},
		{`logger.LogAttrs(nil, 0, "msg", slog.Any("error", err))`, 1},
		{`logger.Info("error", err)`, 1},
	}
	for _, tc := range cases {
		t.Run(tc.statement, func(t *testing.T) {
			src := `package p
import "log/slog"
func f(logger *slog.Logger, err error) { ` + tc.statement + ` }`
			findings, err := checkSource(token.NewFileSet(), "p.go", []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if len(findings) != tc.want {
				t.Fatalf("findings = %v, want %d", findings, tc.want)
			}
			if tc.statement == `logger.Info("error", err)` && findings[0].Reason != "logger call records a raw error" {
				t.Fatalf("findings = %v, want raw error rather than message key", findings)
			}
		})
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

func TestCheckSourceAllowsOrdinaryValues(t *testing.T) {
	src := `package p
import "log/slog"
func f(logger *slog.Logger, err, cause, delErr error, reason string, count, projectID int) {
	logger.Warn("startup recovery failed", "error_class", errclass.Of(err))
	logger.Warn("workspace operation failed", "project_id", projectID, "error_class", workspaceErrorClass(cause))
	logger.Warn("webhook removal failed during project delete", "status", gitlab.StatusOf(delErr))
	logger.Info("job leases recovered", "count", count, "reason", reason)
	logger.Info("x", "count", "20", "reason", "rejected")
	logger.Info("x", slog.Any("detail", errclass.Of(err)))
}
func g(logger *slog.Logger, err, closeErr string) {
	logger.Info("x", "reason", err, slog.Any("detail", closeErr))
}
func h(logger *slog.Logger) {
	countErr, indexErr := 0, ""
	logger.Info("job leases recovered", "count", countErr, "reason", indexErr, slog.Any("detail", indexErr))
}`
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
	writeFile(t, filepath.Join(dir, "b.go"), "package a\n\nfunc g(l interface{ Warn(string, ...any) }, err error) { l.Warn(\"x\", \"detail\", err) }\n")
	writeFile(t, filepath.Join(dir, "c.go"), "package a\nimport \"log/slog\"\nfunc h(l interface{ Warn(string, ...any) }, err error) { l.Warn(\"x\", slog.Any(\"detail\", err)) }\n")
	writeFile(t, filepath.Join(dir, "d.go"), "package a\nimport \"errors\"\nfunc h(l interface{ Warn(string, ...any) }) { problem := errors.New(\"fake-test-error\"); l.Warn(\"failed\", \"detail\", problem) }\n")
	writeFile(t, filepath.Join(dir, "a_test.go"), "package a\n\nfunc g(l interface{ Warn(string, ...any) }, err error) { l.Warn(\"x\", \"error\", err) }\n")
	var stdout, stderr bytes.Buffer
	if code := run(dir, &stdout, &stderr); code != 1 {
		t.Fatalf("run = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), "a.go:3:") || !strings.Contains(stderr.String(), "b.go:3:") || !strings.Contains(stderr.String(), "c.go:3:") || !strings.Contains(stderr.String(), "d.go:3:") || strings.Contains(stderr.String(), "a_test.go") {
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
