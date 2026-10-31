package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type runtimeRepositorySource struct {
	*fakeGitOps
	head  string
	body  string
	files map[string][]byte
}

func (s *runtimeRepositorySource) ResolveCommitRef(context.Context, string, string) (string, error) {
	return s.head, nil
}

func (s *runtimeRepositorySource) ReadRepositorySkills(_ context.Context, _ string, _ string, deny func(string) bool) (map[string][]byte, error) {
	out := make(map[string][]byte)
	for name, data := range s.files {
		if deny == nil || !deny(".claude/skills/"+name) {
			out[name] = data
		}
	}
	if s.files != nil {
		return out, nil
	}
	body := s.body
	if body == "" {
		body = policyGateSkillBody
	}
	out["demo/SKILL.md"] = []byte(body)
	return out, nil
}

func (s *runtimeRepositorySource) ReadRepositorySkillConfig(context.Context, string, string) ([]byte, bool, error) {
	return nil, false, nil
}

func attachRepositorySource(f *reviewFixture, head string) {
	f.service.git = &runtimeRepositorySource{fakeGitOps: f.git, head: head}
	f.publisher.git = f.service.git
}

func repositoryRuntimeFixture(t *testing.T, targetSHA string, skills ...string) (*reviewFixture, *store.RepositorySkillAuthorization) {
	t.Helper()
	f := newReviewFixture(t, reviewTestHead)
	attachRepositorySource(f, f.head)
	ctx := context.Background()
	if targetSHA == "" {
		targetSHA = f.head
	}
	if len(skills) == 0 {
		skills = []string{"repo:demo"}
	}
	snapshot, err := repo.RepositorySkillsSnapshotSHA256(map[string][]byte{"demo/SKILL.md": []byte(policyGateSkillBody)})
	if err != nil {
		t.Fatal(err)
	}
	setRepositorySkillSetting(t, f, true)
	now := time.Now().UTC()
	if err := f.db.CreateSession(ctx, store.Session{IDHash: []byte(strings.Repeat("x", 32)), UserID: 7, Username: "admin", Name: "admin", Role: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	grant, err := f.db.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{
		ProjectID: 42, TargetBranch: "main", Block: "code-review",
		TargetSHA: targetSHA, Skills: skills, SnapshotSHA256: snapshot,
	}, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	return f, grant
}

func captureRepositorySkill(t *testing.T, f *reviewFixture) (*string, *string) {
	t.Helper()
	var prompt, loaded string
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		for _, block := range spec.System {
			prompt += block.Text
		}
		for _, tool := range spec.Tools {
			if tool.Def().Name == "load_skill" {
				result, err := tool.Call(ctx, json.RawMessage(`{"name":"repo:demo"}`))
				if err != nil {
					return nil, err
				}
				loaded = result.Content
			}
		}
		return f.runner.Run(ctx, spec)
	}
	return &prompt, &loaded
}

func TestRepositorySkillRuntimeUsesVerifiedGrant(t *testing.T) {
	f, grant := repositoryRuntimeFixture(t, "")
	prompt, loaded := captureRepositorySkill(t, f)
	job := f.seedReview(t, "repository-runtime")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*prompt, "repo:demo") || !strings.Contains(*prompt, "visible") || !strings.Contains(*loaded, "body") {
		t.Fatalf("verified repository skill was not bound: prompt=%q loaded=%q", *prompt, *loaded)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatal(err)
	}
	provenance := dossier.RepositorySkillProvenance
	if provenance == nil || provenance.Mode != "repository" || provenance.TargetSHA != f.head ||
		provenance.TargetBranch != "main" || provenance.SnapshotSHA256 != grant.SnapshotSHA256 ||
		provenance.GrantRevisions["code-review"] != grant.Revision {
		t.Fatalf("execution provenance = %+v", provenance)
	}
	if f.runner.count() == 0 {
		t.Fatal("review did not run")
	}
}

