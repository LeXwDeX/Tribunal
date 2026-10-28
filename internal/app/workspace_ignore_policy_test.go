package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/mcp"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

type ignoreCountingTools struct{ calls int }

func (p *ignoreCountingTools) Tools(context.Context, string) ([]agent.Tool, error) {
	p.calls++
	return nil, nil
}

func TestNewReviewEffectiveIgnoreBlocksToolsAndStaysPass(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	if _, err := f.db.UpdateProject(context.Background(), 42, store.ProjectUpdate{Settings: json.RawMessage(`{"ignore":["project.txt"]}`)}); err != nil {
		t.Fatal(err)
	}
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [private.txt, AGENTS.md]\n"
	f.gitlab.docs["AGENTS.md"] = "CANARY_DOC"
	provider := &ignoreCountingTools{}
	f.service.tools = provider
	f.service.blocks = map[string]config.BlockBinding{review.BlockCodeReview: {MCPTools: []string{"mcp__graph__search"}}}
	called := 0
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		called++
		for _, b := range spec.System {
			if strings.Contains(b.Text, "CANARY_DOC") {
				t.Error("ignored document reached model")
			}
		}
		for _, tool := range spec.Tools {
			if tool.Def().Name == "read_file" || tool.Def().Name == "grep" {
				ws := f.manager.Get(42)
				entries, err := os.ReadDir(filepath.Join(ws.Root, "runs"))
				if err != nil || len(entries) != 1 {
					t.Fatalf("runs: %v %v", entries, err)
				}
				head := filepath.Join(ws.Root, "runs", entries[0].Name())
				if err := os.WriteFile(filepath.Join(head, "private.txt"), []byte("CANARY_FILE\n"), 0600); err != nil {
					t.Fatal(err)
				}
				input := json.RawMessage(`{"path":"private.txt"}`)
				if tool.Def().Name == "grep" {
					input = json.RawMessage(`{"pattern":"CANARY"}`)
				}
				res, err := tool.Call(ctx, input)
				if tool.Def().Name == "read_file" && err == nil {
					t.Errorf("ignored read = %q", res.Content)
				}
				if tool.Def().Name == "grep" && (err != nil || strings.Contains(res.Content, "CANARY_FILE")) {
					t.Errorf("ignored grep = %q %v", res.Content, err)
				}
			}
		}
		return &agent.RunResult{Output: &review.CodeReviewOut{Summary: "ok", Findings: []review.FindingOut{}}}, nil
	}
	job := f.seedReview(t, "new-ignore-policy")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if called == 0 || provider.calls != 0 || wf.Verdict != review.VerdictPass {
		t.Fatalf("calls=%d MCP=%d verdict=%s", called, provider.calls, wf.Verdict)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatal(err)
	}
	if dossier.IgnorePolicy == nil || strings.Join(dossier.IgnorePolicy.Patterns, ",") != "**/*.lock,project.txt,private.txt,AGENTS.md" {
		t.Fatalf("policy = %+v", dossier.IgnorePolicy)
	}
}

func TestNewReviewIgnoreWithoutMCPBindingStaysPass(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.Ignore = []string{"private.txt"}
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	provider := &ignoreCountingTools{}
	f.service.tools = provider
	job := f.seedReview(t, "ignore-no-mcp")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil || provider.calls != 0 || wf.Verdict != review.VerdictPass {
		t.Fatalf("workflow=%+v MCP=%d err=%v", wf, provider.calls, err)
	}
}

func TestOnboardingIgnorePolicyAndIndex(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ignore []string
		strict bool
	}{
		{"nonempty", []string{"private.txt", "AGENTS.md"}, true},
		{"empty", []string{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			f.defaults.Ignore = tc.ignore
			f.gitlab.docs["AGENTS.md"] = "CANARY_DOC"
			calls := 0
			runner := func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				calls++
				if err := os.WriteFile(filepath.Join(f.manager.Get(42).BaseDir(), "private.txt"), []byte("CANARY_FILE\n"), 0600); err != nil {
					t.Fatal(err)
				}
				prompt := spec.Prompt
				for _, block := range spec.System {
					prompt += block.Text
				}
				if strings.Contains(prompt, "CANARY_DOC") == tc.strict {
					t.Errorf("prompt = %q", prompt)
				}
				for _, tool := range spec.Tools {
					if tool.Def().Name == "read_file" {
						result, err := tool.Call(ctx, json.RawMessage(`{"root":"base","path":"private.txt"}`))
						if (err != nil) != tc.strict {
							t.Errorf("read=%q err=%v", result.Content, err)
						}
					}
				}
				return onboardingIndexRunner(ctx, spec)
			}
			service := newOnboardingContextService(t, f, runner)
			manager := indexTestManager(t, []string{indexTrueCommand})
			indexCalls := 0
			service.mcpFactory = func(context.Context, int64) (*mcp.Manager, error) { indexCalls++; return manager, nil }
			if err := service.Onboard(context.Background(), 42); err != nil {
				t.Fatal(err)
			}
			state, err := f.db.GetWorkspace(context.Background(), 42)
			if err != nil {
				t.Fatal(err)
			}
			if calls != 1 || (indexCalls == 0) != tc.strict {
				t.Fatalf("runner=%d index=%d state=%+v", calls, indexCalls, state)
			}
			if tc.strict && (state.IndexSHA != "" || !strings.Contains(state.LastError, "ignore")) {
				t.Fatalf("unsafe index state: %+v", state)
			}
			if !tc.strict && state.IndexSHA != reviewTestHead {
				t.Fatalf("empty policy index state: %+v", state)
			}
		})
	}
}

