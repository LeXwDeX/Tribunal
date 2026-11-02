package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestIssue161RetryOnboardingDeletedBeforeReregistration(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	for attempt := 1; attempt <= 3; attempt++ {
		job, err := r.store.Claim(ctx, "old", []string{"workspace.onboard"}, time.Minute)
		if err != nil || job == nil || job.Attempts != attempt {
			t.Fatalf("old claim: %+v %v", job, err)
		}
		if err := r.store.Fail(ctx, *job, errors.New("failed"), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.store.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "error", LastError: store.WorkspaceOperationFailed}); err != nil {
		t.Fatal(err)
	}
	retryID, err := r.workspaces.RetryOnboarding(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := r.store.GetJob(ctx, retryID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("old retry after deletion: %v", err)
	}
	registration, err := r.Projects().Register(ctx, RegisterInput{Project: "42", AccessToken: "synthetic-token"})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.Claim(ctx, "new", []string{"workspace.onboard"}, time.Minute)
	if err != nil || job == nil || job.ID != registration.JobID || job.ID == retryID || job.Key != "workspace.onboard:42" {
		t.Fatalf("new claim: %+v %v", job, err)
	}
}

func TestIssue161ReviewWorkerSuccessorStatus(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	api := newReviewGitLab(sha40)
	transient := errors.New("temporary upstream error")
	api.mrErr = transient
	r.reviews.gitlab = api
	first, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open","profile":"default"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var old *store.Job
	for attempt := 1; attempt <= 2; attempt++ {
		old, err = r.store.Claim(ctx, "old", []string{"review.run"}, time.Minute)
		if err != nil || old == nil || old.ID != first.JobID || old.Attempts != attempt {
			t.Fatalf("old claim: %+v %v", old, err)
		}
		if err := r.reviews.RunReview(ctx, old); !errors.Is(err, transient) {
			t.Fatalf("old run: %v", err)
		}
		if attempt == 1 {
			if err := r.store.Fail(ctx, *old, transient, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	second, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_update","profile":"default"}`)})
	if err != nil || second.Generation != first.Generation+1 || second.JobID == first.JobID {
		t.Fatalf("successor: %+v %v", second, err)
	}
	if err := r.store.Fail(ctx, *old, transient, nil); err != nil {
		t.Fatal(err)
	}
	if job, err := r.store.GetJob(ctx, old.ID); err != nil || job.State != store.JobCancelled {
		t.Fatalf("old job: %+v %v", job, err)
	}
	job, err := r.store.Claim(ctx, "new", []string{"review.run"}, time.Minute)
	if err != nil || job == nil || job.ID != second.JobID || job.Attempts != 1 {
		t.Fatalf("new claim: %+v %v", job, err)
	}
	api.mrErr = nil
	r.reviews.git = newFakeGitOps(sha40)
	r.reviews.extensions = nil
	r.reviews.testIdentity = "synthetic"
	r.reviews.profiles = map[string]config.ProfileConfig{"default": testReviewProfile()}
	r.reviews.providerNames = []string{"fake"}
	r.reviews.agentRun = (&fakeAgentRunner{}).Run
	r.reviews.clients, r.reviews.tools, r.reviews.skills, r.reviews.blocks = nil, nil, nil, nil
	r.reviews.reviewDefaults = testDefaults()
	r.publisher.gitlab, r.publisher.profiles = api, r.reviews.profiles
	r.publisher.reviewDefaults = r.reviews.reviewDefaults
	if err := r.reviews.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	workflow, err := r.store.GetWorkflowByJob(ctx, job.ID)
	if err != nil || workflow.Status != "completed" {
		t.Fatalf("workflow: %+v %v", workflow, err)
	}
	if err := r.store.Complete(ctx, *job); err != nil {
		t.Fatal(err)
	}
	if err := r.reviews.ReconcileSideEffects(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := r.store.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.Generation != second.Generation || status.Attempt != 1 || status.State != store.ReviewStatusPass {
		t.Fatalf("status: %+v %v", status, err)
	}
}
