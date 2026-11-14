package review

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func errorClass(t *testing.T, err error) string {
	t.Helper()
	var classified interface{ ErrorClass() string }
	if !errors.As(err, &classified) {
		t.Fatalf("error %v has no class", err)
	}
	return classified.ErrorClass()
}

func TestExecutorClassifiesProviderError(t *testing.T) {
	finding := FindingOut{
		Path: "a.go", Line: 3, Severity: SeverityP1, Category: "correctness",
		Title: "bug", Body: "body", Evidence: "evidence", Confidence: ConfidenceHigh,
	}
	params, err := encodeParams(verifyParams{GroupID: "g1", Finding: finding, Provider: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		kind llm.ErrKind
		want string
	}{
		{name: "auth", kind: llm.ErrAuth, want: "auth"},
		{name: "rate limited", kind: llm.ErrRateLimited, want: "rate_limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			executor := newTestExecutor(t, ExecutorDeps{
				Dossier:     validDossier(t),
				MaxAttempts: 1,
				AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
					return &agent.RunResult{Usage: llm.Usage{InputTokens: 7, OutputTokens: 2}}, &llm.Error{Kind: tc.kind}
				},
			})
			out, err := executor.Execute(context.Background(), dag.NodeInput{
				WorkflowID: "wf",
				Node:       dag.NodeSpec{ID: "verify.1", Kind: KindVerify, Params: params},
			})
			if err == nil {
				t.Fatal("Execute succeeded, want provider failure")
			}
			if got := errorClass(t, err); got != tc.want {
				t.Fatalf("error class = %q, want %q", got, tc.want)
			}
			if out.Usage.InputTokens != 7 || out.Usage.OutputTokens != 2 {
				t.Fatalf("usage = %+v, want failed attempt usage", out.Usage)
			}
		})
	}
}

func TestClassifyErrorMapsSandboxDenial(t *testing.T) {
	for _, sentinel := range []error{agent.ErrUnsafePath, agent.ErrIgnoredPath} {
		err := classifyError(fmt.Errorf("review: read evidence: %w", sentinel))
		if got := errorClass(t, err); got != sandboxDeniedClass {
			t.Fatalf("%v class = %q, want %q", sentinel, got, sandboxDeniedClass)
		}
		if !errors.Is(err, sentinel) {
			t.Fatalf("%v chain was lost", sentinel)
		}
	}
}

func TestClassifyErrorLeavesUnknownGeneric(t *testing.T) {
	err := classifyError(errors.New("review: unknown failure"))
	var classified interface{ ErrorClass() string }
	if errors.As(err, &classified) {
		t.Fatalf("unknown error gained class %q", classified.ErrorClass())
	}
}

func errorDetail(t *testing.T, err error) string {
	t.Helper()
	var detailed interface{ ErrorDetail() string }
	if !errors.As(err, &detailed) {
		t.Fatalf("error %v has no detail", err)
	}
	return detailed.ErrorDetail()
}

// The five run failures that used to collapse into executor_error each keep a
// fixed, code-owned class and a sanitized detail.
func TestClassifyErrorFixedFailureClasses(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantClass  string
		wantDetail string
	}{
		{"no submit", agent.ErrNoSubmit, "no_submit", "submit_result missing"},
		{"invalid submit", agent.ErrInvalidSubmit, "invalid_submit", "submit_result rejected"},
		{"context too long", &llm.Error{Kind: llm.ErrContextTooLong}, "context_too_long", "model context window exceeded"},
		{"token budget", errTokenBudgetExhausted, "budget_exhausted", "workflow token budget exhausted"},
		{
			name: "token budget with limits", err: &tokenBudgetError{used: 1950000, max: 2000000},
			wantClass: "budget_exhausted", wantDetail: "workflow token budget exhausted (1950000 of 2000000 tokens)",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := classifyError(fmt.Errorf("review: node rv.p1: %w", tc.err))
			if got := errorClass(t, err); got != tc.wantClass {
				t.Fatalf("class = %q, want %q", got, tc.wantClass)
			}
			if got := errorDetail(t, err); got != tc.wantDetail {
				t.Fatalf("detail = %q, want %q", got, tc.wantDetail)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("classifying lost the original error chain")
			}
		})
	}
}

