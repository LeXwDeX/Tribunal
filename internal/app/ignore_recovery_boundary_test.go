package app

import (
	"bytes"
	"context"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

func TestRecoverChecksIgnorePolicyWithoutConfigHash(t *testing.T) {
	for _, invalidConfig := range []bool{false, true} {
		name := "changed-policy"
		if invalidConfig {
			name = "unreadable-policy"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, "policy-recovery")
			configHash, promptHash := currentReviewHashes(t, f)
			dossier := testDossier(t, f.head)
			raw, err := review.MarshalDossier(dossier)
			if err != nil {
				t.Fatal(err)
			}
			hash, err := review.DossierHash(dossier)
			if err != nil {
				t.Fatal(err)
			}
			spec, _ := testGraph(t, dossier)
			id, _, err := f.db.CreateReviewWorkflow(ctx, sqlite.ReviewWorkflowRequest{
				JobID: job.ID, Spec: spec, Meta: map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA, "fingerprint": hash, "config_hash": configHash, "prompt_hash": promptHash},
				DossierVersion: review.DossierSchemaVersion, DossierHash: hash, DossierData: raw,
				ExpectedGeneration: jobGeneration(t, job), ProjectID: 42, MRIID: 7, HeadSHA: f.head,
			})
			if err != nil {
				t.Fatal(err)
			}
			f.gitlab.docs[".gitlab-review.yml"] = "ignore: [main.go]\n"
			if invalidConfig {
				f.gitlab.docs[".gitlab-review.yml"] = "ignore: [\n"
			}
			err = f.service.Recover(ctx)
			if !invalidConfig && err != nil {
				t.Fatal(err)
			}
			if invalidConfig && err != nil {
				t.Fatalf("recovery did not settle unreadable ignore configuration: %v", err)
			}
			if f.runner.count() != 0 || f.git.addCalls != 0 || countPublicationEffects(f) != (publicationEffects{}) {
				t.Fatalf("unsafe recovery: agent=%d worktrees=%d effects=%+v", f.runner.count(), f.git.addCalls, countPublicationEffects(f))
			}
			wf, err := f.db.GetWorkflow(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if !invalidConfig && (wf.Status != "cancelled" || wf.PublicationState != store.WorkflowPublicationSkipped) {
				t.Fatalf("changed policy left workflow active: %+v", wf)
			}
			if invalidConfig && wf.Status != "cancelled" {
				t.Fatalf("configuration read failure left workflow active: %+v", wf)
			}
			if invalidConfig {
				if wf.PublicationState != store.WorkflowPublicationSkipped || wf.PublicationError != invalidRepoConfigDetail {
					t.Fatalf("configuration failure publication = %q/%q", wf.PublicationState, wf.PublicationError)
				}
				if f.gitlab.noteCount() != 1 {
					t.Fatalf("configuration failure notes = %d, want one explanation", f.gitlab.noteCount())
				}
				if err := f.service.Recover(ctx); err != nil {
					t.Fatalf("second Recover: %v", err)
				}
				if f.gitlab.noteCount() != 1 {
					t.Fatalf("second Recover repeated the explanation: %d", f.gitlab.noteCount())
				}
				status, err := f.db.GetReviewStatus(ctx, 42, 7)
				if err != nil || status.State != reviewStatusFailed || status.Detail != invalidRepoConfigDetail {
					t.Fatalf("review status = %+v, %v", status, err)
				}
			}
			after, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil || after.Hash != hash || !bytes.Equal(after.Data, raw) {
				t.Fatalf("recovery rewrote dossier: %v", err)
			}
		})
	}
}
