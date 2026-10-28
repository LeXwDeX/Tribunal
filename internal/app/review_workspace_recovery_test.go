package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func rewriteWorkspaceEvidence(t *testing.T, f *reviewFixture, id string, change func(*review.Dossier)) []byte {
	t.Helper()
	ctx := context.Background()
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var d review.Dossier
	if err := json.Unmarshal(record.Data, &d); err != nil {
		t.Fatal(err)
	}
	change(&d)
	record.Data, err = json.Marshal(&d)
	if err != nil {
		t.Fatal(err)
	}
	record.Hash = ""
	if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
		t.Fatal(err)
	}
	return record.Data
}

func TestWorkspaceEvidenceRetryRejectsLegacyAndContradictions(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*review.Dossier)
	}{
		{"legacy", func(d *review.Dossier) { d.WorkspaceProvenance = nil }},
		{"version", func(d *review.Dossier) { d.WorkspaceProvenance.Version++ }},
		{"context", func(d *review.Dossier) {
			d.ContextDocs = "CANARY"
			d.WorkspaceProvenance.ContextBaseSHA = strings.Repeat("1", 40)
		}},
		{"index", func(d *review.Dossier) {
			d.WorkspaceProvenance.MCPEligible = true
			d.WorkspaceProvenance.IndexSHA = strings.Repeat("1", 40)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, "workspace-evidence-retry")
			id := interruptReview(t, f, job)
			before := rewriteWorkspaceEvidence(t, f, id, tc.change)
			ctx := context.Background()
			if err := f.service.Handle(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil || wf.ID == id {
				t.Fatalf("workflow=%+v err=%v", wf, err)
			}
			old, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil || old.JobID != "" || old.PublicationState != store.WorkflowPublicationSkipped || !bytes.Equal(old.Data, before) {
				t.Fatalf("old record=%+v err=%v", old, err)
			}
		})
	}
}

func TestWorkspaceEvidenceRecoveryRejectsFailedCurrentIndex(t *testing.T) {
	for _, mode := range []string{"retry", "recover"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.service.reviewDefaults.Ignore, f.publisher.reviewDefaults.Ignore = nil, nil
			target := strings.Repeat("2", 40)
			if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", IndexSHA: target}); err != nil {
				t.Fatal(err)
			}
			job := f.seedReview(t, "index-failure-recovery")
			id := interruptReview(t, f, job)
			before, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", IndexSHA: target, LastError: store.WorkspaceIndexFailed}); err != nil {
				t.Fatal(err)
			}
			calls := f.runner.count()
			if mode == "retry" {
				err = f.service.Handle(ctx, job)
			} else {
				err = f.service.Recover(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			old, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil || old.PublicationState != store.WorkflowPublicationSkipped || !bytes.Equal(old.Data, before.Data) {
				t.Fatalf("old=%+v err=%v", old, err)
			}
			if mode == "recover" && f.runner.count() != calls {
				t.Fatal("recovery executed old snapshot")
			}
			if mode == "retry" {
				wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
				if err != nil || wf.ID == id {
					t.Fatalf("workflow=%+v err=%v", wf, err)
				}
			}
		})
	}
}

func TestWorkspaceEvidenceRecoveryRejectsMalformedSnapshot(t *testing.T) {
	for _, mode := range []string{"retry", "recover"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, "malformed-workspace-evidence")
			id := interruptReview(t, f, job)
			record, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			data["workspace_provenance"] = "invalid"
			record.Data, err = json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			record.Hash = ""
			if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
				t.Fatal(err)
			}
			calls := f.runner.count()
			if mode == "retry" {
				err = f.service.Handle(ctx, job)
			} else {
				err = f.service.Recover(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil || after.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(after.PublicationError, "rerun") || !bytes.Equal(after.Data, record.Data) {
				t.Fatalf("record=%+v err=%v", after, err)
			}
			if mode == "recover" && f.runner.count() != calls {
				t.Fatal("recovery executed malformed snapshot")
			}
			if mode == "retry" {
				wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
				if err != nil || wf.ID == id {
					t.Fatalf("workflow=%+v err=%v", wf, err)
				}
			}
		})
	}
}

func TestWorkspaceEvidenceRetryDoesNotUpgradeBlockedMCP(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.reviewDefaults.Ignore, f.publisher.reviewDefaults.Ignore = nil, nil
	job := f.seedReview(t, "blocked-index-retry")
	id := interruptReview(t, f, job)
	before, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", IndexSHA: strings.Repeat("2", 40)}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.ID != id {
		t.Fatalf("workflow=%+v err=%v", wf, err)
	}
	after, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || !bytes.Equal(before.Data, after.Data) {
		t.Fatalf("snapshot changed: %v", err)
	}
	var d review.Dossier
	if err := json.Unmarshal(after.Data, &d); err != nil || d.WorkspaceProvenance.MCPEligible {
		t.Fatalf("MCP was upgraded: %+v err=%v", d.WorkspaceProvenance, err)
	}
}

