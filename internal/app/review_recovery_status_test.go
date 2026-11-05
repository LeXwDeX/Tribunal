package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func mutateDossierMetadata(t *testing.T, f *reviewFixture, id, key, value string) {
	t.Helper()
	ctx := context.Background()
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(record.Data, &data); err != nil {
		t.Fatal(err)
	}
	data["metadata"].(map[string]any)[key] = value
	if record.Data, err = json.Marshal(data); err != nil {
		t.Fatal(err)
	}
	record.Hash = ""
	if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
		t.Fatal(err)
	}
}

// A recovery cancel of the MR's current generation must leave a terminal
// status that names the cause, and the publication reason must match it.
func TestRecoveryCancelSettlesCurrentGenerationStatus(t *testing.T) {
	for _, tc := range []struct{ cause, key, value string }{
		{"profile_hash_changed", "profile_hash", "sha256:other"},
		{"execution_identity_changed", "execution_identity", "sha256:other"},
		{"config_changed", "config_hash", "sha256:other"},
	} {
		t.Run(tc.cause, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, "job-settle-"+tc.cause)
			id := interruptReview(t, f, job)
			f.service.status = testStatusReporter(f.gitlab)
			mutateDossierMetadata(t, f, id, tc.key, tc.value)
			if err := f.service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			drainReviewSideEffects(t, f)
			wf, err := f.db.GetWorkflow(ctx, id)
			if err != nil || wf.Status != "cancelled" {
				t.Fatalf("workflow = %+v, %v; want cancelled", wf, err)
			}
			status, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || status.State != reviewStatusCancelled || !strings.Contains(status.Detail, "cancelled on recovery: "+tc.cause) {
				t.Fatalf("status = %+v, %v; want %s with cause %s", status, err, reviewStatusCancelled, tc.cause)
			}
			record, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil || record.PublicationState != store.WorkflowPublicationSkipped ||
				record.PublicationError != "cancelled on recovery: "+tc.cause {
				t.Fatalf("publication = %q/%q, %v; want skipped with cause %s", record.PublicationState, record.PublicationError, err, tc.cause)
			}
			f.gitlab.mu.Lock()
			updates := strings.Join(f.gitlab.statusUpdates, "\n")
			f.gitlab.mu.Unlock()
			if !strings.Contains(updates, "评审已取消") || !strings.Contains(updates, tc.cause) {
				t.Fatalf("GitLab status updates missing cancelled/%s:\n%s", tc.cause, updates)
			}
		})
	}
}

// A newer generation owns the MR; the recovery cancel of an older one must not
// write status over it.
func TestRecoveryCancelSettleSkipsSupersededGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-settle-old")
	id := interruptReview(t, f, job)
	f.seedReview(t, "job-settle-new")
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	f.service.settleRecoveryCancel(ctx, id, "profile_hash_changed")
	after, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || after.State != before.State || after.Generation != before.Generation || after.Detail != before.Detail {
		t.Fatalf("status = %+v, %v; want unchanged %+v", after, err, before)
	}
}
