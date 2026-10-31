package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// Add a source note without re-validating the intentionally legacy dossier.
func seedProvenanceOutcome(t *testing.T, f *reviewFixture, id string, state string) {
	t.Helper()
	ctx := context.Background()
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var dossier map[string]json.RawMessage
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(dossier["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["note_id"] = "555"
	dossier["metadata"], err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record.Data, err = json.Marshal(dossier)
	if err != nil {
		t.Fatal(err)
	}
	record.Hash = ""
	if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
		t.Fatal(err)
	}
	current, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: current.Generation, Attempt: current.Attempt + 1,
		HeadSHA: f.head, State: state,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetReviewReaction(ctx, store.ReviewReactionGoal{
		ProjectID: 42, MRIID: 7, NoteID: 555, Generation: current.Generation, Desired: reviewAwardEyes,
	}); err != nil {
		t.Fatal(err)
	}
}

func assertProvenanceOutcome(t *testing.T, f *reviewFixture, id, workflow, publication, state, reaction, detail string) {
	t.Helper()
	ctx := context.Background()
	wf, err := f.db.GetWorkflow(ctx, id)
	if err != nil || wf.Status != workflow || wf.PublicationState != publication {
		t.Errorf("workflow = %+v, %v; want %s/%s", wf, err, workflow, publication)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.State != state || (detail != "" && !strings.Contains(status.Detail, detail)) {
		t.Errorf("status = %+v, %v; want %s with %q", status, err, state, detail)
	}
	if err == nil && detail != "" {
		body := reviewStatusBody(reviewStatusUpdate{State: status.State, Detail: status.Detail})
		if !strings.Contains(body, detail) {
			t.Errorf("rendered status %q does not show %q", body, detail)
		}
	}
	goal, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || goal.Desired != reaction {
		t.Errorf("reaction = %+v, %v; want %s", goal, err, reaction)
	}
}

func TestProvenanceRecoverySettlesVisibleOutcome(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := repositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusRunning)
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertProvenanceOutcome(t, f, id, "cancelled", store.WorkflowPublicationSkipped, reviewStatusStale, reviewAwardNone, "provenance")
	if f.runner.count() != 0 || countPublicationEffects(f).summaries != 0 {
		t.Fatal("rejected workflow reached execution or publication")
	}
}

func TestProvenanceRetryKeepsTerminalExecutionOutcome(t *testing.T) {
	for _, execution := range []string{"failed", "cancelled"} {
		t.Run(execution, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			var id string
			if execution == "failed" {
				f.defaults.PostFailureNote = false
				f.service.reviewDefaults = f.defaults
				f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
				job := f.seedReview(t, "failed-provenance")
				if err := f.service.RunReview(ctx, job); err != nil {
					t.Fatal(err)
				}
				wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
				if err != nil || wf.Status != "failed" {
					t.Fatalf("fixture workflow = %+v, %v", wf, err)
				}
				id = wf.ID
				record, err := f.db.GetWorkflowDossier(ctx, id)
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
				if err := f.db.SetWorkflowPublicationState(ctx, id, store.WorkflowPublicationPending, ""); err != nil {
					t.Fatal(err)
				}
			} else {
				id = repositorySkillWorkflow(t, f, "legacy")
				if err := f.engine.Cancel(ctx, id, "user_cancelled"); err != nil {
					t.Fatal(err)
				}
			}
			seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
			record, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			job, err := f.db.GetJob(ctx, record.JobID)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			reaction := reviewAwardFail
			if execution == "cancelled" {
				reaction = reviewAwardNone
			}
			assertProvenanceOutcome(t, f, id, execution, store.WorkflowPublicationSkipped, execution, reaction, "")
		})
	}
}

