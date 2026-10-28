package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestCancelledPendingPublicationRecoversWithoutPublisher(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "cancelled-publication")
	id := createPendingReview(t, f, job.ID, jobGeneration(t, job))
	if err := f.engine.Cancel(ctx, id, "crash before skip"); err != nil {
		t.Fatal(err)
	}
	items, err := f.db.ListPendingPublications(ctx, 50)
	if err != nil || len(items) != 1 || items[0].WorkflowID != id || items[0].Status != "cancelled" {
		t.Fatalf("pending after cancel = %+v, %v", items, err)
	}
	f.service.publisher = nil
	for i := 0; i < 2; i++ {
		if err := f.service.PublishPending(ctx); err != nil {
			t.Fatal(err)
		}
	}
	record, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || record.PublicationState != store.WorkflowPublicationSkipped || record.PublicationError != "review cancelled" {
		t.Fatalf("recovered dossier = %+v, %v", record, err)
	}
	if n := f.gitlab.noteCount(); n != 0 {
		t.Fatalf("remote notes written: %d", n)
	}
	if err := f.db.SetWorkflowPublicationState(ctx, id, store.WorkflowPublicationPending, ""); err != nil {
		t.Fatal(err)
	}
	stale := items[0]
	stale.ProjectID++
	if _, err := f.service.publishPendingItem(ctx, stale); err != nil {
		t.Fatal(err)
	}
	record, err = f.db.GetWorkflowDossier(ctx, id)
	if err != nil || record.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("stale project settled a different workflow: %+v, %v", record, err)
	}
	for _, state := range []string{store.WorkflowPublicationPublished, store.WorkflowPublicationFailed, store.WorkflowPublicationSkipped} {
		if err := f.db.SetWorkflowPublicationState(ctx, id, state, "settled"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.service.publishPendingItem(ctx, items[0]); err != nil {
			t.Fatal(err)
		}
		record, err := f.db.GetWorkflowDossier(ctx, id)
		if err != nil || record.PublicationState != state {
			t.Fatalf("settled state %s overwritten: %+v, %v", state, record, err)
		}
	}
}

type failedCancelCASStore struct {
	ReviewStore
	calls  int
	onCall func()
	noOp   bool
}

var errCancelCAS = errors.New("injected cancellation CAS failure")

func (s *failedCancelCASStore) SetWorkflowPublicationStateIfPending(context.Context, string, string) error {
	s.calls++
	if s.onCall != nil {
		s.onCall()
	}
	if s.noOp {
		return nil
	}
	return errCancelCAS
}

func TestCancelRecoveryCASFailureStopsWithoutProgress(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "cancel-cas-failure")
	id := createPendingReview(t, f, job.ID, jobGeneration(t, job))
	if err := f.engine.Cancel(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	failed := &failedCancelCASStore{ReviewStore: f.db}
	f.service.store, f.service.publisher = failed, nil
	if err := f.service.PublishPending(ctx); !errors.Is(err, errCancelCAS) {
		t.Fatalf("PublishPending = %v", err)
	}
	if failed.calls != 1 {
		t.Fatalf("CAS calls = %d, want 1 without progress", failed.calls)
	}
}

func TestCancelRecoveryStopsWhenContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "cancel-context")
	id := createPendingReview(t, f, job.ID, jobGeneration(t, job))
	if err := f.engine.Cancel(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	failed := &failedCancelCASStore{ReviewStore: f.db, onCall: cancel}
	f.service.store, f.service.publisher = failed, nil
	if err := f.service.PublishPending(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishPending = %v, want context canceled", err)
	}
	if failed.calls != 1 {
		t.Fatalf("CAS calls = %d", failed.calls)
	}
}

func TestCancelRecoveryNoOpCASDoesNotCountAsProgress(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "cancel-cas-noop")
	id := createPendingReview(t, f, job.ID, jobGeneration(t, job))
	if err := f.engine.Cancel(ctx, id, "test"); err != nil {
		t.Fatal(err)
	}
	noop := &failedCancelCASStore{ReviewStore: f.db, noOp: true}
	f.service.store, f.service.publisher = noop, nil
	if err := f.service.PublishPending(ctx); err != nil {
		t.Fatal(err)
	}
	if noop.calls != 1 {
		t.Fatalf("CAS calls = %d, want 1 without progress", noop.calls)
	}
	d, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || d.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("dossier = %+v, %v", d, err)
	}
}

