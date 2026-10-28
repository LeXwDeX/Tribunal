package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRunCleanupSettlesKnownMissingJobReservation(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "active")
	for _, d := range []store.WorkflowDossier{
		{WorkflowID: "orphan-missing", ProjectID: 42, JobID: "missing-job", Version: 1, Data: json.RawMessage(`{"old":true}`)},
		{WorkflowID: "orphan-active", ProjectID: 42, JobID: job.ID, Version: 1, Data: json.RawMessage(`{}`)},
		{WorkflowID: "orphan-unknown", JobID: "unknown-job", Version: 1, Data: json.RawMessage(`{}`)},
	} {
		if _, err := f.db.SaveWorkflowDossier(ctx, d); err != nil {
			t.Fatal(err)
		}
	}
	ws := f.manager.Get(42)
	for _, id := range []string{"orphan-missing", "orphan-active", "orphan-unknown"} {
		dir := filepath.Join(ws.Root, "runs", id)
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
		old := time.Now().Add(-3 * time.Hour)
		if err := os.Chtimes(dir, old, old); err != nil {
			t.Fatal(err)
		}
	}
	cleanup, err := NewRunCleanupService(RunCleanupConfig{Store: f.db, Workspace: f.manager, Git: &cleanupTestGit{}, After: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		id, state string
		removed   bool
	}{
		{"orphan-missing", store.WorkflowPublicationSkipped, true},
		{"orphan-active", store.WorkflowPublicationPending, false},
		{"orphan-unknown", store.WorkflowPublicationPending, false},
	} {
		d, err := f.db.GetWorkflowDossier(ctx, tc.id)
		if err != nil || d.PublicationState != tc.state {
			t.Fatalf("%s state = %+v, %v", tc.id, d, err)
		}
		_, err = os.Stat(filepath.Join(ws.Root, "runs", tc.id))
		if tc.removed != os.IsNotExist(err) {
			t.Fatalf("%s stat: %v", tc.id, err)
		}
	}
}

func TestReviewRecoverySettlesKnownOrphan(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if _, err := f.db.SaveWorkflowDossier(ctx, store.WorkflowDossier{
		WorkflowID: "recovery-orphan", ProjectID: 42, JobID: "retained-job", Version: 1, Data: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatal(err)
	}
	f.service.publisher = nil
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	d, err := f.db.GetWorkflowDossier(ctx, "recovery-orphan")
	if err != nil || d.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("orphan after recovery = %+v, %v", d, err)
	}
}
