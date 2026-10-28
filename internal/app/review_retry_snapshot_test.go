package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type snapshotGitLab struct {
	*reviewGitLab
	mu     sync.Mutex
	status string
}

func (g *snapshotGitLab) ListMRPipelines(context.Context, string, int64, int64) ([]gitlab.Pipeline, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return []gitlab.Pipeline{{ID: 1, Status: g.status, Ref: "test"}}, nil
}

func TestRetryUsesStoredMRMaterials(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-snapshot-materials")
	wfID := interruptReview(t, f, job)
	before, err := f.db.GetWorkflowDossier(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	f.gitlab.mu.Lock()
	f.gitlab.mr.Title = "new title after interruption"
	f.gitlab.mu.Unlock()
	var mu sync.Mutex
	var prompts []string
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		mu.Lock()
		prompts = append(prompts, spec.Prompt)
		mu.Unlock()
		return f.runner.Run(ctx, spec)
	}
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if len(prompts) == 0 {
		t.Fatal("retry did not run any agent")
	}
	for _, prompt := range prompts {
		if strings.Contains(prompt, "new title after interruption") {
			t.Fatal("retry mixed new MR title with old workflow outputs")
		}
	}
	after, err := f.db.GetWorkflowDossier(context.Background(), wfID)
	if err != nil {
		t.Fatal(err)
	}
	if string(after.Data) != string(before.Data) {
		t.Fatal("retry changed the persisted materials snapshot")
	}
}

func TestRetryKeepsPipelineAndContextSnapshot(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.reviewDefaults.Ignore = nil
	f.publisher.reviewDefaults.Ignore = nil
	api := &snapshotGitLab{reviewGitLab: f.gitlab, status: "success"}
	f.service.gitlab = api
	ctx := context.Background()
	base := strings.Repeat("2", 40)
	if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SaveManualContextDoc(ctx, store.ContextDoc{ProjectID: 42, BaseSHA: base, Kind: "agents", Content: "original-context", Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	job := f.seedReview(t, "job-pipeline-context")
	wfID := interruptReview(t, f, job)
	before, err := f.db.GetWorkflowDossier(ctx, wfID)
	if err != nil {
		t.Fatal(err)
	}
	api.mu.Lock()
	api.status = "failed"
	api.mu.Unlock()
	if _, err := f.db.SaveManualContextDoc(ctx, store.ContextDoc{ProjectID: 42, BaseSHA: base, Kind: "agents", Content: "changed-context", Source: "manual"}); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var prompts []string
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		mu.Lock()
		text := spec.Prompt
		for _, block := range spec.System {
			text += "\n" + block.Text
		}
		prompts = append(prompts, text)
		mu.Unlock()
		return f.runner.Run(ctx, spec)
	}
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if len(prompts) == 0 {
		t.Fatal("retry did not run agent")
	}
	joined := strings.Join(prompts, "\n")
	if !strings.Contains(joined, "original-context") || !strings.Contains(joined, "最新 pipeline：success") {
		t.Fatal("retry did not use the original pipeline and context")
	}
	for _, prompt := range prompts {
		if strings.Contains(prompt, "changed-context") || strings.Contains(prompt, "最新 pipeline：failed") {
			t.Fatal("retry used changed materials")
		}
	}
	after, err := f.db.GetWorkflowDossier(ctx, wfID)
	if err != nil || string(after.Data) != string(before.Data) {
		t.Fatalf("snapshot changed: %v", err)
	}
	var doc struct {
		Pipeline *struct {
			Status string `json:"status"`
		} `json:"pipeline"`
		Context string `json:"context_docs"`
	}
	if err := json.Unmarshal(before.Data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Pipeline == nil || doc.Pipeline.Status != "success" || doc.Context != "original-context" {
		t.Fatalf("original snapshot missing materials: %+v", doc)
	}
}

func TestRetryRebuildsWhenExecutionIdentityChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *reviewFixture)
	}{
		{"profile", func(_ *testing.T, f *reviewFixture) {
			p := f.profiles["default"]
			p.Plan.MaxPackages++
			f.profiles["default"] = p
		}},
		{"prompt", func(_ *testing.T, f *reviewFixture) { f.service.prompts.Hash = "changed-prompt-hash" }},
		{"base", func(_ *testing.T, f *reviewFixture) {
			f.gitlab.mu.Lock()
			defer f.gitlab.mu.Unlock()
			f.gitlab.version.BaseSHA = strings.Repeat("3", 40)
		}},
		{"target-start", func(_ *testing.T, f *reviewFixture) {
			f.gitlab.mu.Lock()
			defer f.gitlab.mu.Unlock()
			f.gitlab.version.StartSHA = strings.Repeat("4", 40)
		}},
		{"workspace", func(t *testing.T, f *reviewFixture) {
			if err := os.MkdirAll(f.manager.Get(42).BaseDir(), 0700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, fmt.Sprintf("job-identity-%s", tc.name))
			oldID := interruptReview(t, f, job)
			old, err := f.db.GetWorkflowDossier(context.Background(), oldID)
			if err != nil {
				t.Fatal(err)
			}
			tc.change(t, f)
			if err := f.service.Handle(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wf.ID == oldID {
				t.Fatal("changed execution identity reused old workflow")
			}
			after, err := f.db.GetWorkflowDossier(context.Background(), oldID)
			if err != nil {
				t.Fatal(err)
			}
			if string(after.Data) != string(old.Data) || after.JobID != "" || after.PublicationState != store.WorkflowPublicationSkipped {
				t.Fatalf("old snapshot not preserved: %+v", after)
			}
			fresh, err := f.db.GetWorkflowDossier(context.Background(), wf.ID)
			if err != nil || fresh.JobID != job.ID || fresh.Generation != old.Generation {
				t.Fatalf("new snapshot binding = %+v, %v", fresh, err)
			}
		})
	}
}

