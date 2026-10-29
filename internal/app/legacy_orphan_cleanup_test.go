package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func TestRunCleanupSettlesExpiredLegacyOrphans(t *testing.T) {
	ctx := context.Background()
	clock := newRetentionClock()
	st, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "review.db"), sqlite.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	save := func(workflowID string) {
		t.Helper()
		if _, err := st.SaveWorkflowDossier(ctx, store.WorkflowDossier{
			WorkflowID: workflowID, ProjectID: 0, JobID: workflowID + "-job",
			Version: 1, Data: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatalf("SaveWorkflowDossier %s: %v", workflowID, err)
		}
	}

	save("legacy-old")
	clock.Advance(6 * 24 * time.Hour)
	save("legacy-fresh")
	queued, _, err := st.Enqueue(ctx, store.Job{Kind: "review.run", Key: "legacy-live"})
	if err != nil {
		t.Fatalf("Enqueue: %v", err)
	}
	if _, err := st.SaveWorkflowDossier(ctx, store.WorkflowDossier{
		WorkflowID: "legacy-live", ProjectID: 0, JobID: queued,
		Version: 1, Data: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("SaveWorkflowDossier legacy-live: %v", err)
	}
	clock.Advance(24*time.Hour + time.Hour)

	cleanup, err := NewRunCleanupService(RunCleanupConfig{
		Store:            st,
		Workspace:        workspace.NewManager(t.TempDir(), nil),
		Git:              &cleanupTestGit{fail: map[string]error{}},
		After:            time.Hour,
		DossierRetention: 7 * 24 * time.Hour,
		Now:              clock.Now,
	})
	if err != nil {
		t.Fatalf("NewRunCleanupService: %v", err)
	}
	if err := cleanup.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	for _, tc := range []struct {
		id, state string
	}{
		{"legacy-old", store.WorkflowPublicationSkipped},
		{"legacy-fresh", store.WorkflowPublicationPending},
		{"legacy-live", store.WorkflowPublicationPending},
	} {
		dossier, err := st.GetWorkflowDossier(ctx, tc.id)
		if err != nil {
			t.Fatalf("GetWorkflowDossier %s: %v", tc.id, err)
		}
		if dossier.PublicationState != tc.state {
			t.Errorf("%s state = %q, want %q", tc.id, dossier.PublicationState, tc.state)
		}
	}

	protected, err := st.ProtectedRunRefs(ctx)
	if err != nil {
		t.Fatalf("ProtectedRunRefs: %v", err)
	}
	for _, id := range []string{"legacy-old", "legacy-old-job"} {
		if _, ok := protected[id]; ok {
			t.Errorf("settled legacy orphan still protects %q", id)
		}
	}
	for _, id := range []string{"legacy-fresh", "legacy-live", queued} {
		if _, ok := protected[id]; !ok {
			t.Errorf("protected refs missing live reservation %q", id)
		}
	}
}
