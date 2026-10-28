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
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type contextCanaryProvider struct{ tools, calls int }

func (p *contextCanaryProvider) BoundToolNames(context.Context, string) ([]string, error) {
	return []string{"mcp__graph__search"}, nil
}
func (p *contextCanaryProvider) Tools(context.Context, string) ([]agent.Tool, error) {
	p.tools++
	return []agent.Tool{p}, nil
}
func (p *contextCanaryProvider) Def() llm.ToolDef { return llm.ToolDef{Name: "mcp__graph__search"} }
func (p *contextCanaryProvider) Call(context.Context, json.RawMessage) (agent.Result, error) {
	p.calls++
	return agent.Result{Content: "OLD_INDEX_CANARY"}, nil
}

func TestRefreshContextUsesOnlyMatchingIndex(t *testing.T) {
	for _, tc := range []struct {
		name             string
		reindex          bool
		command          []string
		index, lastError string
		wantTools        int
	}{
		{"skip-old-index", false, nil, strings.Repeat("a", 40), "", 0},
		{"failed-old-index", true, []string{"/definitely/not/a/command"}, strings.Repeat("a", 40), "", 0},
		{"failed-same-index", true, []string{"/definitely/not/a/command"}, reviewTestHead, "", 0},
		{"skipped-previous-failure", false, nil, reviewTestHead, store.WorkspaceIndexFailed, 0},
		{"no-command", true, nil, reviewTestHead, store.WorkspaceIndexFailed, 0},
		{"skip-valid-index", false, nil, reviewTestHead, "", 1},
		{"new-index", true, []string{indexTrueCommand}, strings.Repeat("a", 40), "", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: strings.Repeat("a", 40), IndexSHA: tc.index, LastError: tc.lastError}); err != nil {
				t.Fatal(err)
			}
			p := &contextCanaryProvider{}
			runner := func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				res, err := onboardingIndexRunner(ctx, spec)
				if err != nil {
					return nil, err
				}
				for _, tool := range spec.Tools {
					if tool.Def().Name == "mcp__graph__search" {
						result, err := tool.Call(ctx, nil)
						if err != nil {
							return nil, err
						}
						res.Output.(*review.OnboardingOut).Overview = result.Content
					}
				}
				return res, nil
			}
			svc := newOnboardingContextService(t, f, runner)
			svc.reviewDefaults.Ignore = nil
			svc.tools = p
			svc.blocks = map[string]config.BlockBinding{review.BlockOnboarding: {MCPTools: []string{"mcp__graph__search"}}}
			setIndexFactory(svc, indexTestManager(t, tc.command))
			if err := svc.RefreshWithOptions(ctx, 42, "main", WorkspaceRefreshOptions{Reindex: contextBoolPtr(tc.reindex), RegenerateContext: contextBoolPtr(true)}); err != nil {
				t.Fatal(err)
			}
			doc, err := f.db.GetCurrentContextDoc(ctx, 42, "agents")
			if err != nil {
				t.Fatal(err)
			}
			if p.tools != tc.wantTools || p.calls != tc.wantTools || strings.Contains(doc.Content, "OLD_INDEX_CANARY") != (tc.wantTools > 0) {
				t.Fatalf("tools=%d calls=%d content=%q", p.tools, p.calls, doc.Content)
			}
			if tc.name == "failed-same-index" {
				if err := svc.RefreshWithOptions(ctx, 42, "main", WorkspaceRefreshOptions{Reindex: contextBoolPtr(false), RegenerateContext: contextBoolPtr(true)}); err != nil {
					t.Fatal(err)
				}
				if p.tools != 0 || p.calls != 0 {
					t.Fatalf("failed index became usable after skipped refresh: tools=%d calls=%d", p.tools, p.calls)
				}
			}
			unlock, err := f.manager.Get(42).RLock(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, eligible, indexErr := f.service.reviewIndexState(ctx, 42, reviewTestHead, nil)
			unlock()
			if indexErr != nil || eligible != (tc.wantTools > 0) {
				t.Fatalf("final review index eligibility=%v err=%v", eligible, indexErr)
			}
			if tc.wantTools == 0 {
				f.gitlab.version.StartSHA = reviewTestHead
				f.service.reviewDefaults.Ignore, f.publisher.reviewDefaults.Ignore = nil, nil
				job := f.seedReview(t, "context-canary-review")
				if err := f.service.RunReview(ctx, job); err != nil {
					t.Fatal(err)
				}
				wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
				if err != nil || strings.Contains(string(record.Data), "OLD_INDEX_CANARY") {
					t.Fatalf("MR leaked canary: %v", err)
				}
			}
		})
	}
}