type missingWorkspaceEvidenceStore struct{ ReviewStore }

func (s missingWorkspaceEvidenceStore) GetWorkflowDossier(ctx context.Context, id string) (*store.WorkflowDossier, error) {
	record, err := s.ReviewStore.GetWorkflowDossier(ctx, id)
	if err != nil {
		return nil, err
	}
	var d review.Dossier
	if err := json.Unmarshal(record.Data, &d); err != nil {
		return nil, err
	}
	d.WorkspaceProvenance = nil
	record.Data, err = json.Marshal(d)
	return record, err
}

func TestWorkspaceEvidenceDirectPublisherRejectsLegacy(t *testing.T) {
	for _, mode := range []string{"publish", "failure", "republish"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			var id string
			if mode == "failure" {
				f.defaults.PostFailureNote = false
				f.service.reviewDefaults = f.defaults
				f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
				job := f.seedReview(t, "legacy-direct-failed")
				if err := f.service.RunReview(ctx, job); err != nil {
					t.Fatal(err)
				}
				wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				id = wf.ID
				if err := f.db.SetWorkflowPublicationState(ctx, id, store.WorkflowPublicationPending, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				id = reviewWithFailedNote(t, f, "legacy-direct").ID
			}
			before, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			f.publisher.store = missingWorkspaceEvidenceStore{f.db}
			f.publisher.gitlab = nil
			switch mode {
			case "publish":
				err = f.publisher.Publish(ctx, id)
			case "failure":
				err = f.publisher.PublishFailure(ctx, id, "failure")
			case "republish":
				err = f.publisher.Republish(ctx, id)
			}
			if !errors.Is(err, ErrStalePublication) {
				t.Fatalf("publication err=%v", err)
			}
			after, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil || after.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(after.PublicationError, "rerun") || !bytes.Equal(before.Data, after.Data) {
				t.Fatalf("record=%+v err=%v", after, err)
			}
		})
	}
}

func TestWorkspaceEvidencePendingPublication(t *testing.T) {
	for _, status := range []string{"completed", "failed"} {
		for _, evidence := range []string{"legacy", "context", "safe"} {
			t.Run(status+"/"+evidence, func(t *testing.T) {
				ctx := context.Background()
				f := newReviewFixture(t, reviewTestHead)
				if evidence == "safe" {
					f.defaults.Ignore = nil
					f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
					if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", IndexSHA: strings.Repeat("2", 40)}); err != nil {
						t.Fatal(err)
					}
				}
				var id string
				if status == "completed" {
					id = reviewWithFailedNote(t, f, "evidence-publish").ID
				} else {
					f.defaults.PostFailureNote = true
					f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
					f.gitlab.noteErr = errors.New("synthetic failure")
					f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
					job := f.seedReview(t, "evidence-failed")
					if err := f.service.RunReview(ctx, job); err == nil {
						t.Fatal("expected publication failure")
					}
					f.gitlab.noteErr = nil
					wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
					if err != nil {
						t.Fatal(err)
					}
					id = wf.ID
				}
				if err := f.db.SetWorkflowPublicationState(ctx, id, store.WorkflowPublicationPending, ""); err != nil {
					t.Fatal(err)
				}
				if evidence == "safe" {
					record, err := f.db.GetWorkflowDossier(ctx, id)
					if err != nil {
						t.Fatal(err)
					}
					var d review.Dossier
					if err := json.Unmarshal(record.Data, &d); err != nil || !d.MCPIndexEligible() {
						t.Fatalf("safe snapshot missing index evidence: %+v err=%v", d.WorkspaceProvenance, err)
					}
				}
				if evidence != "safe" {
					rewriteWorkspaceEvidence(t, f, id, func(d *review.Dossier) {
						if evidence == "legacy" {
							d.WorkspaceProvenance = nil
						} else {
							d.ContextDocs = "CANARY"
							d.WorkspaceProvenance.ContextBaseSHA = strings.Repeat("1", 40)
						}
					})
				}
				before := countPublicationEffects(f)
				if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "error", IndexSHA: strings.Repeat("9", 40)}); err != nil {
					t.Fatal(err)
				}
				if err := f.service.PublishPending(ctx); err != nil {
					t.Fatal(err)
				}
				wf, err := f.db.GetWorkflow(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				if evidence != "safe" {
					if wf.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(wf.PublicationError, "rerun") || countPublicationEffects(f) != before {
						t.Fatalf("unsafe publication: %+v", wf)
					}
				} else if wf.PublicationState != store.WorkflowPublicationPublished {
					t.Fatalf("safe snapshot not published: %+v", wf)
				}
			})
		}
	}
}