func TestClassifyErrorReportsPartialRunTurns(t *testing.T) {
	partial := &PartialRunError{
		Class:      noSubmitClass,
		Turns:      30,
		Usage:      dag.Usage{InputTokens: 1950000, OutputTokens: 1200},
		Incomplete: true,
		Result:     &agent.RunResult{Turns: 30, Partial: true},
		err:        agent.ErrNoSubmit,
	}
	err := classifyError(fmt.Errorf("review: node rv.p1: %w", partial))
	if got := errorClass(t, err); got != noSubmitClass {
		t.Fatalf("class = %q, want %q", got, noSubmitClass)
	}
	if got := errorDetail(t, err); got != "submit_result missing after 30 turns" {
		t.Fatalf("detail = %q, want the turn count", got)
	}
	if !errors.Is(err, agent.ErrNoSubmit) {
		t.Fatal("the partial error chain was lost")
	}
	if knownUsage(err).InputTokens != 1950000 || !knownUsageIncomplete(err) {
		t.Fatalf("partial usage = %+v incomplete=%t", knownUsage(err), knownUsageIncomplete(err))
	}
}

func TestPartialRunErrorDetailsForFixedClasses(t *testing.T) {
	for _, tc := range []struct {
		name      string
		err       *PartialRunError
		wantClass string
		want      string
	}{
		{
			name: "invalid submit", err: &PartialRunError{Class: invalidSubmitClass, Turns: 2, err: agent.ErrInvalidSubmit},
			wantClass: invalidSubmitClass, want: "submit_result rejected after 2 turns",
		},
		{
			name: "context", err: &PartialRunError{Class: contextTooLongClass, err: &llm.Error{Kind: llm.ErrContextTooLong}},
			wantClass: contextTooLongClass, want: "model context window exceeded",
		},
		{
			name: "budget", err: &PartialRunError{Class: tokenBudgetClass, err: &tokenBudgetError{used: 9, max: 10}},
			wantClass: tokenBudgetClass, want: "workflow token budget exhausted (9 of 10 tokens)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			classified := classifyError(tc.err)
			if got := errorClass(t, classified); got != tc.wantClass {
				t.Fatalf("class = %q, want %q", got, tc.wantClass)
			}
			if got := errorDetail(t, classified); got != tc.want {
				t.Fatalf("detail = %q, want %q", got, tc.want)
			}
		})
	}
}

// The WARN log's class never falls back to raw error text and keeps the same
// precedence as the durable failure event.
func TestFailureClassForLogs(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"sandbox", agent.ErrUnsafePath, sandboxDeniedClass},
		{"provider", &llm.Error{Kind: llm.ErrAuth}, "auth"},
		{"no submit", agent.ErrNoSubmit, noSubmitClass},
		{"unknown", errors.New("raw upstream canary"), "executor_error"},
		{"nil", nil, "executor_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := failureClass(tc.err); got != tc.want {
				t.Fatalf("failureClass = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExecuteClassifiesIgnoredModelOutput(t *testing.T) {
	head := t.TempDir()
	if err := os.WriteFile(filepath.Join(head, "private.txt"), []byte("CANARY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	d := validDossier(t)
	d.HeadDir = head
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"private.txt"}}
	runner := func(_ context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		decoded, err := spec.Submit.Decode(json.RawMessage(`{"summary":"ok","findings":[{"path":"private.txt","line":1,"severity":"P1","category":"correctness","title":"x","body":"x","evidence":"x","confidence":"high"}]}`))
		return &agent.RunResult{Output: decoded}, err
	}
	e := newTestExecutor(t, ExecutorDeps{Dossier: d, AgentRun: runner})

	_, err := e.Execute(context.Background(), codeReviewInput(t))
	var classified *classifiedError
	if !errors.As(err, &classified) || classified.ErrorClass() != sandboxDeniedClass {
		t.Fatalf("error = %v, want a %s classification", err, sandboxDeniedClass)
	}
	if strings.Contains(err.Error(), "private.txt") {
		t.Fatalf("error leaked an ignored path: %v", err)
	}
}
