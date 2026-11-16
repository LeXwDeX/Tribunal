package app

import (
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// sharedSentinelProject converts a seeded project into the zero-length token
// sentinel used by shared-credential mode.
func sharedSentinelProject(t *testing.T, f *reviewFixture) *store.Project {
	t.Helper()
	ctx := context.Background()
	before, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := f.db.RewrapProjectSecrets(ctx, 42, before.TokenEnc, []byte{}, before.WebhookSecretEnc, before.WebhookSecretEnc)
	if err != nil || !ok {
		t.Fatalf("convert to shared sentinel = %v, %v", ok, err)
	}
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.TokenEnc) != 0 {
		t.Fatalf("project is not a shared sentinel: %q", project.TokenEnc)
	}
	return project
}

func TestResolveReviewConfigDisablesCommitStatusForSharedProjects(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true

	projectTokenProject, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	if cfg := resolveReviewConfig(projectTokenProject, review.RepoDocs{}, f.defaults, f.profiles); !cfg.Settings.CommitStatus {
		t.Fatal("project-token review lost commit_status")
	}
	shared := sharedSentinelProject(t, f)
	if cfg := resolveReviewConfig(shared, review.RepoDocs{}, f.defaults, f.profiles); cfg.Settings.CommitStatus {
		t.Fatal("shared review kept commit_status despite the Reporter-only bot")
	}
}

func TestSharedProjectSkipsCommitStatus(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	shared := sharedSentinelProject(t, f)
	loaded := &loadedReview{
		workflow: &store.Workflow{ID: "wf-status"},
		dossier:  &review.Dossier{DiffVersion: gitlab.Version{HeadSHA: f.head}},
		record:   &store.WorkflowDossier{Hash: "fingerprint"},
		project:  shared,
		token:    sharedBotToken,
		mrIID:    7,
	}
	if err := f.publisher.publishCommitStatus(context.Background(), loaded, "success", "ok", func() error { return nil }); err != nil {
		t.Fatalf("publishCommitStatus: %v", err)
	}
	if len(f.gitlab.statuses) != 0 {
		t.Fatalf("shared project wrote %d commit statuses", len(f.gitlab.statuses))
	}
	publications, err := f.db.ListPublications(context.Background(), "wf-status")
	if err != nil {
		t.Fatal(err)
	}
	if len(publications) != 0 {
		t.Fatalf("shared project recorded status publications: %+v", publications)
	}

	// Defense in depth: a direct status write also skips shared projects.
	bound, err := bindOutbound(context.Background(), f.manager.Get(42))
	if err != nil {
		t.Fatal(err)
	}
	status := gitlab.CommitStatus{State: "success", Name: "ai-review"}
	if err := f.publisher.setCommitStatus(bound, sharedBotToken, 42, f.head, status, func() error { return nil }); err != nil {
		t.Fatalf("setCommitStatus: %v", err)
	}
	if len(f.gitlab.statuses) != 0 {
		t.Fatalf("shared project wrote a commit status through the direct path")
	}

	// A project-token project keeps writing commit statuses.
	ok, err := f.db.RewrapProjectSecrets(context.Background(), 42, []byte{}, []byte("project-token"), shared.WebhookSecretEnc, shared.WebhookSecretEnc)
	if err != nil || !ok {
		t.Fatalf("restore project token = %v, %v", ok, err)
	}
	if err := f.publisher.setCommitStatus(bound, "glpat-project-token", 42, f.head, status, func() error { return nil }); err != nil {
		t.Fatalf("project-token setCommitStatus: %v", err)
	}
	if len(f.gitlab.statuses) != 1 {
		t.Fatalf("project-token project wrote %d commit statuses, want 1", len(f.gitlab.statuses))
	}
}
