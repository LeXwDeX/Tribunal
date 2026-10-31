package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type invalidateBeforeDiscussion struct {
	ReviewStore
	ws *workspace.Workspace
}

func (s invalidateBeforeDiscussion) GetPublication(ctx context.Context, projectID, mrIID int64, kind, key string) (*store.Publication, error) {
	publication, err := s.ReviewStore.GetPublication(ctx, projectID, mrIID, kind, key)
	if kind == discussionPublishKind {
		if changeErr := s.ws.ChangeOutbound(ctx, func(context.Context) error { return nil }); changeErr != nil {
			return nil, changeErr
		}
	}
	return publication, err
}

func TestDiscussionGateRejectedBeforeReservationCanRetry(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.publisher.retries = 1
	wf := reviewWithFailedNote(t, f, "gate-before-discussion")
	before, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(before) != 1 {
		t.Fatalf("findings = %+v, %v", before, err)
	}
	if _, err := f.db.SetFindingAnnotation(ctx, wf.ID, before[0].ID, store.FindingAnnotationUncertain, "reviewer"); err != nil {
		t.Fatal(err)
	}
	f.publisher.store = invalidateBeforeDiscussion{ReviewStore: f.db, ws: f.manager.Get(42)}
	err = f.publisher.Publish(ctx, wf.ID)
	if !errors.Is(err, workspace.ErrOutboundInvalidated) || errors.Is(err, errDiscussionUnknown) {
		t.Errorf("publish = %v, want invalidated without unknown", err)
	}
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil || unknown || countPublicationEffects(f).discussions != 0 {
		t.Fatalf("pre-send unknown=%v, creates=%d, err=%v", unknown, countPublicationEffects(f).discussions, err)
	}
	f.publisher.store = f.db
	if err := f.service.PublishPending(ctx); err != nil {
		t.Fatalf("retry after invalidation: %v", err)
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPublished {
		t.Errorf("retry state = %s", got)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Errorf("retry created %d discussions", got)
	}
	after, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(after) != 1 || after[0].Evidence != before[0].Evidence || after[0].Annotation != store.FindingAnnotationUncertain {
		t.Fatalf("finding changed: %+v, %v", after, err)
	}
}

func TestPendingRecoveryNotStarvedByCancelledUnknownMR(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.publisher.retries = 1
	wf := reviewWithFailedNote(t, f, "other-project")
	seedProject(t, f.db, f.publisher.cipher, 43, "whsec", "{}", "default")
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 43, 8, "unknown-key"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	spec := dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "n", Kind: "test", Required: true}}}
	meta := map[string]string{"kind": "review", "project_id": "43", "mr_iid": "8", "trigger": "api"}
	ids := make([]string, 0, 55)
	for i := 0; i < 55; i++ {
		id, err := f.engine.Create(ctx, spec, meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.SaveWorkflowDossier(ctx, store.WorkflowDossier{WorkflowID: id, JobID: fmt.Sprintf("cancelled-unknown-%d", i), Version: 1, Data: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := f.engine.Cancel(ctx, id, "crash before skip"); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := f.service.PublishPending(ctx); err != nil {
		t.Fatalf("recovery: %v", err)
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPublished {
		t.Errorf("unrelated completed publication = %s, want published", got)
	}
	for _, id := range ids {
		d, err := f.db.GetWorkflowDossier(ctx, id)
		if err != nil || d.PublicationState != store.WorkflowPublicationPending || d.PublicationError != publicationUnknownReason {
			t.Fatalf("cancelled unknown %s = %+v, %v", id, d, err)
		}
	}
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 43, 8)
	if err != nil || !unknown {
		t.Fatalf("unknown intent lost: %v, %v", unknown, err)
	}
}

func TestPendingRecoveryPaginatesCompletedUnknownMR(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.publisher.retries = 1
	wf := reviewWithFailedNote(t, f, "completed-behind-unknown")
	seedProject(t, f.db, f.publisher.cipher, 43, "whsec", "{}", "default")
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 43, 8, "unknown-key"); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	spec := dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "n", Kind: "test", Required: true}}}
	meta := map[string]string{"kind": "review", "project_id": "43", "mr_iid": "8", "trigger": "api"}
	events := []dag.Event{
		{Type: dag.EventWorkflowStarted, Payload: json.RawMessage(`{}`)},
		{Type: dag.EventNodeQueued, Payload: json.RawMessage(`{"node_id":"n","attempt":1}`)},
		{Type: dag.EventNodeStarted, Payload: json.RawMessage(`{"node_id":"n","attempt":1}`)},
		{Type: dag.EventNodeCompleted, Payload: json.RawMessage(`{"node_id":"n","attempt":1}`)},
		{Type: dag.EventWorkflowCompleted, Payload: json.RawMessage(`{}`)},
	}
	var ids []string
	for i := 0; i < 55; i++ {
		id, err := f.engine.Create(ctx, spec, meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.SaveWorkflowDossier(ctx, store.WorkflowDossier{WorkflowID: id, Version: 1, Data: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if _, err := f.db.Append(ctx, id, 1, events...); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := f.service.PublishPending(ctx); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("recovery = %v, want unknown", err)
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPublished {
		t.Errorf("unrelated completed publication = %s", got)
	}
	for _, id := range ids {
		d, err := f.db.GetWorkflowDossier(ctx, id)
		if err != nil || d.PublicationState != store.WorkflowPublicationPending || d.PublicationError != publicationUnknownReason {
			t.Fatalf("completed unknown %s = %+v, %v", id, d, err)
		}
	}
}

type firstMRFailure struct {
	*reviewGitLab
	calls int
}

func (g *firstMRFailure) GetMergeRequest(ctx context.Context, token string, projectID, mrIID int64) (*gitlab.MergeRequest, error) {
	g.calls++
	if g.calls == 1 {
		return nil, errors.New("temporary merge request error")
	}
	return g.reviewGitLab.GetMergeRequest(ctx, token, projectID, mrIID)
}

func TestPendingRecoveryRetainsBoundedRetryAfterProgress(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.publisher.retries = 1
	wf := reviewWithFailedNote(t, f, "retry-after-progress")
	seedProject(t, f.db, f.publisher.cipher, 43, "whsec", "{}", "default")
	spec := dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "n", Kind: "test", Required: true}}}
	id, err := f.engine.Create(ctx, spec, map[string]string{"kind": "review", "project_id": "43", "mr_iid": "8", "trigger": "api"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SaveWorkflowDossier(ctx, store.WorkflowDossier{WorkflowID: id, Version: 1, Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	if err := f.engine.Cancel(ctx, id, "crash before skip"); err != nil {
		t.Fatal(err)
	}
	api := &firstMRFailure{reviewGitLab: f.gitlab}
	f.publisher.gitlab = api
	if err := f.service.PublishPending(ctx); err == nil {
		t.Error("transient error was not reported")
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPublished || api.calls != 2 {
		t.Fatalf("publication = %s, MR calls = %d; want published after retry", got, api.calls)
	}
}

func TestReconcileCompletedReviewRespectsPublicationState(t *testing.T) {
	for _, tc := range []struct {
		state, reason, wantState, wantDetail, wantReaction string
	}{
		{store.WorkflowPublicationPending, publicationUnknownReason, reviewStatusPublishing, "", reviewAwardEyes},
		{store.WorkflowPublicationPublished, "", reviewStatusPass, "", reviewAwardPass},
		{store.WorkflowPublicationSkipped, "stale: merge request head changed", reviewStatusStale, "review is no longer current", reviewAwardNone},
		{store.WorkflowPublicationFailed, publicationFailedReason, reviewStatusFailed, "publication failed", reviewAwardFail},
	} {
		t.Run(tc.state, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.publisher.retries = 1
			wf := reviewWithFailedNote(t, f, "reconcile-state")
			seedWorkflowNoteID(t, f, wf.ID, 555)
			if err := f.db.SetWorkflowPublicationState(ctx, wf.ID, tc.state, tc.reason); err != nil {
				t.Fatal(err)
			}
			current, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: current.Generation, Attempt: current.Attempt + 1, HeadSHA: f.head, State: reviewStatusPublishing})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.SetReviewReaction(ctx, store.ReviewReactionGoal{ProjectID: 42, MRIID: 7, NoteID: 555, Generation: current.Generation, Desired: reviewAwardEyes}); err != nil {
				t.Fatal(err)
			}
			job, err := f.db.Claim(ctx, "dead-worker", []string{"review.run"}, time.Minute)
			if err != nil || job == nil {
				t.Fatalf("claim = %+v, %v", job, err)
			}
			if err := f.db.Complete(ctx, *job); err != nil {
				t.Fatal(err)
			}
			row, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil {
				t.Fatal(err)
			}
			f.service.store = &stateListStore{Store: f.db, statuses: []store.ReviewStatus{*row}}
			if err := f.service.ReconcileSideEffects(ctx); err != nil {
				t.Fatal(err)
			}
			status, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || status.State != tc.wantState || status.Detail != tc.wantDetail {
				t.Errorf("status = %+v, %v; want %s / %s", status, err, tc.wantState, tc.wantDetail)
			}
			reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
			if err != nil || reaction.Desired != tc.wantReaction {
				t.Errorf("reaction = %+v, %v; want %s", reaction, err, tc.wantReaction)
			}
		})
	}
}

func commandReviewWithPendingPublication(t *testing.T) (*reviewFixture, *store.Workflow) {
	t.Helper()
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.publisher.retries = 1
	result, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head, Payload: json.RawMessage(`{"trigger":"command","profile":"default","full":true,"note_id":555}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := f.db.GetJob(ctx, result.JobID)
	if err != nil {
		t.Fatal(err)
	}
	f.gitlab.noteErr = errors.New("summary unavailable")
	if err := f.service.RunReview(ctx, job); err == nil {
		t.Fatal("review unexpectedly published")
	}
	f.gitlab.noteErr = nil
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || state.State != reviewStatusFailed {
		t.Fatalf("before = %+v, %v", state, err)
	}
	claimed, err := f.db.Claim(ctx, "finished-worker", []string{"review.run"}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	if err := f.db.Complete(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	return f, wf
}

func manualPublicationJob(t *testing.T, f *reviewFixture, workflowID string) *store.Job {
	t.Helper()
	ctx := context.Background()
	id, _, err := f.db.Enqueue(ctx, store.Job{Kind: "review.publish", Key: "review.publish:" + workflowID, Payload: json.RawMessage(fmt.Sprintf(`{"workflow_id":%q}`, workflowID)), MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	publicationJob, err := f.db.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	return publicationJob
}

func TestManualRepublishSettlesPreviouslyFailedReview(t *testing.T) {
	ctx := context.Background()
	f, wf := commandReviewWithPendingPublication(t)
	publicationJob := manualPublicationJob(t, f, wf.ID)
	if err := f.service.RepublishReview(ctx, publicationJob); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || state.State != reviewStatusPass {
		t.Errorf("republished status = %+v, %v; want PASS", state, err)
	}
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardPass {
		t.Errorf("republished reaction = %+v, %v; want pass", reaction, err)
	}
}

var errStatusCrash = errors.New("injected status write failure")

type statusCrashStore struct{ ReviewStore }

func (s statusCrashStore) SetReviewStatus(ctx context.Context, goal store.ReviewStatusGoal) (bool, error) {
	if goal.State == reviewStatusPass {
		return false, errStatusCrash
	}
	return s.ReviewStore.SetReviewStatus(ctx, goal)
}

func TestPublicationStateSurvivesStatusWriteCrash(t *testing.T) {
	for _, manual := range []bool{false, true} {
		t.Run(map[bool]string{false: "startup", true: "manual"}[manual], func(t *testing.T) {
			ctx := context.Background()
			f, wf := commandReviewWithPendingPublication(t)
			job := manualPublicationJob(t, f, wf.ID)
			f.service.store = statusCrashStore{f.db}
			var err error
			if manual {
				err = f.service.RepublishReview(ctx, job)
			} else {
				err = f.service.PublishPending(ctx)
			}
			if !errors.Is(err, errStatusCrash) {
				t.Fatalf("status write = %v", err)
			}
			if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPublished {
				t.Fatalf("publication = %s, want published", got)
			}
			row, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || row.State != reviewStatusPublishing {
				t.Fatalf("status after crash = %+v, %v", row, err)
			}
			f.service.store = &stateListStore{Store: f.db, statuses: []store.ReviewStatus{*row}}
			if err := f.service.ReconcileSideEffects(ctx); err != nil {
				t.Fatal(err)
			}
			status, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || status.State != reviewStatusPass {
				t.Errorf("recovered status = %+v, %v", status, err)
			}
			reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
			if err != nil || reaction.Desired != reviewAwardPass {
				t.Errorf("recovered reaction = %+v, %v", reaction, err)
			}
		})
	}
}

var errReactionCrash = errors.New("injected reaction write failure")

type reactionCrashStore struct{ ReviewStore }

func (s reactionCrashStore) SetReviewReaction(ctx context.Context, goal store.ReviewReactionGoal) (bool, error) {
	if goal.Desired == reviewAwardPass {
		return false, errReactionCrash
	}
	return s.ReviewStore.SetReviewReaction(ctx, goal)
}

func TestPublicationRecoveryRepairsReactionAfterStatusWrite(t *testing.T) {
	ctx := context.Background()
	f, wf := commandReviewWithPendingPublication(t)
	job := manualPublicationJob(t, f, wf.ID)
	f.service.store = reactionCrashStore{f.db}
	if err := f.service.RepublishReview(ctx, job); !errors.Is(err, errReactionCrash) {
		t.Fatalf("reaction write = %v", err)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.State != reviewStatusPass {
		t.Fatalf("saved status = %+v, %v", status, err)
	}
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardEyes {
		t.Fatalf("reaction before recovery = %+v, %v", reaction, err)
	}
	f.service.store = &stateListStore{Store: f.db, reactions: []store.ReviewReaction{*reaction}}
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatal(err)
	}
	reaction, err = f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardPass {
		t.Fatalf("recovered reaction = %+v, %v; want pass", reaction, err)
	}
}

func TestManualRepublishUnknownKeepsPublishingUntilConfirmed(t *testing.T) {
	ctx := context.Background()
	f, wf := commandReviewWithPendingPublication(t)
	findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) != 1 {
		t.Fatalf("findings = %+v, %v", findings, err)
	}
	if _, err := f.db.SetFindingAnnotation(ctx, wf.ID, findings[0].ID, store.FindingAnnotationUncertain, "reviewer"); err != nil {
		t.Fatal(err)
	}
	job := manualPublicationJob(t, f, wf.ID)
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.service.RepublishReview(ctx, job); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("unknown republish = %v", err)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.State != reviewStatusPublishing {
		t.Fatalf("unknown status = %+v, %v", status, err)
	}
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardEyes {
		t.Fatalf("unknown reaction = %+v, %v", reaction, err)
	}
	f.publisher.gitlab = f.gitlab
	if err := f.service.RepublishReview(ctx, job); err != nil {
		t.Fatalf("confirm republish: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Errorf("created %d discussions", got)
	}
	status, err = f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.State != reviewStatusPass {
		t.Errorf("confirmed status = %+v, %v", status, err)
	}
	reaction, err = f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardPass {
		t.Errorf("confirmed reaction = %+v, %v", reaction, err)
	}
	after, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(after) != 1 || after[0].Evidence != findings[0].Evidence || after[0].Annotation != store.FindingAnnotationUncertain {
		t.Fatalf("finding changed: %+v, %v", after, err)
	}
}

func TestManualRepublishFailureSettlesWithoutPass(t *testing.T) {
	for _, tc := range []struct {
		name, wantState, wantReaction string
		change                        func(*reviewFixture)
	}{
		{"stale", reviewStatusStale, reviewAwardNone, func(f *reviewFixture) { f.gitlab.version.BaseSHA = "changed" }},
		{"failed", reviewStatusFailed, reviewAwardFail, func(f *reviewFixture) { f.gitlab.mrErr = &gitlab.Error{Op: "get_merge_request", Status: 404} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, wf := commandReviewWithPendingPublication(t)
			job := manualPublicationJob(t, f, wf.ID)
			tc.change(f)
			if err := f.service.RepublishReview(ctx, job); err == nil {
				t.Fatal("republish unexpectedly succeeded")
			}
			status, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || status.State != tc.wantState {
				t.Errorf("status = %+v, %v; want %s", status, err, tc.wantState)
			}
			reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
			if err != nil || reaction.Desired != tc.wantReaction {
				t.Errorf("reaction = %+v, %v; want %s", reaction, err, tc.wantReaction)
			}
		})
	}
}

func TestManualRepublishDoesNotOverwriteNewerReviewStatus(t *testing.T) {
	ctx := context.Background()
	f, wf := commandReviewWithPendingPublication(t)
	job := manualPublicationJob(t, f, wf.ID)
	newer, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command","note_id":555}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.RepublishReview(ctx, job); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("old republish = %v, want stale", err)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.Generation != newer.Generation || status.State != reviewStatusQueued {
		t.Fatalf("newer status = %+v, %v", status, err)
	}
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Generation != newer.Generation || reaction.Desired != reviewAwardEyes {
		t.Fatalf("newer reaction = %+v, %v", reaction, err)
	}
}

type signalingRecoveryStore struct {
	*stateListStore
	latest chan struct{}
}

func (s *signalingRecoveryStore) LatestReviewWorkflow(ctx context.Context, projectID, mrIID int64) (*store.Workflow, error) {
	wf, err := s.stateListStore.LatestReviewWorkflow(ctx, projectID, mrIID)
	select {
	case s.latest <- struct{}{}:
	default:
	}
	return wf, err
}

func TestReconcileWaitsForConcurrentManualRepublish(t *testing.T) {
	ctx := context.Background()
	f, wf := commandReviewWithPendingPublication(t)
	if err := f.publisher.Publish(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{ProjectID: 42, MRIID: 7, Generation: status.Generation,
		Attempt: status.Attempt + 1, HeadSHA: f.head, State: reviewStatusPublishing}); err != nil {
		t.Fatal(err)
	}
	status, err = f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	job := manualPublicationJob(t, f, wf.ID)
	recovery := &signalingRecoveryStore{stateListStore: &stateListStore{Store: f.db, statuses: []store.ReviewStatus{*status}}, latest: make(chan struct{}, 4)}
	f.service.store = recovery
	block := newBlockedOutbound(t)
	f.publisher.gitlab = &blockedPublicationGitLab{reviewGitLab: f.gitlab, blockedOutbound: block, stage: "update"}
	manualDone := make(chan error, 1)
	go func() { manualDone <- f.service.RepublishReview(ctx, job) }()
	t.Cleanup(func() { block.unblock(); <-manualDone })
	awaitOutbound(t, block.entered, "manual republish")
	awaitOutbound(t, recovery.latest, "manual status check")
	reconciled := make(chan error, 1)
	go func() { reconciled <- f.service.ReconcileSideEffects(ctx) }()
	awaitOutbound(t, recovery.latest, "reconciliation workflow lookup")
	select {
	case err := <-reconciled:
		t.Fatalf("reconciliation passed an in-flight publication: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	block.unblock()
	if err := <-reconciled; err != nil {
		t.Fatal(err)
	}
	err = <-manualDone
	manualDone <- err
	if err != nil {
		t.Fatal(err)
	}
	status, err = f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.State != reviewStatusPass {
		t.Fatalf("final status = %+v, %v", status, err)
	}
}

type pausedReactionReadStore struct {
	*stateListStore
	entered chan struct{}
	release chan struct{}
	paused  atomic.Bool
}

func (s *pausedReactionReadStore) GetReviewReaction(ctx context.Context, projectID, mrIID, noteID int64) (*store.ReviewReaction, error) {
	row, err := s.stateListStore.GetReviewReaction(ctx, projectID, mrIID, noteID)
	if s.paused.CompareAndSwap(false, true) {
		close(s.entered)
		<-s.release
	}
	return row, err
}

func TestStaleEyesCleanupDoesNotOverwriteConcurrentRepublish(t *testing.T) {
	ctx := context.Background()
	f, wf := commandReviewWithPendingPublication(t)
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetReviewReaction(ctx, store.ReviewReactionGoal{ProjectID: 42, MRIID: 7, NoteID: 555,
		Generation: status.Generation, Desired: reviewAwardEyes}); err != nil {
		t.Fatal(err)
	}
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil {
		t.Fatal(err)
	}
	job := manualPublicationJob(t, f, wf.ID)
	paused := &pausedReactionReadStore{stateListStore: &stateListStore{Store: f.db, reactions: []store.ReviewReaction{*reaction}},
		entered: make(chan struct{}), release: make(chan struct{})}
	f.service.store = paused
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	var once sync.Once
	unblock := func() { once.Do(func() { close(paused.release) }) }
	reconciled := make(chan error, 1)
	go func() { reconciled <- f.service.ReconcileSideEffects(ctx) }()
	t.Cleanup(func() { unblock(); <-reconciled })
	awaitOutbound(t, paused.entered, "stale eyes read")
	manualDone := make(chan error, 1)
	go func() { manualDone <- f.service.RepublishReview(ctx, job) }()
	t.Cleanup(func() { unblock(); <-manualDone })
	select {
	case err := <-manualDone:
		manualDone <- err
	case <-time.After(100 * time.Millisecond):
	}
	unblock()
	if err := <-reconciled; err != nil {
		reconciled <- err
		t.Fatal(err)
	}
	reconciled <- nil
	err = <-manualDone
	manualDone <- err
	if !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("manual republish = %v, want unknown", err)
	}
	reaction, err = f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardEyes {
		t.Fatalf("pending reaction = %+v, %v; want eyes", reaction, err)
	}
}