func TestProvenanceUnknownDiscussionDoesNotReportPublishing(t *testing.T) {
	for _, mode := range []string{"pending", "manual", "direct"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			id := completeRepositorySkillWorkflow(t, f, "legacy")
			seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
			if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, "unknown"); err != nil || !created {
				t.Fatalf("reserve = %v, %v", created, err)
			}
			before := countPublicationEffects(f)
			var err error
			switch mode {
			case "pending":
				err = f.service.PublishPending(ctx)
			case "manual":
				err = f.service.RepublishReview(ctx, manualPublicationJob(t, f, id))
			case "direct":
				err = f.publisher.Publish(ctx, id)
			}
			if !errors.Is(err, errDiscussionUnknown) {
				t.Fatalf("%s = %v, want unknown discussion", mode, err)
			}
			assertProvenanceOutcome(t, f, id, "completed", store.WorkflowPublicationPending, reviewStatusStale, reviewAwardNone, "discussion outcome unknown")
			unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
			if err != nil || !unknown || countPublicationEffects(f) != before {
				t.Fatalf("unknown=%v, effects=%+v, err=%v", unknown, countPublicationEffects(f), err)
			}
		})
	}
}

var errProvenanceStatusCrash = errors.New("injected provenance outcome crash")

type provenanceOutcomeCrashStore struct {
	ReviewStore
	crashReaction bool
	crashState    string
}

func (s provenanceOutcomeCrashStore) SetReviewStatus(ctx context.Context, goal store.ReviewStatusGoal) (bool, error) {
	state := s.crashState
	if state == "" {
		state = reviewStatusStale
	}
	if !s.crashReaction && goal.State == state {
		return false, errProvenanceStatusCrash
	}
	return s.ReviewStore.SetReviewStatus(ctx, goal)
}

func (s provenanceOutcomeCrashStore) SetReviewReaction(ctx context.Context, goal store.ReviewReactionGoal) (bool, error) {
	if s.crashReaction && goal.Desired == reviewAwardNone {
		return false, errProvenanceStatusCrash
	}
	return s.ReviewStore.SetReviewReaction(ctx, goal)
}

func TestProvenanceRestartRepairsOutcomeAfterCrash(t *testing.T) {
	for _, mode := range []string{"recovery", "pending", "manual", "direct"} {
		for _, reaction := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/reaction=%t", mode, reaction), func(t *testing.T) {
				ctx := context.Background()
				f := newReviewFixture(t, reviewTestHead)
				id, workflow := "", "completed"
				if mode == "recovery" {
					id, workflow = repositorySkillWorkflow(t, f, "legacy"), "cancelled"
				} else {
					id = completeRepositorySkillWorkflow(t, f, "legacy")
				}
				seedProvenanceOutcome(t, f, id, reviewStatusRunning)
				f.service.store = provenanceOutcomeCrashStore{ReviewStore: f.db, crashReaction: reaction}
				var err error
				switch mode {
				case "recovery":
					err = f.service.Recover(ctx)
				case "pending":
					err = f.service.PublishPending(ctx)
				case "manual":
					err = f.service.RepublishReview(ctx, manualPublicationJob(t, f, id))
				case "direct":
					f.publisher.store = f.service.store
					err = f.publisher.Publish(ctx, id)
				}
				if !errors.Is(err, errProvenanceStatusCrash) {
					t.Fatalf("injected crash = %v", err)
				}
				wf, err := f.db.GetWorkflow(ctx, id)
				if err != nil || wf.Status != workflow || wf.PublicationState != store.WorkflowPublicationSkipped {
					t.Fatalf("durable workflow after crash = %+v, %v", wf, err)
				}
				f.service.store = f.db
				f.publisher.store = f.db
				if err := f.service.Recover(ctx); err != nil {
					t.Fatal(err)
				}
				assertProvenanceOutcome(t, f, id, workflow, store.WorkflowPublicationSkipped, reviewStatusStale, reviewAwardNone, "provenance")
			})
		}
	}
}