func TestRecoveryRejectsChangedExecutionIdentity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*testing.T, *reviewFixture)
	}{
		{"profile", func(_ *testing.T, f *reviewFixture) {
			p := f.profiles["default"]
			p.Plan.MaxPackages++
			f.profiles["default"] = p
		}},
		{"prompt", func(_ *testing.T, f *reviewFixture) { f.service.prompts.Hash = "changed-prompt-hash" }},
		{"base", func(_ *testing.T, f *reviewFixture) {
			f.gitlab.mu.Lock()
			defer f.gitlab.mu.Unlock()
			f.gitlab.version.BaseSHA = strings.Repeat("3", 40)
		}},
		{"target-start", func(_ *testing.T, f *reviewFixture) {
			f.gitlab.mu.Lock()
			defer f.gitlab.mu.Unlock()
			f.gitlab.version.StartSHA = strings.Repeat("4", 40)
		}},
		{"workspace", func(t *testing.T, f *reviewFixture) {
			if err := os.MkdirAll(f.manager.Get(42).BaseDir(), 0700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, "job-recover-"+tc.name)
			id := interruptReview(t, f, job)
			before, err := f.db.GetWorkflowDossier(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			calls := f.runner.count()
			tc.change(t, f)
			if err := f.service.Recover(context.Background()); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflow(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if wf.Status != "cancelled" {
				t.Fatalf("recovery reused stale workflow: %s", wf.Status)
			}
			after, err := f.db.GetWorkflowDossier(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if string(before.Data) != string(after.Data) || after.PublicationState != store.WorkflowPublicationSkipped {
				t.Fatal("recovery did not retain and skip stale snapshot")
			}
			if f.runner.count() != calls {
				t.Fatal("recovery executed an agent with stale identity")
			}
		})
	}
}

func TestRetryChecksWorkspaceBaseIdentity(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx := context.Background()
	base := f.manager.Get(42).BaseDir()
	if err := os.MkdirAll(base, 0700); err != nil {
		t.Fatal(err)
	}
	start := strings.Repeat("2", 40)
	gitDir := filepath.Join(f.manager.Get(42).MirrorDir(), "worktrees", "base")
	if err := os.MkdirAll(gitDir, 0700); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{
		filepath.Join(base, ".git"):     "gitdir: " + gitDir,
		filepath.Join(gitDir, "gitdir"): filepath.Join(base, ".git"),
		filepath.Join(gitDir, "HEAD"):   start,
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: start, IndexSHA: start}); err != nil {
		t.Fatal(err)
	}
	f.git.mu.Lock()
	f.git.dirHeads[base] = start
	f.git.mu.Unlock()
	job := f.seedReview(t, "job-workspace-base")
	oldID := interruptReview(t, f, job)
	before, err := f.db.GetWorkflowDossier(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	var old struct {
		BaseDir string `json:"base_dir"`
	}
	if err := json.Unmarshal(before.Data, &old); err != nil || old.BaseDir != base {
		t.Fatalf("original base = %q, %v", old.BaseDir, err)
	}
	f.git.mu.Lock()
	f.git.dirHeads[base] = strings.Repeat("3", 40)
	f.git.mu.Unlock()
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.ID == oldID {
		t.Fatal("reused nodes after workspace base changed")
	}
	newRecord, err := f.db.GetWorkflowDossier(ctx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	var fresh struct {
		BaseDir string `json:"base_dir"`
	}
	if err := json.Unmarshal(newRecord.Data, &fresh); err != nil {
		t.Fatal(err)
	}
	if fresh.BaseDir != "" {
		t.Fatalf("unverified workspace base was passed to sandbox: %q", fresh.BaseDir)
	}
	after, err := f.db.GetWorkflowDossier(ctx, oldID)
	if err != nil || string(after.Data) != string(before.Data) {
		t.Fatal("old snapshot changed")
	}
}

func TestRecoveryRejectsLegacyIdentityWithoutProof(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-legacy-identity")
	id := interruptReview(t, f, job)
	ctx := context.Background()
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(record.Data, &data); err != nil {
		t.Fatal(err)
	}
	delete(data["metadata"].(map[string]any), "profile_hash")
	record.Data, err = json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	record.Hash = ""
	if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
		t.Fatal(err)
	}
	calls := f.runner.count()
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflow(ctx, id)
	if err != nil || wf.Status != "cancelled" {
		t.Fatalf("legacy recovery = %+v, %v", wf, err)
	}
	if f.runner.count() != calls {
		t.Fatal("legacy recovery executed an agent")
	}
}
