package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRecoveryOwnershipDefersQueueWithoutSpendingBudget(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.seedReview(t, "unused")
	ctx := context.Background()
	job, err := f.db.Claim(ctx, "first", []string{"review.run"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim: %v, %v", job, err)
	}
	wfID := interruptReview(t, f, job)
	if err := f.db.Fail(ctx, *job, errors.New("interrupted"), nil); err != nil {
		t.Fatal(err)
	}
	block := make(chan struct{})
	runner := &fakeAgentRunner{}
	runner.setBlock(block, make(chan struct{}))
	f.service.agentRun = runner.Run
	recoverCtx, stopRecovery := context.WithCancel(ctx)
	recovered := make(chan error, 1)
	go func() { recovered <- f.service.Recover(recoverCtx) }()
	t.Cleanup(stopRecovery)
	select {
	case <-runner.startedSignal():
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not acquire ownership")
	}
	workerCtx, stopWorker := context.WithCancel(ctx)
	workerDone := make(chan error, 1)
	w := jobs.NewWorker(f.db, jobs.WorkerConfig{
		Handlers:     map[string]jobs.Handler{"review.run": f.service.Handle},
		PollInterval: 5 * time.Millisecond, Lease: time.Second,
		RetryBackoff: func(int) time.Duration { return 5 * time.Millisecond },
	})
	go func() { workerDone <- w.Run(workerCtx) }()
	t.Cleanup(func() { stopWorker(); <-workerDone })
	deadline := time.Now().Add(5 * time.Second)
	for {
		current, err := f.db.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == store.JobFailed || current.State == store.JobDone {
			t.Fatalf("contended job settled as %s at attempt %d", current.State, current.Attempts)
		}
		if current.Attempts >= 5 {
			remaining := current.MaxAttempts - current.Attempts
			if current.State == store.JobRunning {
				remaining++
			}
			if remaining < 2 {
				t.Fatalf("contention consumed retry budget: %+v", current)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("contention did not defer repeatedly")
		}
		time.Sleep(5 * time.Millisecond)
	}
	stopRecovery()
	select {
	case err := <-recovered:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("recovery error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("recovery did not stop")
	}
	close(block)
	deadline = time.Now().Add(5 * time.Second)
	for {
		current, err := f.db.GetJob(ctx, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		if current.State == store.JobDone {
			break
		}
		if current.State == store.JobFailed || time.Now().After(deadline) {
			t.Fatalf("job did not resume: %+v", current)
		}
		time.Sleep(5 * time.Millisecond)
	}
	assertReviewPublished(t, f, wfID)
}