func TestProvenanceRestartAfterCancellationBeforeSkip(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := repositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusRunning)
	if err := f.engine.Cancel(ctx, id, repositorySkillEvidenceReason); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertProvenanceOutcome(t, f, id, "cancelled", store.WorkflowPublicationSkipped, reviewStatusStale, reviewAwardNone, "provenance")
}

func TestProvenanceRestartRepairsUnknownAfterStatusCrash(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := completeRepositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, "unknown"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	f.service.store = provenanceOutcomeCrashStore{ReviewStore: f.db}
	if err := f.service.PublishPending(ctx); !errors.Is(err, errProvenanceStatusCrash) {
		t.Fatalf("injected crash = %v", err)
	}
	f.service.store = f.db
	if err := f.service.Recover(ctx); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("recovery = %v", err)
	}
	assertProvenanceOutcome(t, f, id, "completed", store.WorkflowPublicationPending, reviewStatusStale, reviewAwardNone, "discussion outcome unknown")
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil || !unknown {
		t.Fatalf("unknown = %v, %v", unknown, err)
	}
}

func TestProvenanceRestartRepairsPreviouslySkippedFailure(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.PostFailureNote = false
	f.service.reviewDefaults = f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
	job := f.seedReview(t, "failed-existing-skip")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.Status != "failed" || wf.PublicationError != failureNoteDisabledReason {
		t.Fatalf("workflow = %+v, %v", wf, err)
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
	seedProvenanceOutcome(t, f, wf.ID, reviewStatusPublishing)
	f.service.store = provenanceOutcomeCrashStore{ReviewStore: f.db, crashState: reviewStatusFailed}
	if err := f.service.RunReview(ctx, job); !errors.Is(err, errProvenanceStatusCrash) {
		t.Fatalf("injected crash = %v", err)
	}
	f.service.store = f.db
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertProvenanceOutcome(t, f, wf.ID, "failed", store.WorkflowPublicationSkipped, reviewStatusFailed, reviewAwardFail, "")
}

func TestProvenanceConcurrentPublicationSettlesSameGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	id := completeRepositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
	job := manualPublicationJob(t, f, id)
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- f.service.PublishPending(ctx) }()
	go func() { <-start; results <- f.service.RepublishReview(ctx, job) }()
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, ErrStalePublication) {
				t.Errorf("concurrent publication = %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	assertProvenanceOutcome(t, f, id, "completed", store.WorkflowPublicationSkipped, reviewStatusStale, reviewAwardNone, "provenance")
}

func TestProvenanceConcurrentPublicationAndNewGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	id := completeRepositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
	job := manualPublicationJob(t, f, id)
	newer, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() { <-start; results <- f.service.PublishPending(ctx) }()
	go func() { <-start; results <- f.service.RepublishReview(ctx, job) }()
	close(start)
	for i := 0; i < 2; i++ {
		select {
		case err := <-results:
			if err != nil && !errors.Is(err, ErrStalePublication) {
				t.Errorf("concurrent publication = %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.Generation != newer.Generation || status.State != reviewStatusQueued {
		t.Fatalf("newer status = %+v, %v", status, err)
	}
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Generation != newer.Generation || reaction.Desired != reviewAwardEyes {
		t.Fatalf("newer reaction = %+v, %v", reaction, err)
	}
	if countPublicationEffects(f).summaries != 0 {
		t.Fatal("unproven workflow published a summary")
	}
}

func TestProvenanceDirectPublicationSettlesStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := completeRepositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
	if err := f.publisher.Publish(ctx, id); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("publish = %v", err)
	}
	assertProvenanceOutcome(t, f, id, "completed", store.WorkflowPublicationSkipped, reviewStatusStale, reviewAwardNone, "provenance")
}

func TestProvenanceUnknownDoesNotInferSuccessFromPublishedRow(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := completeRepositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
	if err := f.db.SetWorkflowPublicationState(ctx, id, store.WorkflowPublicationPublished, ""); err != nil {
		t.Fatal(err)
	}
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, "unknown"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	if err := f.publisher.Publish(ctx, id); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("publish = %v", err)
	}
	assertProvenanceOutcome(t, f, id, "completed", store.WorkflowPublicationPublished, reviewStatusStale, reviewAwardNone, "discussion outcome unknown")
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil || !unknown {
		t.Fatalf("unknown = %v, %v", unknown, err)
	}
}

