package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func setRepositorySkillSetting(t *testing.T, f *reviewFixture, enabled bool) {
	t.Helper()
	settings, err := json.Marshal(map[string]bool{"allow_repo_skills": enabled})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.UpdateProject(context.Background(), 42, store.ProjectUpdate{Settings: settings}); err != nil {
		t.Fatal(err)
	}
}

func repositorySkillWorkflow(t *testing.T, f *reviewFixture, origin string) string {
	t.Helper()
	ctx := context.Background()
	job := f.seedReview(t, "legacy-repository-skill")
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatal(err)
	}
	configHash := reviewConfigHash(f.service.resolveConfig(project, review.RepoDocs{}))
	dossier := testDossier(t, f.head)
	dossier.Metadata = map[string]string{
		"config_hash": configHash, "prompt_hash": f.bundle.Hash,
		"profile_hash":       f.service.reviewProfileHash(review.Profile{Name: "default", Config: f.profiles["default"]}),
		"workspace_identity": "absent", "requested_profile": "default", "profile_explicit": "false",
		"execution_identity": testExecutionIdentity(t, f),
	}
	raw, err := review.MarshalDossier(dossier)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]any
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	switch origin {
	case "legacy":
		delete(data, "repository_skill_provenance")
	case "none":
		data["repository_skill_provenance"] = map[string]any{"version": 1, "mode": "none"}
	case "unknown":
		data["repository_skill_provenance"] = map[string]any{"version": 99, "mode": "used"}
	case "repository":
		data["repository_skill_provenance"] = map[string]any{"version": 1, "mode": "repository", "target_branch": "main", "target_sha": strings.Repeat("3", 40), "grant_revisions": map[string]string{"code-review": strings.Repeat("a", 64)}, "snapshot_sha256": strings.Repeat("b", 64)}
	default:
		t.Fatalf("unknown origin %q", origin)
	}
	raw, err = json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	hash := sha256Hex(string(raw))
	spec, _ := testGraph(t, dossier)
	id, _, err := f.db.CreateReviewWorkflow(ctx, sqlite.ReviewWorkflowRequest{
		JobID: job.ID, Spec: spec,
		Meta: map[string]string{"trigger": "mr_open", "base_sha": dossier.DiffVersion.BaseSHA,
			"fingerprint": reviewFingerprint(hash, configHash, f.bundle.Hash), "config_hash": configHash,
			"prompt_hash": f.bundle.Hash, "profile": "default"},
		DossierVersion: review.DossierSchemaVersion, DossierHash: hash, DossierData: raw,
		ExpectedGeneration: jobGeneration(t, job), ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestRepositorySkillLegacyRecoveryFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name       string
		enabled    bool
		origin     string
		wantStatus string
	}{
		{"disabled-legacy", false, "legacy", "cancelled"},
		{"enabled-legacy", true, "legacy", "cancelled"},
		{"disabled-explicit-none", false, "none", "completed"},
		{"enabled-explicit-none", true, "none", "completed"},
		{"unknown-origin", false, "unknown", "cancelled"},
		{"repository-not-wired", true, "repository", "cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			setRepositorySkillSetting(t, f, tc.enabled)
			id := repositorySkillWorkflow(t, f, tc.origin)
			if err := f.service.Recover(ctx); err != nil {
				t.Errorf("Recover: %v", err)
			}
			wf, err := f.db.GetWorkflow(ctx, id)
			if err != nil || wf.Status != tc.wantStatus {
				t.Errorf("recovered workflow = %+v, %v, want %s", wf, err, tc.wantStatus)
			}
			if tc.wantStatus == "cancelled" && f.runner.count() != 0 {
				t.Fatal("unknown repository origin reached an agent")
			}
		})
	}
}

// Build a terminal legacy workflow through the engine, as a pre-upgrade
// process could, without asking the new review service to resume it.
func completeRepositorySkillWorkflow(t *testing.T, f *reviewFixture, origin string) string {
	t.Helper()
	ctx := context.Background()
	id := repositorySkillWorkflow(t, f, origin)
	dossier := testDossier(t, f.head)
	index := gitlab.BuildDiffIndex(dossier.DiffVersion, dossierDiffs(dossier))
	executor, err := review.NewExecutor(review.ExecutorDeps{
		Dossier: dossier, Profile: review.Profile{Name: "default", Config: f.profiles["default"]},
		Graph: review.GraphConfig{AllowedProviders: []string{"fake"}}, Prompts: f.bundle,
		AgentRun: f.runner.Run, Snapshot: engineSnapshot(f.engine), Locate: locateFunc(index),
	})
	if err != nil {
		t.Fatal(err)
	}
	f.executors.Register(id, executor)
	defer f.executors.Unregister(id)
	if err := f.engine.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflow(ctx, id)
	if err != nil || wf.Status != string(dag.WorkflowCompleted) {
		t.Fatalf("legacy setup = %+v, %v", wf, err)
	}
	return id
}

func TestRepositorySkillLegacyPublicationFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		origin  string
		enabled bool
	}{
		{"legacy", false}, {"legacy", true}, {"unknown", false}, {"repository", true},
	} {
		for _, mode := range []string{"pending", "manual", "direct"} {
			t.Run(tc.origin+"/"+mode+"/"+map[bool]string{true: "enabled", false: "disabled"}[tc.enabled], func(t *testing.T) {
				f := newReviewFixture(t, reviewTestHead)
				setRepositorySkillSetting(t, f, tc.enabled)
				id := completeRepositorySkillWorkflow(t, f, tc.origin)
				before := countPublicationEffects(f)
				record, err := f.db.GetWorkflowDossier(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := f.engine.Snapshot(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				result, err := synthesisResult(snapshot)
				if err != nil || len(result.Groups) == 0 {
					t.Fatalf("missing setup findings: %v", err)
				}
				if err := f.db.ReplaceFindings(context.Background(), id, findingsForStore(id, result.Groups)); err != nil {
					t.Fatal(err)
				}
				findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: id})
				if err != nil || len(findings) == 0 {
					t.Fatalf("missing stored findings: %v", err)
				}
				if _, err := f.db.SetFindingAnnotation(context.Background(), id, findings[0].ID, store.FindingAnnotationValid, "reviewer"); err != nil {
					t.Fatal(err)
				}
				findings, err = f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: id})
				if err != nil || len(findings) == 0 || findings[0].Annotation != store.FindingAnnotationValid {
					t.Fatalf("missing manual annotation: %v", err)
				}
				switch mode {
				case "pending":
					err = f.service.PublishPending(context.Background())
				case "manual":
					err = f.publisher.Republish(context.Background(), id)
				case "direct":
					err = f.publisher.Publish(context.Background(), id)
				}
				if (mode == "pending" && err != nil) || (mode != "pending" && !errors.Is(err, ErrStalePublication)) {
					t.Fatalf("publication = %v", err)
				}
				if got := countPublicationEffects(f); got != before {
					t.Fatalf("legacy publication emitted effects: before=%+v after=%+v", before, got)
				}
				wf, err := f.db.GetWorkflow(context.Background(), id)
				if err != nil || wf.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(wf.PublicationError, "repository skill provenance") {
					t.Fatalf("publication state = %+v, %v", wf, err)
				}
				after, err := f.engine.Snapshot(context.Background(), id)
				if err != nil {
					t.Fatal(err)
				}
				preserved, err := synthesisResult(after)
				if err != nil || len(preserved.Groups) != len(result.Groups) {
					t.Fatalf("finding evidence lost: %+v, %v", preserved, err)
				}
				storedFindings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: id})
				if err != nil || !reflect.DeepEqual(findings, storedFindings) {
					t.Fatalf("stored findings changed: %+v, %v", storedFindings, err)
				}
				afterRecord, err := f.db.GetWorkflowDossier(context.Background(), id)
				if err != nil || !bytes.Equal(record.Data, afterRecord.Data) {
					t.Fatalf("dossier evidence changed: %v", err)
				}
			})
		}
	}
}

func TestRepositorySkillTerminalRetryPreservesEvidence(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := completeRepositorySkillWorkflow(t, f, "legacy")
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.db.GetJob(ctx, record.JobID)
	if err != nil {
		t.Fatal(err)
	}
	before, calls := countPublicationEffects(f), f.runner.count()
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	workflow, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || workflow.ID != id || workflow.Status != "completed" || workflow.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("workflow evidence changed: %+v, %v", workflow, err)
	}
	if countPublicationEffects(f) != before || f.runner.count() != calls {
		t.Fatal("terminal legacy workflow sent content")
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.State != reviewStatusStale {
		t.Fatalf("terminal legacy status = %+v, %v", status, err)
	}
	after, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || !bytes.Equal(record.Data, after.Data) {
		t.Fatalf("dossier evidence changed: %v", err)
	}
}

