package review

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestMCPIndexEvidenceIsRequiredForMR(t *testing.T) {
	for _, tc := range []struct {
		name                string
		change              func(*Dossier)
		wantCalls, wantGaps int
	}{
		{"valid", func(*Dossier) {}, 1, 0},
		{"legacy", func(d *Dossier) { d.WorkspaceProvenance = nil }, 0, 1},
		{"wrong-version", func(d *Dossier) { d.WorkspaceProvenance.Version = 2 }, 0, 1},
		{"wrong-target", func(d *Dossier) { d.WorkspaceProvenance.TargetSHA = testBaseSHA }, 0, 1},
		{"wrong-index", func(d *Dossier) { d.WorkspaceProvenance.IndexSHA = testBaseSHA }, 0, 1},
		{"invalid-index", func(d *Dossier) { d.WorkspaceProvenance.IndexSHA = "bad" }, 0, 1},
		{"blocked", func(d *Dossier) { d.WorkspaceProvenance.MCPEligible = false }, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := validDossier(t)
			d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{}}
			tc.change(d)
			provider := &ignoreMetadataProvider{names: []string{"mcp__graph__search"}}
			var seen []string
			e := newTestExecutor(t, ExecutorDeps{Dossier: d, Blocks: graphBlocks, Tools: provider, AgentRun: submitCodeReview(`{"summary":"ok","findings":[]}`, &seen)})
			out, err := e.Execute(context.Background(), codeReviewInput(t))
			if err != nil {
				t.Fatal(err)
			}
			result, err := DecodeStrict[*CodeReviewOut](out.Data)
			if err != nil || provider.calls != tc.wantCalls || len(result.ToolGaps) != tc.wantGaps {
				t.Fatalf("calls=%d gaps=%v err=%v", provider.calls, result.ToolGaps, err)
			}
		})
	}
}

func TestOnboardingMCPRequiresIndexEvidence(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mr        int64
		wantCalls int
		wantError bool
		valid     bool
	}{
		{"onboarding-valid", 0, 1, false, true},
		{"onboarding-unproven", 0, 0, false, false},
		{"mr", 7, 0, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &Dossier{MR: MRMeta{IID: tc.mr}, IgnorePolicy: &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{}}}
			if tc.valid {
				d.DiffVersion = gitlab.Version{StartSHA: testStartSHA}
				d.WorkspaceProvenance = &WorkspaceProvenance{Version: WorkspaceProvenanceVersion, TargetSHA: testStartSHA, IndexSHA: testStartSHA, MCPEligible: true}
			}
			provider := &ignoreMetadataProvider{names: []string{"mcp__graph__search"}}
			e := newTestExecutor(t, ExecutorDeps{Dossier: d, Blocks: map[string]config.BlockBinding{BlockOnboarding: {MCPTools: []string{"mcp__graph__search"}}}, Tools: provider, AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return &agent.RunResult{}, nil }})
			role, _ := roleForKind(KindOnboarding)
			_, err := e.callAgent(context.Background(), dag.NodeInput{}, agentOptions{Role: role, Provider: "test", Block: BlockOnboarding, BlockTools: true, Submit: agent.SubmitSpec{Schema: json.RawMessage(`{}`), Decode: func(json.RawMessage) (any, error) { return nil, nil }}})
			if (err != nil) != tc.wantError || provider.calls != tc.wantCalls {
				t.Fatalf("calls=%d err=%v", provider.calls, err)
			}
		})
	}
}

func TestExecutorRejectsUnprovenContextBeforeAgent(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*Dossier)
	}{
		{"missing", func(d *Dossier) { d.WorkspaceProvenance = nil }},
		{"wrong-target", func(d *Dossier) { d.WorkspaceProvenance.ContextBaseSHA = testBaseSHA }},
		{"ignored", func(d *Dossier) {
			d.WorkspaceProvenance.ContextBaseSHA = testStartSHA
			d.IgnorePolicy.Patterns = []string{"private"}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := validDossier(t)
			d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{}}
			d.ContextDocs = "CANARY"
			tc.change(d)
			calls := 0
			e := newTestExecutor(t, ExecutorDeps{Dossier: d, AgentRun: func(context.Context, agent.RunSpec) (*agent.RunResult, error) { calls++; return nil, nil }})
			_, err := e.Execute(context.Background(), codeReviewInput(t))
			if err == nil || calls != 0 {
				t.Fatalf("err=%v agent calls=%d", err, calls)
			}
		})
	}
}