func TestProvenanceCancelledWithUnknownDiscussionKeepsCancellation(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := repositorySkillWorkflow(t, f, "legacy")
	if err := f.engine.Cancel(ctx, id, "user_cancelled"); err != nil {
		t.Fatal(err)
	}
	seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, "unknown"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	if err := f.service.PublishPending(ctx); err != nil {
		t.Fatal(err)
	}
	assertProvenanceOutcome(t, f, id, "cancelled", store.WorkflowPublicationPending, reviewStatusCancelled, reviewAwardNone, "discussion outcome unknown")
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil || !unknown {
		t.Fatalf("unknown = %v, %v", unknown, err)
	}
}

type provenanceSwitchStore struct {
	ReviewStore
	reads    int
	switchAt int
}

func (s *provenanceSwitchStore) GetWorkflowDossier(ctx context.Context, id string) (*store.WorkflowDossier, error) {
	s.reads++
	record, err := s.ReviewStore.GetWorkflowDossier(ctx, id)
	if err != nil || s.reads != s.switchAt {
		return record, err
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(record.Data, &data); err != nil {
		return nil, err
	}
	delete(data, "repository_skill_provenance")
	record.Data, err = json.Marshal(data)
	if err != nil {
		return nil, err
	}
	record.Hash = ""
	if _, err := s.ReviewStore.SaveWorkflowDossier(ctx, *record); err != nil {
		return nil, err
	}
	return s.ReviewStore.GetWorkflowDossier(ctx, id)
}

func TestProvenanceChangeDuringPublicationIsSettled(t *testing.T) {
	for _, mode := range []string{"direct", "manual"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			id := completeRepositorySkillWorkflow(t, f, "none")
			seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
			if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, "unknown"); err != nil || !created {
				t.Fatalf("reserve = %v, %v", created, err)
			}
			f.publisher.store = &provenanceSwitchStore{ReviewStore: f.db, switchAt: 2}
			var err error
			if mode == "direct" {
				err = f.publisher.Publish(ctx, id)
			} else {
				err = f.service.RepublishReview(ctx, manualPublicationJob(t, f, id))
			}
			if !errors.Is(err, errDiscussionUnknown) {
				t.Fatalf("publication = %v", err)
			}
			assertProvenanceOutcome(t, f, id, "completed", store.WorkflowPublicationPending, reviewStatusStale, reviewAwardNone, "discussion outcome unknown")
		})
	}
}

func TestProvenanceChangeDuringFailedPendingPublicationIsSettled(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprintf("unknown=%t", unknown), func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.defaults.PostFailureNote = false
			f.service.reviewDefaults = f.defaults
			f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
			job := f.seedReview(t, "failed-pending-change")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil || wf.Status != "failed" {
				t.Fatalf("workflow = %+v, %v", wf, err)
			}
			if err := f.db.SetWorkflowPublicationState(ctx, wf.ID, store.WorkflowPublicationPending, ""); err != nil {
				t.Fatal(err)
			}
			seedProvenanceOutcome(t, f, wf.ID, reviewStatusPublishing)
			if unknown {
				if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, "unknown"); err != nil || !created {
					t.Fatalf("reserve = %v, %v", created, err)
				}
			}
			f.service.store = &provenanceSwitchStore{ReviewStore: f.db, switchAt: 3}
			err = f.service.PublishPending(ctx)
			if (unknown && !errors.Is(err, errDiscussionUnknown)) || (!unknown && err != nil) {
				t.Fatalf("pending publication = %v", err)
			}
			publication, detail := store.WorkflowPublicationSkipped, ""
			if unknown {
				publication, detail = store.WorkflowPublicationPending, store.WorkflowPublicationUnknownReason
				stillUnknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
				if err != nil || !stillUnknown {
					t.Fatalf("unknown = %v, %v", stillUnknown, err)
				}
			}
			assertProvenanceOutcome(t, f, wf.ID, "failed", publication, reviewStatusFailed, reviewAwardFail, detail)
		})
	}
}