func TestRepositorySkillRuntimeRejectsTargetAndSnapshotMismatch(t *testing.T) {
	for _, tc := range []struct {
		name string
		head string
	}{
		{"target", strings.Repeat("c", 40)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := repositoryRuntimeFixture(t, tc.head)
			prompt, _ := captureRepositorySkill(t, f)
			job := f.seedReview(t, "repository-target-mismatch")
			if err := f.service.RunReview(context.Background(), job); err == nil || !strings.Contains(err.Error(), "target changed") {
				t.Fatalf("target mismatch = %v", err)
			}
			if f.runner.count() != 0 || strings.Contains(*prompt, "repo:demo") {
				t.Fatal("mismatched target reached a model")
			}
		})
	}

	f := newReviewFixture(t, reviewTestHead)
	attachRepositorySource(f, f.head)
	ctx := context.Background()
	setRepositorySkillSetting(t, f, true)
	now := time.Now().UTC()
	if err := f.db.CreateSession(ctx, store.Session{IDHash: []byte(strings.Repeat("x", 32)), UserID: 7, Username: "admin", Name: "admin", Role: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{
		ProjectID: 42, TargetBranch: "main", Block: "code-review",
		TargetSHA: f.head, Skills: []string{"repo:demo"}, SnapshotSHA256: strings.Repeat("d", 64),
	}, store.BlockBindingAbsentRevision, 7); err != nil {
		t.Fatal(err)
	}
	prompt, _ := captureRepositorySkill(t, f)
	job := f.seedReview(t, "repository-snapshot-mismatch")
	if err := f.service.RunReview(ctx, job); err == nil || !strings.Contains(err.Error(), "snapshot changed") {
		t.Fatalf("snapshot mismatch = %v", err)
	}
	if f.runner.count() != 0 || strings.Contains(*prompt, "repo:demo") {
		t.Fatal("mismatched snapshot reached a model")
	}
}

func TestRepositorySkillRuntimePublishRequiresCurrentGrant(t *testing.T) {
	f, grant := repositoryRuntimeFixture(t, "")
	job := f.seedReview(t, "repository-publish")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.PublicationState != store.WorkflowPublicationPublished || f.gitlab.noteCount() == 0 {
		t.Fatalf("verified provenance publication = %+v notes=%d", workflow, f.gitlab.noteCount())
	}
	revoked := *grant
	revoked.Skills = []string{}
	reviewed, err := f.db.SaveRepositorySkillAuthorization(context.Background(), revoked, grant.Revision, 7)
	if err != nil || len(reviewed.Skills) != 0 {
		t.Fatalf("revoke grant = %+v, %v", reviewed, err)
	}
	before := f.gitlab.noteCount()
	if err := f.publisher.Republish(context.Background(), workflow.ID); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("republish after revocation = %v", err)
	}
	if got := f.gitlab.noteCount(); got != before {
		t.Fatalf("revoked provenance published %d notes", got-before)
	}
	if _, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID); err != nil {
		t.Fatal(err)
	}
}

func TestRepositorySkillRuntimeRecoveryRevalidatesGrant(t *testing.T) {
	f, _ := repositoryRuntimeFixture(t, "")
	job := f.seedReview(t, "repository-recovery")
	workflowID := interruptReview(t, f, job)
	before := f.runner.count()
	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), workflowID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Status != "completed" || workflow.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("recovered workflow = %+v", workflow)
	}
	if f.runner.count() <= before {
		t.Fatal("recovery did not resume the verified repository execution")
	}
}

