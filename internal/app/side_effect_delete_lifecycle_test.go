package app

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

type sideEffectJobBarrier struct {
	ReviewStore
	barrier *enqueueBarrier
	reads   atomic.Int32
}

func (s *sideEffectJobBarrier) GetJob(ctx context.Context, id string) (*store.Job, error) {
	s.reads.Add(1)
	if s.barrier != nil {
		if err := s.barrier.wait(ctx); err != nil {
			return nil, err
		}
	}
	return s.ReviewStore.GetJob(ctx, id)
}

func TestLifecycleSideEffectAndCandidateHoldLockThroughJobRead(t *testing.T) {
	for _, kind := range []string{"review.status", "review.reaction", "review.candidate"} {
		t.Run(kind, func(t *testing.T) {
			r := newLifecycleRuntime(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if kind == "review.candidate" {
				_, err := r.store.RecordReviewCandidateDelivery(ctx, store.Delivery{UUID: "candidate-lock", ProjectID: 42, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued}, &store.ReviewCandidate{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open"}`), MaxAttempts: 3})
				if err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"note_id":555}`)}); err != nil {
				t.Fatal(err)
			}
			job, err := r.store.Claim(ctx, "worker", []string{kind}, time.Minute)
			if err != nil || job == nil {
				t.Fatalf("claim: %v", err)
			}
			b := &enqueueBarrier{entered: make(chan struct{}), release: make(chan struct{})}
			defer func() {
				select {
				case <-b.release:
				default:
					close(b.release)
				}
			}()
			r.reviews.store = &sideEffectJobBarrier{ReviewStore: r.store, barrier: b}
			api := newReviewGitLab(sha40)
			r.reviews.gitlab = api
			r.reviews.status = testStatusReporter(api)
			done := make(chan error, 1)
			go func() { done <- r.reviews.Handle(ctx, job) }()
			waitConsumer(t, b.entered)
			wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
			deleted := make(chan error, 1)
			go func() { deleted <- r.Projects().Delete(wait, 42) }()
			waitConsumer(t, wait.waiting)
			if _, err := r.store.GetProject(ctx, 42); err != nil {
				t.Fatalf("delete overtook job: %v", err)
			}
			close(b.release)
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if err := <-deleted; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestLifecycleDeleteFirstRejectsSideEffectAndCandidate(t *testing.T) {
	for _, kind := range []string{"review.status", "review.reaction", "review.candidate"} {
		t.Run(kind, func(t *testing.T) {
			r := newLifecycleRuntime(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if kind == "review.candidate" {
				_, err := r.store.RecordReviewCandidateDelivery(ctx, store.Delivery{UUID: "candidate-delete-first", ProjectID: 42, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued}, &store.ReviewCandidate{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open"}`), MaxAttempts: 3})
				if err != nil {
					t.Fatal(err)
				}
			} else if _, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"note_id":555}`)}); err != nil {
				t.Fatal(err)
			}
			job, err := r.store.Claim(ctx, "worker", []string{kind}, time.Minute)
			if err != nil || job == nil {
				t.Fatalf("claim: %v", err)
			}
			unlock, err := r.workspace.Get(42).Lock(ctx)
			if err != nil {
				t.Fatal(err)
			}
			observed := &sideEffectJobBarrier{ReviewStore: r.store}
			r.reviews.store = observed
			wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- r.reviews.Handle(wait, job) }()
			waitConsumer(t, wait.waiting)
			if observed.reads.Load() != 0 {
				unlock()
				t.Fatal("job read before project lock")
			}
			if err := r.store.DeleteProject(ctx, 42); err != nil {
				unlock()
				t.Fatal(err)
			}
			seedProject(t, r.store, r.cipher, 42, "new-secret", "{}", "default")
			unlock()
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if observed.reads.Load() != 1 {
				t.Fatalf("job reads = %d", observed.reads.Load())
			}
			if _, err := r.store.GetReviewControl(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("old job wrote new project: %v", err)
			}
		})
	}
}

type reconcileListBarrier struct {
	ReviewStore
	barrier *enqueueBarrier
}

func (s *reconcileListBarrier) ListPendingReviewStatuses(ctx context.Context) ([]store.ReviewStatus, error) {
	rows, err := s.ReviewStore.ListPendingReviewStatuses(ctx)
	if err == nil {
		err = s.barrier.wait(ctx)
	}
	return rows, err
}

func TestLifecycleReconcileEnumerationHoldsProjectLock(t *testing.T) {
	r := newLifecycleRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40}); err != nil {
		t.Fatal(err)
	}
	b := &enqueueBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-b.release:
		default:
			close(b.release)
		}
	}()
	r.reviews.store = &reconcileListBarrier{ReviewStore: r.store, barrier: b}
	done := make(chan error, 1)
	go func() { done <- r.reviews.ReconcileSideEffects(ctx) }()
	waitConsumer(t, b.entered)
	deleteCtx := &waitContext{Context: ctx, waiting: make(chan struct{})}
	deleted := make(chan error, 1)
	go func() {
		unlock, err := r.workspace.Get(42).Lock(deleteCtx)
		if err == nil {
			unlock()
		}
		deleted <- err
	}()
	waitConsumer(t, deleteCtx.waiting)
	if _, err := r.store.GetProject(ctx, 42); err != nil {
		t.Fatalf("delete passed enumeration: %v", err)
	}
	close(b.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-deleted; err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleOldReactionJobCannotApplyNewRegistration(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	_, err := r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"note_id":555}`)})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.Claim(ctx, "worker", []string{"review.reaction"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	seedProject(t, r.store, r.cipher, 42, "new-secret", "{}", "default")
	_, err = r.store.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"note_id":555}`)})
	if err != nil {
		t.Fatal(err)
	}
	r.reviews.status = testStatusReporter(newReviewGitLab(sha40))
	if err := r.reviews.RunReactionJob(ctx, job); err != nil && !IsPermanentJob(err) {
		t.Fatal(err)
	}
	row, err := r.store.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || row.AppliedVersion != 0 {
		t.Fatalf("old reaction applied new registration: %+v, %v", row, err)
	}
}

func TestLifecycleOldCandidateDoesNotReadNewProject(t *testing.T) {
	ctx := context.Background()
	r := newLifecycleRuntime(t)
	_, err := r.store.RecordReviewCandidateDelivery(ctx, store.Delivery{UUID: "old-candidate", ProjectID: 42, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued}, &store.ReviewCandidate{ProjectID: 42, MRIID: 7, HeadSHA: sha40, Payload: json.RawMessage(`{"trigger":"mr_open"}`), MaxAttempts: 3})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.Claim(ctx, "worker", []string{"review.candidate"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim: %v", err)
	}
	if err := r.Projects().Delete(ctx, 42); err != nil {
		t.Fatal(err)
	}
	seedProject(t, r.store, r.cipher, 42, "new-secret", "{}", "default")
	observed := &observingConsumerStore{ReviewStore: r.store}
	r.reviews.store = observed
	if err := r.reviews.RunCandidate(ctx, job); err != nil && !IsPermanentJob(err) {
		t.Fatal(err)
	}
	if observed.projectReads.Load() != 0 {
		t.Fatal("old candidate read new project")
	}
}

type publisherDossierBarrier struct {
	ReviewStore
	barrier *enqueueBarrier
	once    sync.Once
}

type publisherWorkflowObserver struct {
	ReviewStore
	read         chan struct{}
	once         sync.Once
	projectReads atomic.Int32
}

func (s *publisherWorkflowObserver) GetWorkflow(ctx context.Context, id string) (*store.Workflow, error) {
	wf, err := s.ReviewStore.GetWorkflow(ctx, id)
	s.once.Do(func() { close(s.read) })
	return wf, err
}

func (s *publisherWorkflowObserver) GetProject(ctx context.Context, id int64) (*store.Project, error) {
	s.projectReads.Add(1)
	return s.ReviewStore.GetProject(ctx, id)
}

func TestLifecycleDeleteFirstRejectsOldPublish(t *testing.T) {
	r := newLifecycleRuntime(t)
	_, workflow := seedConsumerWorkflow(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := json.Marshal(reviewPublishPayload{WorkflowID: workflow.ID})
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := r.store.Enqueue(ctx, store.Job{Kind: "review.publish", Key: "review.publish:" + workflow.ID, Payload: raw})
	if err != nil {
		t.Fatal(err)
	}
	job, err := r.store.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := r.workspace.Get(42).Lock(ctx)
	if err != nil {
		t.Fatal(err)
	}
	observer := &publisherWorkflowObserver{ReviewStore: r.store, read: make(chan struct{})}
	r.publisher.store = observer
	done := make(chan error, 1)
	go func() { done <- r.reviews.RepublishReview(ctx, job) }()
	waitConsumer(t, observer.read)
	if err := r.store.DeleteProject(ctx, 42); err != nil {
		unlock()
		t.Fatal(err)
	}
	seedProject(t, r.store, r.cipher, 42, "new-secret", "{}", "default")
	unlock()
	if err := <-done; err != nil && !IsPermanentJob(err) {
		t.Fatal(err)
	}
	if observer.projectReads.Load() != 0 {
		t.Fatal("old publication read new project")
	}
}

func (s *publisherDossierBarrier) GetWorkflowDossier(ctx context.Context, id string) (*store.WorkflowDossier, error) {
	var err error
	s.once.Do(func() { err = s.barrier.wait(ctx) })
	if err != nil {
		return nil, err
	}
	return s.ReviewStore.GetWorkflowDossier(ctx, id)
}

func TestLifecyclePublisherHoldsProjectLock(t *testing.T) {
	r := newLifecycleRuntime(t)
	_, workflow := seedConsumerWorkflow(t, r)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b := &enqueueBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-b.release:
		default:
			close(b.release)
		}
	}()
	r.publisher.store = &publisherDossierBarrier{ReviewStore: r.store, barrier: b}
	done := make(chan error, 1)
	go func() { done <- r.publisher.Publish(ctx, workflow.ID) }()
	waitConsumer(t, b.entered)
	wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
	locked := make(chan error, 1)
	go func() {
		unlock, err := r.workspace.Get(42).Lock(wait)
		if err == nil {
			unlock()
		}
		locked <- err
	}()
	waitConsumer(t, wait.waiting)
	close(b.release)
	<-done
	if err := <-locked; err != nil {
		t.Fatal(err)
	}
}

func TestLifecycleQueuedWriterDoesNotDeadlockReviewPublication(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "review-lock")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	b := &enqueueBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-b.release:
		default:
			close(b.release)
		}
	}()
	f.publisher.store = &publisherDossierBarrier{ReviewStore: f.db, barrier: b}
	done := make(chan error, 1)
	go func() { done <- f.service.RunReview(ctx, job) }()
	waitConsumer(t, b.entered)
	wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
	writer := make(chan error, 1)
	go func() {
		unlock, err := f.manager.Get(42).Lock(wait)
		if err == nil {
			unlock()
		}
		writer <- err
	}()
	waitConsumer(t, wait.waiting)
	close(b.release)
	if err := <-done; err != nil {
		t.Fatalf("review with queued writer: %v", err)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
}

func TestLifecyclePendingPublicationHoldsLock(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	job := f.seedReview(t, "pending-lock")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetWorkflowPublicationState(ctx, wf.ID, store.WorkflowPublicationPending, ""); err != nil {
		t.Fatal(err)
	}
	b := &enqueueBarrier{entered: make(chan struct{}), release: make(chan struct{})}
	defer func() {
		select {
		case <-b.release:
		default:
			close(b.release)
		}
	}()
	f.publisher.store = &publisherDossierBarrier{ReviewStore: f.db, barrier: b}
	done := make(chan error, 1)
	go func() { done <- f.service.PublishPending(ctx) }()
	waitConsumer(t, b.entered)
	wait := &waitContext{Context: ctx, waiting: make(chan struct{})}
	writer := make(chan error, 1)
	go func() {
		unlock, err := f.manager.Get(42).Lock(wait)
		if err == nil {
			unlock()
		}
		writer <- err
	}()
	waitConsumer(t, wait.waiting)
	close(b.release)
	if err := <-done; err != nil {
		t.Fatalf("pending publication: %v", err)
	}
	if err := <-writer; err != nil {
		t.Fatal(err)
	}
}