func TestRetryLegacyDossierRebuildsWithoutReusingOutput(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "legacy-policy-retry")
	configHash, promptHash := currentReviewHashes(t, f)
	dossier := testDossier(t, f.head)
	dossier.IgnorePolicy = nil
	dossier.Metadata = map[string]string{"config_hash": configHash, "prompt_hash": promptHash}
	raw, err := review.MarshalDossier(dossier)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := review.DossierHash(dossier)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := testGraph(t, dossier)
	oldID, _, err := f.db.CreateReviewWorkflow(context.Background(), sqlite.ReviewWorkflowRequest{
		JobID: job.ID, Spec: spec, Meta: map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA, "fingerprint": reviewFingerprint(hash, configHash, promptHash), "config_hash": configHash, "prompt_hash": promptHash},
		DossierVersion: review.DossierSchemaVersion, DossierHash: hash, DossierData: raw, ExpectedGeneration: jobGeneration(t, job), ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	current, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil || current.ID == oldID {
		t.Fatalf("legacy was reused: %+v %v", current, err)
	}
	old, err := f.db.GetWorkflow(context.Background(), oldID)
	if err != nil || old.Status != "cancelled" || old.PublicationState != "skipped" {
		t.Fatalf("old workflow: %+v %v", old, err)
	}
	oldDossier, err := f.db.GetWorkflowDossier(context.Background(), oldID)
	if err != nil || oldDossier.JobID != "" || oldDossier.Hash != hash || !bytes.Equal(oldDossier.Data, raw) {
		t.Fatalf("legacy dossier changed instead of detaching: %v", err)
	}
	newDossier, err := f.db.GetWorkflowDossier(context.Background(), current.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fresh review.Dossier
	if err := json.Unmarshal(newDossier.Data, &fresh); err != nil || !validPublicationPolicy(&fresh) || f.runner.count() == 0 {
		t.Fatalf("fresh workflow did not execute with an explicit policy: %v", err)
	}
}

func TestRecoverLegacyDossierCancelsWithoutRunningAgent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "legacy-policy-recover")
	configHash, promptHash := currentReviewHashes(t, f)
	dossier := testDossier(t, f.head)
	dossier.IgnorePolicy = nil
	dossier.Metadata = map[string]string{"config_hash": configHash, "prompt_hash": promptHash}
	raw, err := review.MarshalDossier(dossier)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := review.DossierHash(dossier)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := testGraph(t, dossier)
	oldID, _, err := f.db.CreateReviewWorkflow(context.Background(), sqlite.ReviewWorkflowRequest{
		JobID: job.ID, Spec: spec, Meta: map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA, "fingerprint": reviewFingerprint(hash, configHash, promptHash), "config_hash": configHash, "prompt_hash": promptHash},
		DossierVersion: review.DossierSchemaVersion, DossierHash: hash, DossierData: raw, ExpectedGeneration: jobGeneration(t, job), ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !f.executors.Acquire(job.ID) {
		t.Fatal("could not acquire job ownership")
	}
	old, err := f.db.GetWorkflow(context.Background(), oldID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.recoverWorkflow(context.Background(), *old); err != nil {
		t.Fatal(err)
	}
	owned, err := f.db.GetWorkflow(context.Background(), oldID)
	if err != nil || owned.Status != "pending" {
		t.Fatalf("recovery cancelled an owned workflow: %+v %v", owned, err)
	}
	f.executors.Release(job.ID)
	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	old, err = f.db.GetWorkflow(context.Background(), oldID)
	if err != nil || old.Status != "cancelled" || old.PublicationState != "skipped" || f.runner.count() != 0 {
		t.Fatalf("old workflow=%+v agent calls=%d err=%v", old, f.runner.count(), err)
	}
	stored, err := f.db.GetWorkflowDossier(context.Background(), oldID)
	if err != nil || stored.Hash != hash || !bytes.Equal(stored.Data, raw) {
		t.Fatalf("recovery changed legacy dossier: %v", err)
	}
}

func TestOnboardingConfigFetchFailureDoesNotRunIndexOrAgent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.rawErr[".gitlab-review.yml"] = errors.New("upstream unavailable")
	calls := 0
	service := newOnboardingContextService(t, f, func(context.Context, agent.RunSpec) (*agent.RunResult, error) { calls++; return nil, nil })
	service.mcpFactory = func(context.Context, int64) (*mcp.Manager, error) { calls++; return nil, nil }
	if err := service.Onboard(context.Background(), 42); err == nil {
		t.Fatal("onboarding succeeded without configuration")
	}
	if calls != 0 {
		t.Fatalf("unsafe calls=%d", calls)
	}
}

func TestOnboardingRepoIgnoreRetainsPreviousIndex(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.Ignore = nil
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [private.txt]\n"
	oldSHA := strings.Repeat("a", 40)
	if _, err := f.db.SaveWorkspace(context.Background(), store.Workspace{ProjectID: 42, State: "ready", BaseSHA: oldSHA, IndexSHA: oldSHA}); err != nil {
		t.Fatal(err)
	}
	service := newOnboardingContextService(t, f, onboardingIndexRunner)
	indexCalls := 0
	service.mcpFactory = func(context.Context, int64) (*mcp.Manager, error) { indexCalls++; return nil, nil }
	if err := service.Onboard(context.Background(), 42); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil || indexCalls != 0 || state.IndexSHA != oldSHA || !strings.Contains(state.LastError, "ignore") {
		t.Fatalf("state=%+v calls=%d err=%v", state, indexCalls, err)
	}
}
