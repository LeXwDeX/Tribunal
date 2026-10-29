package app

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRetryOnboardingAdvancesWorkspaceFromError(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	if _, err := f.db.SaveWorkspace(context.Background(), store.Workspace{
		ProjectID: 42, State: "error", LastError: store.WorkspaceOperationFailed,
	}); err != nil {
		t.Fatalf("SaveWorkspace: %v", err)
	}
	service := unrestrictedIndexService(t, f)
	if _, err := service.RetryOnboarding(context.Background(), 42); err != nil {
		t.Fatalf("RetryOnboarding: %v", err)
	}
	if err := service.Onboard(context.Background(), 42); err != nil {
		t.Fatalf("consume retry job: %v", err)
	}
	state, err := f.db.GetWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if state.State != "ready" || state.LastError != "" {
		t.Fatalf("workspace = %+v, want ready without last_error", state)
	}
}

func TestRetryOnboardingEnqueuesJob(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	service := unrestrictedIndexService(t, f)

	jobID, err := service.RetryOnboarding(context.Background(), 42)
	if err != nil {
		t.Fatalf("RetryOnboarding: %v", err)
	}
	job, err := f.db.GetJob(context.Background(), jobID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if job.Kind != "workspace.onboard" || job.State != store.JobQueued {
		t.Fatalf("job = %+v, want queued workspace.onboard", job)
	}
	var payload struct {
		ProjectID int64  `json:"project_id"`
		Trigger   string `json:"trigger"`
	}
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatalf("payload: %v", err)
	}
	if payload.ProjectID != 42 || payload.Trigger != "manual_retry" {
		t.Fatalf("payload = %+v", payload)
	}

	secondID, err := service.RetryOnboarding(context.Background(), 42)
	if err != nil || secondID != jobID {
		t.Fatalf("second retry = %q, %v; want idempotent %q", secondID, err, jobID)
	}

	if _, err := service.RetryOnboarding(context.Background(), 99); err == nil {
		t.Fatal("missing project accepted")
	} else {
		var appErr *Error
		if !errors.As(err, &appErr) || appErr.Code != "not_found" {
			t.Fatalf("missing project error = %v, want not_found", err)
		}
	}
}
