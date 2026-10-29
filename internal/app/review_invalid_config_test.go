package app

import (
	"context"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRunReviewInvalidRepoConfigIsPermanentAndCommented(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.docs[".gitlab-review.yml"] = "ignore: [\n"
	job := f.seedRunFor(t, "invalid-config", reviewTestHead, "mr_open")

	err := f.service.RunReview(ctx, job)
	if err == nil || !IsPermanentJob(err) {
		t.Fatalf("error = %v, want a permanent job error", err)
	}
	if !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("error = %v, want ErrInvalidRepoConfig", err)
	}
	if _, err := f.db.GetWorkflowByJob(ctx, job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("workflow was created for invalid configuration: %v", err)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("notes = %d, want one explanation comment", f.gitlab.noteCount())
	}
	if body := f.gitlab.notes[0].Body; body != invalidRepoConfigNote {
		t.Fatalf("note = %q, want %q", body, invalidRepoConfigNote)
	}
}

func TestStoredInvalidIgnorePolicyIsPermanent(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if _, err := f.db.UpdateProject(ctx, 42, store.ProjectUpdate{Settings: []byte(`{"ignore":["!keep"]}`)}); err != nil {
		t.Fatalf("UpdateProject: %v", err)
	}
	job := f.seedRunFor(t, "invalid-stored-policy", reviewTestHead, "mr_open")
	calls := 0
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		calls++
		return nil, nil
	}

	err := f.service.RunReview(ctx, job)
	if err == nil || !IsPermanentJob(err) || !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("error = %v, want a permanent ErrInvalidRepoConfig", err)
	}
	if calls != 0 {
		t.Fatalf("model calls = %d, want 0", calls)
	}
	if f.gitlab.noteCount() != 1 {
		t.Fatalf("notes = %d, want one explanation", f.gitlab.noteCount())
	}
	if _, err := f.db.GetWorkflowByJob(ctx, job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("workflow was created for a stored invalid policy: %v", err)
	}
}