func corruptProvenanceIdentity(t *testing.T, f *reviewFixture, id, kind string) {
	t.Helper()
	ctx := context.Background()
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(record.Data, &data); err != nil {
		t.Fatal(err)
	}
	delete(data, "repository_skill_provenance")
	switch kind {
	case "missing":
		delete(data, "project")
	case "project":
		data["project"] = json.RawMessage(`{"id":43,"path":"another/project"}`)
	case "mr":
		data["mr"] = json.RawMessage(`{"iid":8}`)
	case "head":
		data["diff_version"] = json.RawMessage(`{"HeadSHA":"cccccccccccccccccccccccccccccccccccccccc"}`)
	default:
		t.Fatalf("unknown identity corruption %q", kind)
	}
	record.Data, err = json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	record.Hash = ""
	if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
		t.Fatal(err)
	}
}

func TestProvenanceMissingOrMismatchedIdentityDoesNotGuessReaction(t *testing.T) {
	for _, kind := range []string{"missing", "project", "mr", "head"} {
		t.Run(kind, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			id := repositorySkillWorkflow(t, f, "legacy")
			seedProvenanceOutcome(t, f, id, reviewStatusRunning)
			corruptProvenanceIdentity(t, f, id, kind)
			if err := f.service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			assertProvenanceOutcome(t, f, id, "cancelled", store.WorkflowPublicationSkipped, reviewStatusStale, reviewAwardEyes, "reaction identity unavailable")
		})
	}
}

func TestProvenanceMismatchedIdentityUsesCorroboratedJobNoteOnly(t *testing.T) {
	ctx := context.Background()
	f, wf := commandReviewWithPendingPublication(t)
	seedProvenanceOutcome(t, f, wf.ID, reviewStatusPublishing)
	corruptProvenanceIdentity(t, f, wf.ID, "project")
	record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	var data map[string]json.RawMessage
	if err := json.Unmarshal(record.Data, &data); err != nil {
		t.Fatal(err)
	}
	var metadata map[string]string
	if err := json.Unmarshal(data["metadata"], &metadata); err != nil {
		t.Fatal(err)
	}
	metadata["note_id"] = "999"
	data["metadata"], err = json.Marshal(metadata)
	if err != nil {
		t.Fatal(err)
	}
	record.Data, err = json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	record.Hash = ""
	if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetReviewReaction(ctx, store.ReviewReactionGoal{ProjectID: 42, MRIID: 7, NoteID: 999, Generation: record.Generation, Desired: reviewAwardEyes}); err != nil {
		t.Fatal(err)
	}
	seedProject(t, f.db, f.publisher.cipher, 43, "whsec", "{}", "default")
	other, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 43, MRIID: 7, HeadSHA: f.head, Payload: json.RawMessage(`{"trigger":"command","note_id":555}`)})
	if err != nil {
		t.Fatal(err)
	}
	effects := countPublicationEffects(f)
	if err := f.service.PublishPending(ctx); err != nil {
		t.Fatal(err)
	}
	assertProvenanceOutcome(t, f, wf.ID, "completed", store.WorkflowPublicationSkipped, reviewStatusStale, reviewAwardNone, "provenance")
	forged, err := f.db.GetReviewReaction(ctx, 42, 7, 999)
	if err != nil || forged.Desired != reviewAwardEyes {
		t.Fatalf("forged note reaction = %+v, %v", forged, err)
	}
	otherStatus, err := f.db.GetReviewStatus(ctx, 43, 7)
	if err != nil || otherStatus.Generation != other.Generation || otherStatus.State != reviewStatusQueued {
		t.Fatalf("other project status = %+v, %v", otherStatus, err)
	}
	otherReaction, err := f.db.GetReviewReaction(ctx, 43, 7, 555)
	if err != nil || otherReaction.Generation != other.Generation || otherReaction.Desired != reviewAwardEyes || countPublicationEffects(f) != effects {
		t.Fatalf("other project reaction = %+v, %v", otherReaction, err)
	}
}