func TestRecoverDrainsCancelledPublicationBacklog(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.publisher = nil
	spec := dag.GraphSpec{Nodes: []dag.NodeSpec{{ID: "n", Kind: "test", Required: true}}}
	meta := map[string]string{"kind": "review", "project_id": "42", "mr_iid": "7", "trigger": "api"}
	type reservation struct{ id, job string }
	var pending []reservation
	var settled []reservation
	for i := 0; i < 153; i++ {
		id, err := f.engine.Create(ctx, spec, meta)
		if err != nil {
			t.Fatal(err)
		}
		job := fmt.Sprintf("cancelled-job-%d", i)
		if _, err := f.db.SaveWorkflowDossier(ctx, store.WorkflowDossier{WorkflowID: id, JobID: job, Version: 1, Data: json.RawMessage(`{}`)}); err != nil {
			t.Fatal(err)
		}
		if err := f.engine.Cancel(ctx, id, "crash before skip"); err != nil {
			t.Fatal(err)
		}
		r := reservation{id: id, job: job}
		if i < 151 {
			pending = append(pending, r)
		} else {
			state := []string{store.WorkflowPublicationPublished, store.WorkflowPublicationFailed}[i-151]
			if err := f.db.SetWorkflowPublicationState(ctx, id, state, "settled"); err != nil {
				t.Fatal(err)
			}
			settled = append(settled, r)
		}
	}
	// Keep a publishable completed workflow in the same scan with no publisher.
	id, err := f.engine.Create(ctx, spec, meta)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SaveWorkflowDossier(ctx, store.WorkflowDossier{WorkflowID: id, Version: 1, Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	events := []dag.Event{
		{Type: dag.EventWorkflowStarted, Payload: json.RawMessage(`{}`)},
		{Type: dag.EventNodeQueued, Payload: json.RawMessage(`{"node_id":"n","attempt":1}`)},
		{Type: dag.EventNodeStarted, Payload: json.RawMessage(`{"node_id":"n","attempt":1}`)},
		{Type: dag.EventNodeCompleted, Payload: json.RawMessage(`{"node_id":"n","attempt":1}`)},
		{Type: dag.EventWorkflowCompleted, Payload: json.RawMessage(`{}`)},
	}
	if _, err := f.db.Append(ctx, id, 1, events...); err != nil {
		t.Fatal(err)
	}
	refs, err := f.db.ProtectedRunRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pending {
		if _, ok := refs[r.id]; !ok {
			t.Fatalf("pending workflow %s not protected", r.id)
		}
		if _, ok := refs[r.job]; !ok {
			t.Fatalf("pending job %s not protected", r.job)
		}
	}
	if err := f.service.Recover(ctx); err != nil {
		t.Fatal(err)
	}
	refs, err = f.db.ProtectedRunRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range pending {
		d, err := f.db.GetWorkflowDossier(ctx, r.id)
		if err != nil || d.PublicationState != store.WorkflowPublicationSkipped {
			t.Fatalf("cancelled %s = %+v, %v", r.id, d, err)
		}
		if _, ok := refs[r.id]; ok {
			t.Fatalf("cancelled workflow %s still protected", r.id)
		}
		if _, ok := refs[r.job]; ok {
			t.Fatalf("cancelled job %s still protected", r.job)
		}
	}
	for i, r := range settled {
		d, err := f.db.GetWorkflowDossier(ctx, r.id)
		want := []string{store.WorkflowPublicationPublished, store.WorkflowPublicationFailed}[i]
		if err != nil || d.PublicationState != want {
			t.Fatalf("settled %s = %+v, %v", r.id, d, err)
		}
	}
	d, err := f.db.GetWorkflowDossier(ctx, id)
	if err != nil || d.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("completed = %+v, %v", d, err)
	}
	if n := f.gitlab.noteCount(); n != 0 {
		t.Fatalf("remote notes written: %d", n)
	}
}

func TestPublishPendingWithoutPublisherLeavesPublishableStatesPending(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed", true: "failed"}[failed], func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			if failed {
				f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
					return nil, agent.ErrNoSubmit
				}
			}
			job := f.seedReview(t, "publishable")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := map[bool]string{false: "completed", true: "failed"}[failed]
			if wf.Status != want {
				t.Fatalf("status = %q, want %q", wf.Status, want)
			}
			if err := f.db.SetWorkflowPublicationState(ctx, wf.ID, store.WorkflowPublicationPending, ""); err != nil {
				t.Fatal(err)
			}
			before := f.gitlab.noteCount()
			f.service.publisher = nil
			if err := f.service.PublishPending(ctx); err != nil {
				t.Fatal(err)
			}
			d, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil || d.PublicationState != store.WorkflowPublicationPending || f.gitlab.noteCount() != before {
				t.Fatalf("publishable state = %+v, %v; notes %d to %d", d, err, before, f.gitlab.noteCount())
			}
		})
	}
}