func TestRepositorySkillRetryKeepsUnknownDiscussionPending(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := completeRepositorySkillWorkflow(t, f, "legacy")
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.db.GetJob(ctx, record.JobID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.engine.Snapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	result, err := synthesisResult(snapshot)
	if err != nil || len(result.Groups) == 0 {
		t.Fatalf("missing setup findings: %v", err)
	}
	if err := f.db.ReplaceFindings(ctx, id, findingsForStore(id, result.Groups)); err != nil {
		t.Fatal(err)
	}
	findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: id})
	if err != nil || len(findings) == 0 {
		t.Fatalf("missing stored findings: %v", err)
	}
	if _, err := f.db.SetFindingAnnotation(ctx, id, findings[0].ID, store.FindingAnnotationValid, "reviewer"); err != nil {
		t.Fatal(err)
	}
	findings, err = f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: id})
	if err != nil || len(findings) == 0 || findings[0].Annotation != store.FindingAnnotationValid {
		t.Fatalf("missing annotation: %v", err)
	}
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, "unknown"); err != nil || !created {
		t.Fatalf("reserve uncertain discussion: %v", err)
	}
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	effects, calls := countPublicationEffects(f), f.runner.count()
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.PublicationState != store.WorkflowPublicationPending || wf.PublicationError != store.WorkflowPublicationUnknownReason {
		t.Fatalf("uncertainty was settled: %+v, %v", wf, err)
	}
	after, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || after.Generation != before.Generation || after.State != reviewStatusStale || !strings.Contains(after.Detail, store.WorkflowPublicationUnknownReason) {
		t.Fatalf("uncertain status = %+v, %v", after, err)
	}
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil || !unknown || countPublicationEffects(f) != effects || f.runner.count() != calls {
		t.Fatalf("unknown=%v effects=%+v runner_calls=%d, err=%v", unknown, countPublicationEffects(f), f.runner.count(), err)
	}
	afterRecord, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || !bytes.Equal(record.Data, afterRecord.Data) {
		t.Fatalf("dossier evidence changed: %v", err)
	}
	afterFindings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: id})
	if err != nil || !reflect.DeepEqual(findings, afterFindings) {
		t.Fatalf("annotated findings changed: %+v, %v", afterFindings, err)
	}
}

func TestRepositorySkillFailedPublicationRequiresProvenance(t *testing.T) {
	for _, mode := range []string{"pending", "manual", "direct"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.defaults.PostFailureNote = false
			f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
			f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
			job := f.seedReview(t, "legacy-failure")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil || wf.Status != "failed" {
				t.Fatalf("failed workflow: %+v, %v", wf, err)
			}
			record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]json.RawMessage
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			delete(data, "repository_skill_provenance")
			record.Data, err = json.Marshal(data)
			if err != nil {
				t.Fatal(err)
			}
			record.Hash = ""
			if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
				t.Fatal(err)
			}
			if err := f.db.SetWorkflowPublicationState(ctx, wf.ID, store.WorkflowPublicationPending, ""); err != nil {
				t.Fatal(err)
			}
			before := countPublicationEffects(f)
			switch mode {
			case "pending":
				err = f.service.PublishPending(ctx)
			case "manual":
				err = f.publisher.Republish(ctx, wf.ID)
			case "direct":
				err = f.publisher.PublishFailure(ctx, wf.ID, "failure")
			}
			if (mode == "pending" && err != nil) || (mode != "pending" && !errors.Is(err, ErrStalePublication)) {
				t.Fatalf("publication: %v", err)
			}
			after, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil || after.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(after.PublicationError, "repository skill provenance") || !bytes.Equal(record.Data, after.Data) || countPublicationEffects(f) != before {
				t.Fatalf("unsafe publication or lost evidence: %+v, %v", after, err)
			}
		})
	}
}

func TestRepositorySkillExplicitNonePublication(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(map[bool]string{true: "enabled", false: "disabled"}[enabled], func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			setRepositorySkillSetting(t, f, enabled)
			id := completeRepositorySkillWorkflow(t, f, "none")
			if err := f.publisher.Publish(context.Background(), id); err != nil {
				t.Fatal(err)
			}
			if got := countPublicationEffects(f); got.summaries == 0 {
				t.Fatalf("no publication: %+v", got)
			}
		})
	}
}

func TestRepositorySkillRetryDoesNotReuseUnknownProvenance(t *testing.T) {
	for _, origin := range []string{"legacy", "unknown", "repository"} {
		t.Run(origin, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			id := repositorySkillWorkflow(t, f, origin)
			record, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			job, err := f.db.GetJob(ctx, record.JobID)
			if err != nil {
				t.Fatal(err)
			}
			before := countPublicationEffects(f)
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			if f.runner.count() != 0 || countPublicationEffects(f) != before {
				t.Fatal("unproven workflow sent content")
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil || wf.ID != id || wf.Status != "cancelled" {
				t.Fatalf("workflow evidence replaced: %+v, %v", wf, err)
			}
			status, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || status.State != reviewStatusStale {
				t.Fatalf("legacy retry status = %+v, %v", status, err)
			}
		})
	}
}

func TestNewReviewRecordsRepositoryCapabilityNone(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	setRepositorySkillSetting(t, f, true)
	job := f.seedReview(t, "fresh-origin")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(record.Data, &raw); err != nil {
		t.Fatal(err)
	}
	var marker struct {
		Version int    `json:"version"`
		Mode    string `json:"mode"`
	}
	if err := json.Unmarshal(raw["repository_skill_provenance"], &marker); err != nil || marker.Version != 1 || marker.Mode != "none" {
		t.Fatalf("missing explicit none: %s, %v", record.Data, err)
	}
}