func TestProvenanceMismatchedIdentityCannotSettleSuccessor(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := repositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusRunning)
	corruptProvenanceIdentity(t, f, id, "head")
	newer, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: strings.Repeat("c", 40), Payload: json.RawMessage(`{"trigger":"command","note_id":555}`)})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.Generation != newer.Generation || status.State != reviewStatusQueued {
		t.Fatalf("successor status = %+v, %v", status, err)
	}
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Generation != newer.Generation || reaction.Desired != reviewAwardEyes {
		t.Fatalf("successor reaction = %+v, %v", reaction, err)
	}
}

func TestProvenanceMismatchedIdentityCannotUseOtherProjectJob(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := repositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusRunning)
	corruptProvenanceIdentity(t, f, id, "project")
	seedProject(t, f.db, f.publisher.cipher, 43, "whsec", "{}", "default")
	other, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 43, MRIID: 7, HeadSHA: f.head, Payload: json.RawMessage(`{"trigger":"command","note_id":999}`)})
	if err != nil {
		t.Fatal(err)
	}
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	record.JobID = other.JobID
	if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	assertProvenanceOutcome(t, f, id, "cancelled", store.WorkflowPublicationSkipped, reviewStatusStale, reviewAwardEyes, "reaction identity unavailable")
	otherStatus, err := f.db.GetReviewStatus(ctx, 43, 7)
	if err != nil || otherStatus.Generation != other.Generation || otherStatus.State != reviewStatusQueued {
		t.Fatalf("other project status = %+v, %v", otherStatus, err)
	}
	otherReaction, err := f.db.GetReviewReaction(ctx, 43, 7, 999)
	if err != nil || otherReaction.Generation != other.Generation || otherReaction.Desired != reviewAwardEyes {
		t.Fatalf("other project reaction = %+v, %v", otherReaction, err)
	}
}

func TestProvenanceMismatchedIdentityPreservesTerminalFailureWithoutGuessing(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.PostFailureNote = false
	f.service.reviewDefaults = f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
	job := f.seedReview(t, "failed-identity")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.Status != "failed" {
		t.Fatalf("workflow = %+v, %v", wf, err)
	}
	seedProvenanceOutcome(t, f, wf.ID, reviewStatusPublishing)
	corruptProvenanceIdentity(t, f, wf.ID, "mr")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	assertProvenanceOutcome(t, f, wf.ID, "failed", store.WorkflowPublicationSkipped, reviewStatusFailed, reviewAwardEyes, "reaction identity unavailable")
}

func TestProvenanceMismatchedIdentityPreservesUnknown(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	id := completeRepositorySkillWorkflow(t, f, "legacy")
	seedProvenanceOutcome(t, f, id, reviewStatusPublishing)
	corruptProvenanceIdentity(t, f, id, "head")
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, "unknown"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	if err := f.service.PublishPending(ctx); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("pending = %v", err)
	}
	assertProvenanceOutcome(t, f, id, "completed", store.WorkflowPublicationPending, reviewStatusStale, reviewAwardEyes, "discussion outcome unknown")
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || !strings.Contains(status.Detail, "reaction identity unavailable") {
		t.Fatalf("limited status = %+v, %v", status, err)
	}
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil || !unknown {
		t.Fatalf("unknown = %v, %v", unknown, err)
	}
}
