package review

import (
	"context"
	"errors"
	"fmt"
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
