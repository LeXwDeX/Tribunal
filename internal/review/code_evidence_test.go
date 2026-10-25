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

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

// fixedSnapshot writes a tiny fixed head and base. The executor must validate
// evidence against these directories, never against a moving checkout.
func fixedSnapshot(t *testing.T) (string, string) {
	t.Helper()
	parent := t.TempDir()
	head := filepath.Join(parent, "head")
	base := filepath.Join(parent, "base")
	if err := os.MkdirAll(head, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(base, 0o755); err != nil {
		t.Fatal(err)
	}
	lines := strings.Repeat("line\n", 20)
	if err := os.WriteFile(filepath.Join(head, "a.go"), []byte(lines), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "a.go"), []byte(strings.Repeat("old\n", 5)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(head, "outside.go"), []byte("package outside\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "secret.go")
	if err := os.WriteFile(outside, []byte("package secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(head, "escape.go")); err != nil {
		t.Fatal(err)
	}
	return head, base
}

func TestAdjudicateRequiresSnapshotValidEvidence(t *testing.T) {
	group := adjudicateGroup("g", SeverityP1, ChallengeRefuted, []CodeRef{{
		Path: "a.go", StartLine: 1, EndLine: 2, Root: "head", Note: "exists",
	}})
	validator := func(ref CodeRef) error {
		if ref.Path == "absent.go" {
			return errors.New("missing")
		}
		return nil
	}
	result, err := Adjudicate([]Group{group}, AdjudicateInput{
		ChallengeOnly: []Severity{SeverityP1},
		CodeEvidence:  validator,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Groups[0].Decision == nil || result.Groups[0].Decision.Action != ActionDrop {
		t.Fatalf("validated refutation was not dropped: %+v", result.Groups[0].Decision)
	}

	invalid := adjudicateGroup("g2", SeverityP1, ChallengeRefuted, []CodeRef{{
		Path: "absent.go", StartLine: 1, EndLine: 1, Note: "invented",
	}})
	result, err = Adjudicate([]Group{invalid}, AdjudicateInput{
		ChallengeOnly: []Severity{SeverityP1},
		CodeEvidence:  validator,
		OnUncertain:   OnUncertainMark,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Groups[0].Decision == nil || result.Groups[0].Decision.Action != ActionMark {
		t.Fatalf("unverifiable refutation dropped the finding: %+v", result.Groups[0].Decision)
	}
	if result.Groups[0].Challenge.Status != ChallengeUncertain {
		t.Fatalf("challenge = %+v, want uncertain", result.Groups[0].Challenge)
	}
}

func TestExecutorEvidenceUsesFixedSnapshot(t *testing.T) {
	head, base := fixedSnapshot(t)
	dossier := validDossier(t)
	dossier.HeadDir = head
	dossier.BaseDir = base
	executor := newTestExecutor(t, ExecutorDeps{Dossier: dossier})
	if err := executor.validateCodeRef(CodeRef{Path: "a.go", StartLine: 1, EndLine: 20, Root: "head", Note: "ok"}); err != nil {
		t.Fatalf("head range: %v", err)
	}
	if err := executor.validateCodeRef(CodeRef{Path: "a.go", StartLine: 5, EndLine: 5, Root: "base", Note: "ok"}); err != nil {
		t.Fatalf("base range: %v", err)
	}
	if err := executor.validateCodeRef(CodeRef{Path: "a.go", StartLine: 1, EndLine: 6, Root: "base", Note: "too long"}); err == nil {
		t.Fatal("base line range beyond the fixed snapshot was accepted")
	}
	if err := executor.validateCodeRef(CodeRef{Path: "missing.go", StartLine: 1, EndLine: 1, Note: "missing"}); err == nil {
		t.Fatal("missing path was accepted")
	}
	if err := executor.validateCodeRef(CodeRef{Path: "a.go", StartLine: 21, EndLine: 21, Note: "outside"}); err == nil {
		t.Fatal("out-of-range line was accepted")
	}
	if err := executor.validateCodeRef(CodeRef{Path: "a.go", StartLine: 1, EndLine: 1, Root: "other", Note: "bad root"}); err == nil {
		t.Fatal("unknown root was accepted")
	}
	if err := executor.validateCodeRef(CodeRef{Path: "escape.go", StartLine: 1, EndLine: 1, Root: "head", Note: "symlink escape"}); err == nil {
		t.Fatal("symlink escape was accepted as evidence")
	}
	if err := executor.validateCodeRef(CodeRef{Path: "outside.go", StartLine: 1, EndLine: 1, Root: "base", Note: "diff outside"}); err == nil {
		t.Fatal("file outside the selected snapshot was accepted")
	}
	// A diff-external file in the selected snapshot is legitimate evidence.
	if err := executor.validateCodeRef(CodeRef{Path: "outside.go", StartLine: 1, EndLine: 1, Root: "head", Note: "diff outside"}); err != nil {
		t.Fatalf("diff-outside snapshot evidence was rejected: %v", err)
	}
}

func TestExecutorEvidenceFailsClosedWithoutFixedSnapshot(t *testing.T) {
	executor := newTestExecutor(t, ExecutorDeps{Dossier: validDossier(t)})
	if err := executor.validateCodeRef(CodeRef{Path: "a.go", StartLine: 1, EndLine: 1, Note: "no snapshot"}); err == nil {
		t.Fatal("validator accepted evidence without a fixed snapshot")
	}
	group := adjudicateGroup("g", SeverityP1, ChallengeRefuted, []CodeRef{{
		Path: "a.go", StartLine: 1, EndLine: 1, Note: "syntactically valid only",
	}})
	result, err := Adjudicate([]Group{group}, AdjudicateInput{
		ChallengeOnly: []Severity{SeverityP1},
		CodeEvidence:  executor.validateCodeRef,
		OnUncertain:   OnUncertainMark,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Groups[0].Decision.Action != ActionMark {
		t.Fatalf("action = %s, want MARK; evidence must fail closed", result.Groups[0].Decision.Action)
	}
}

func TestExecutorSynthesisRevalidatesMainFindingEvidence(t *testing.T) {
	head, base := fixedSnapshot(t)
	dossier := validDossier(t)
	dossier.HeadDir = head
	dossier.BaseDir = base
	profile := defaultProfile()
	profile.Config.Synthesize.Mode = "agent"
	profile.Config.Synthesize.Provider = "claude"
	spec, err := BuildGraph(profile, dossier, GraphConfig{AllowedProviders: []string{"local", "claude"}})
	if err != nil {
		t.Fatal(err)
	}
	group := mainFindingGroup("main")
	group.Challenge = &Challenge{
		Status:   ChallengeRefuted,
		Evidence: []CodeRef{{Path: "missing.go", StartLine: 1, EndLine: 1, Note: "invented"}},
	}
	group.Decision = &FindingDecision{Action: ActionMark, Reason: "unverified"}
	adjudicatedRaw, _ := json.Marshal(AdjudicateOut{Groups: []Group{group}})
	output := SynthesizeOut{
		Verdict:        VerdictBlocking,
		Summary:        "settled",
		Decisions:      []Decision{{FindingID: group.ID, Action: "mark", Reason: "cannot verify"}},
		NewFindings:    []FindingOut{},
		ExtendRequests: []ExtendRequest{},
	}
	outputRaw, _ := json.Marshal(output)
	executor := newTestExecutor(t, ExecutorDeps{
		Dossier: dossier,
		Profile: profile,
		Snapshot: func(context.Context, string) (dag.WorkflowView, error) {
			wf := dag.WorkflowView{
				ID:    "wf-main-evidence",
				Spec:  spec,
				Nodes: map[string]dag.NodeView{NodeAdjudicate: {ID: NodeAdjudicate, Spec: mustNode(t, spec, NodeAdjudicate), Status: dag.NodeCompleted, Output: adjudicatedRaw}},
			}
			return wf, nil
		},
		AgentRun: func(_ context.Context, runSpec agent.RunSpec) (*agent.RunResult, error) {
			decoded, err := runSpec.Submit.Decode(outputRaw)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", agent.ErrInvalidSubmit, err)
			}
			return &agent.RunResult{Output: decoded}, nil
		},
	})
	in := dag.NodeInput{
		WorkflowID: "wf-main-evidence",
		Node:       mustNode(t, spec, NodeSynth),
	}
	_, err = executor.Execute(context.Background(), in)
	if err == nil {
		t.Fatal("synthesis accepted a main finding with invented refutation evidence")
	}
}