func TestOnboardContextRequiresSuccessfulIndex(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command []string
		want    int
	}{
		{"no-command", nil, 0},
		{"failed", []string{"/definitely/not/a/command"}, 0},
		{"indexed", []string{indexTrueCommand}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			p := &contextCanaryProvider{}
			runner := func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				for _, tool := range spec.Tools {
					if tool.Def().Name == "mcp__graph__search" {
						if _, err := tool.Call(ctx, nil); err != nil {
							return nil, err
						}
					}
				}
				return onboardingIndexRunner(ctx, spec)
			}
			svc := newOnboardingContextService(t, f, runner)
			svc.reviewDefaults.Ignore = nil
			svc.tools = p
			svc.blocks = map[string]config.BlockBinding{review.BlockOnboarding: {MCPTools: []string{"mcp__graph__search"}}}
			setIndexFactory(svc, indexTestManager(t, tc.command))
			if err := svc.Onboard(context.Background(), 42); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.GetCurrentContextDoc(context.Background(), 42, "agents"); err != nil {
				t.Fatal(err)
			}
			if p.tools != tc.want || p.calls != tc.want {
				t.Fatalf("tools=%d calls=%d", p.tools, p.calls)
			}
		})
	}
}

func TestTerminalRetryDoesNotPrepareWorkspace(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		t.Run(status, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			target := strings.Repeat("2", 40)
			base := f.manager.Get(42).BaseDir()
			gitDir := filepath.Join(f.manager.Get(42).MirrorDir(), "worktrees", "base")
			if err := os.MkdirAll(base, 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(gitDir, 0700); err != nil {
				t.Fatal(err)
			}
			for path, data := range map[string]string{filepath.Join(base, ".git"): "gitdir: " + gitDir, filepath.Join(gitDir, "gitdir"): filepath.Join(base, ".git"), filepath.Join(gitDir, "HEAD"): target} {
				if err := os.WriteFile(path, []byte(data), 0600); err != nil {
					t.Fatal(err)
				}
			}
			f.git.dirHeads[base] = target
			if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: target, IndexSHA: target}); err != nil {
				t.Fatal(err)
			}
			f.service.reviewDefaults.Ignore, f.publisher.reviewDefaults.Ignore = nil, nil
			if status == "failed" {
				f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
			}
			f.gitlab.noteErr = errors.New("synthetic note failure")
			job := f.seedReview(t, "terminal-snapshot")
			if err := f.service.RunReview(ctx, job); err == nil {
				t.Fatal("expected publication failure")
			}
			f.gitlab.noteErr = nil
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil || wf.Status != status {
				t.Fatalf("workflow=%+v err=%v", wf, err)
			}
			before, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			var dossier review.Dossier
			if err := json.Unmarshal(before.Data, &dossier); err != nil || dossier.BaseDir != base || !dossier.MCPIndexEligible() {
				t.Fatalf("fixture lacks verified base and index: base=%q provenance=%+v err=%v", dossier.BaseDir, dossier.WorkspaceProvenance, err)
			}
			snapshot, err := f.engine.Snapshot(ctx, wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			calls := f.runner.count()
			if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: strings.Repeat("3", 40), IndexSHA: strings.Repeat("3", 40)}); err != nil {
				t.Fatal(err)
			}
			f.git.dirHeads[base] = strings.Repeat("3", 40)
			f.service.git = failingWorkspaceGit{GitOps: f.git, err: errors.New("fetch forbidden")}
			if err := f.service.Handle(ctx, job); err != nil {
				t.Fatal(err)
			}
			after, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil || !bytes.Equal(before.Data, after.Data) || after.PublicationState != store.WorkflowPublicationPublished {
				t.Fatalf("record=%+v err=%v", after, err)
			}
			current, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil || current.ID != wf.ID {
				t.Fatalf("workflow=%+v err=%v", current, err)
			}
			final, err := f.engine.Snapshot(ctx, wf.ID)
			if err != nil || len(final.Nodes) != len(snapshot.Nodes) || f.runner.count() != calls {
				t.Fatalf("nodes changed or agent reran: %v", err)
			}
			for id, old := range snapshot.Nodes {
				if now := final.Nodes[id]; old.Status != now.Status || old.Attempt != now.Attempt || !bytes.Equal(old.Output, now.Output) {
					t.Fatalf("node %s changed", id)
				}
			}
		})
	}
}