func TestRepositorySkillPublicationRevalidatesTargetAndSnapshot(t *testing.T) {
	for _, change := range []string{"manual-target", "manual-snapshot", "pending-target"} {
		t.Run(change, func(t *testing.T) {
			f, _ := repositoryRuntimeFixture(t, "")
			job := f.seedReview(t, "repository-publication-"+change)
			if err := f.service.RunReview(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil || workflow.PublicationState != store.WorkflowPublicationPublished {
				t.Fatalf("initial publication = %+v, %v", workflow, err)
			}
			source := f.service.git.(*runtimeRepositorySource)
			switch change {
			case "manual-target", "pending-target":
				source.head = strings.Repeat("e", 40)
			case "manual-snapshot":
				source.body = "---\nname: demo\ndescription: changed\n---\nchanged body\n"
			}
			before := f.gitlab.noteCount()
			if change == "pending-target" {
				if err := f.db.SetWorkflowPublicationState(context.Background(), workflow.ID, store.WorkflowPublicationPending, ""); err != nil {
					t.Fatal(err)
				}
				if err := f.service.PublishPending(context.Background()); err != nil {
					t.Fatal(err)
				}
			} else if err := f.publisher.Republish(context.Background(), workflow.ID); !errors.Is(err, ErrStalePublication) {
				t.Fatalf("republish = %v", err)
			}
			if got := f.gitlab.noteCount(); got != before {
				t.Fatalf("stale repository provenance published %d notes", got-before)
			}
			workflow, err = f.db.GetWorkflow(context.Background(), workflow.ID)
			if err != nil {
				t.Fatal(err)
			}
			wantState := store.WorkflowPublicationPublished
			if change == "pending-target" {
				wantState = store.WorkflowPublicationSkipped
			}
			if workflow.PublicationState != wantState {
				t.Fatalf("publication state = %+v, want %s", workflow, wantState)
			}
		})
	}
}

func repositoryRuntimeRenameFixture(t *testing.T, grantSnapshot string) (*reviewFixture, *store.RepositorySkillAuthorization) {
	t.Helper()
	f := newReviewFixture(t, reviewTestHead)
	files := map[string][]byte{
		"demo/SKILL.md":   []byte(policyGateSkillBody),
		"hidden/SKILL.md": []byte("---\nname: hidden\ndescription: hidden marker\n---\nhidden body\n"),
	}
	source := &runtimeRepositorySource{fakeGitOps: f.git, head: f.head, files: files}
	f.service.git, f.publisher.git = source, source
	f.gitlab.diffs = append(f.gitlab.diffs, gitlab.FileDiff{
		OldPath: "private/secret.txt", NewPath: ".claude/skills/hidden/SKILL.md", RenamedFile: true,
	})
	ctx := context.Background()
	settings := json.RawMessage(`{"allow_repo_skills":true,"ignore":["**/private/**"]}`)
	if _, err := f.db.UpdateProject(ctx, 42, store.ProjectUpdate{Settings: settings}); err != nil {
		t.Fatal(err)
	}
	if grantSnapshot == "" {
		var err error
		grantSnapshot, err = repo.RepositorySkillsSnapshotSHA256(map[string][]byte{
			"demo/SKILL.md": []byte(policyGateSkillBody),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC()
	if err := f.db.CreateSession(ctx, store.Session{IDHash: []byte(strings.Repeat("x", 32)), UserID: 7, Username: "admin", Name: "admin", Role: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	grant, err := f.db.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{
		ProjectID: 42, TargetBranch: "main", Block: "code-review",
		TargetSHA: f.head, Skills: []string{"repo:demo"}, SnapshotSHA256: grantSnapshot,
	}, store.BlockBindingAbsentRevision, 7)
	if err != nil {
		t.Fatal(err)
	}
	return f, grant
}

func TestRepositorySkillRuntimeDenySetIncludesIgnoredRenames(t *testing.T) {
	f, grant := repositoryRuntimeRenameFixture(t, "")
	prompt, loaded := captureRepositorySkill(t, f)
	job := f.seedReview(t, "repository-rename-deny")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*prompt, "repo:hidden") || strings.Contains(*prompt, "hidden marker") || strings.Contains(*loaded, "hidden body") {
		t.Fatalf("ignored rename reached a model: prompt=%q loaded=%q", *prompt, *loaded)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatal(err)
	}
	excluded := make(map[string]bool, len(dossier.RepositorySkillExcluded))
	for _, name := range dossier.RepositorySkillExcluded {
		excluded[name] = true
	}
	if !excluded[".claude/skills/hidden/SKILL.md"] || !excluded["private/secret.txt"] {
		t.Fatalf("exclusion evidence = %+v", dossier.RepositorySkillExcluded)
	}
	if dossier.RepositorySkillProvenance == nil || dossier.RepositorySkillProvenance.SnapshotSHA256 != grant.SnapshotSHA256 {
		t.Fatalf("provenance = %+v, grant snapshot = %q", dossier.RepositorySkillProvenance, grant.SnapshotSHA256)
	}
}

func TestRepositorySkillRuntimeFailsClosedWhenIgnoredRenameChangesSnapshot(t *testing.T) {
	all, err := repo.RepositorySkillsSnapshotSHA256(map[string][]byte{
		"demo/SKILL.md":   []byte(policyGateSkillBody),
		"hidden/SKILL.md": []byte("---\nname: hidden\ndescription: hidden marker\n---\nhidden body\n"),
	})
	if err != nil {
		t.Fatal(err)
	}
	f, _ := repositoryRuntimeRenameFixture(t, all)
	prompt, _ := captureRepositorySkill(t, f)
	job := f.seedReview(t, "repository-rename-snapshot")
	if err := f.service.RunReview(context.Background(), job); err == nil || !strings.Contains(err.Error(), "snapshot changed") {
		t.Fatalf("unfiltered snapshot = %v", err)
	}
	if f.runner.count() != 0 || strings.Contains(*prompt, "repo:hidden") {
		t.Fatal("unfiltered repository snapshot reached a model")
	}
}

func TestRepositorySkillRuntimeRecoveryAndRepublishUseStoredDenyEvidence(t *testing.T) {
	f, _ := repositoryRuntimeRenameFixture(t, "")
	job := f.seedReview(t, "repository-rename-recovery")
	workflowID := interruptReview(t, f, job)
	before := f.runner.count()
	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), workflowID)
	if err != nil || workflow.Status != "completed" {
		t.Fatalf("recovered workflow = %+v, %v", workflow, err)
	}
	if f.runner.count() <= before {
		t.Fatal("recovery did not resume the verified repository execution")
	}
	if err := f.publisher.Republish(context.Background(), workflowID); err != nil {
		t.Fatalf("republish with stored deny evidence = %v", err)
	}
}

func TestRepositorySkillRetryRebuildsFreshDenyAfterEnablement(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	files := map[string][]byte{
		"demo/SKILL.md":   []byte(policyGateSkillBody),
		"hidden/SKILL.md": []byte("---\nname: hidden\ndescription: hidden marker\n---\nhidden body\n"),
	}
	source := &runtimeRepositorySource{fakeGitOps: f.git, head: f.head, files: files}
	f.service.git, f.publisher.git = source, source
	f.gitlab.diffs = append(f.gitlab.diffs, gitlab.FileDiff{
		OldPath: "private/secret.txt", NewPath: ".claude/skills/hidden/SKILL.md", RenamedFile: true,
	})
	ctx := context.Background()
	disabled := json.RawMessage(`{"allow_repo_skills":false,"ignore":["**/private/**"]}`)
	if _, err := f.db.UpdateProject(ctx, 42, store.ProjectUpdate{Settings: disabled}); err != nil {
		t.Fatal(err)
	}
	job := f.seedReview(t, "repository-enable-retry")
	oldID := interruptReview(t, f, job)
	oldRecord, err := f.db.GetWorkflowDossier(ctx, oldID)
	if err != nil {
		t.Fatal(err)
	}
	var oldDossier review.Dossier
	if err := json.Unmarshal(oldRecord.Data, &oldDossier); err != nil {
		t.Fatal(err)
	}
	if oldDossier.RepositorySkillProvenance == nil || oldDossier.RepositorySkillProvenance.Mode != "none" || len(oldDossier.RepositorySkillExcluded) != 0 {
		t.Fatalf("disabled dossier = provenance %+v excluded %+v", oldDossier.RepositorySkillProvenance, oldDossier.RepositorySkillExcluded)
	}

	enabled := json.RawMessage(`{"allow_repo_skills":true,"ignore":["**/private/**"]}`)
	if _, err := f.db.UpdateProject(ctx, 42, store.ProjectUpdate{Settings: enabled}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := f.db.CreateSession(ctx, store.Session{IDHash: []byte(strings.Repeat("x", 32)), UserID: 7, Username: "admin", Name: "admin", Role: "admin", CreatedAt: now, ExpiresAt: now.Add(time.Hour)}, store.SessionLimits{}); err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetProjectGrant(ctx, 42, 7, store.ProjectAccessManage); err != nil {
		t.Fatal(err)
	}
	snapshot, err := repo.RepositorySkillsSnapshotSHA256(map[string][]byte{"demo/SKILL.md": []byte(policyGateSkillBody)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SaveRepositorySkillAuthorization(ctx, store.RepositorySkillAuthorization{
		ProjectID: 42, TargetBranch: "main", Block: "code-review",
		TargetSHA: f.head, Skills: []string{"repo:demo"}, SnapshotSHA256: snapshot,
	}, store.BlockBindingAbsentRevision, 7); err != nil {
		t.Fatal(err)
	}
	prompt, loaded := captureRepositorySkill(t, f)
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	oldWorkflow, err := f.db.GetWorkflow(ctx, oldID)
	if err != nil || oldWorkflow.Status != "cancelled" {
		t.Fatalf("old workflow = %+v, %v", oldWorkflow, err)
	}
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || workflow.ID == oldID || workflow.Status != "completed" {
		t.Fatalf("rebuilt workflow = %+v, %v", workflow, err)
	}
	record, err := f.db.GetWorkflowDossier(ctx, workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatal(err)
	}
	excluded := make(map[string]bool, len(dossier.RepositorySkillExcluded))
	for _, name := range dossier.RepositorySkillExcluded {
		excluded[name] = true
	}
	if !excluded[".claude/skills/hidden/SKILL.md"] || !excluded["private/secret.txt"] {
		t.Fatalf("rebuilt exclusion evidence = %+v", dossier.RepositorySkillExcluded)
	}
	if dossier.RepositorySkillProvenance == nil || dossier.RepositorySkillProvenance.Mode != "repository" || dossier.RepositorySkillProvenance.SnapshotSHA256 != snapshot {
		t.Fatalf("rebuilt provenance = %+v", dossier.RepositorySkillProvenance)
	}
	if strings.Contains(*prompt, "repo:hidden") || strings.Contains(*loaded, "hidden body") {
		t.Fatalf("rebuilt execution allowed an ignored repository skill: prompt=%q loaded=%q", *prompt, *loaded)
	}
	if f.runner.count() == 0 {
		t.Fatal("rebuilt execution did not run")
	}
}
