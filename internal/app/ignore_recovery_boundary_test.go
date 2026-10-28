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
			if invalidConfig && err == nil {
				t.Fatal("recovery accepted unreadable ignore configuration")
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
			if invalidConfig && wf.Status != "pending" {
				t.Fatalf("configuration read failure lost recoverable workflow: %+v", wf)
			}
			after, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil || after.Hash != hash || !bytes.Equal(after.Data, raw) {
				t.Fatalf("recovery rewrote dossier: %v", err)
			}
		})
	}
}
