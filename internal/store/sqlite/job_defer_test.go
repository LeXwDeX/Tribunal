package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestDeferPreservesBudgetAndLeaseFence(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	id, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:defer", MaxAttempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	var stale *store.Job
	for i := 0; i < 4; i++ {
		claim, err := s.Claim(ctx, "same-owner", []string{"review.run"}, time.Minute)
		if err != nil || claim == nil {
			t.Fatalf("claim: %v %v", claim, err)
		}
		if stale != nil {
			if claim.Attempts <= stale.Attempts {
				t.Fatal("attempt fence did not advance")
			}
			if err := s.Complete(ctx, *stale); !errors.Is(err, store.ErrLeaseLost) {
				t.Fatalf("stale completion: %v", err)
			}
		}
		if err := s.Defer(ctx, *claim, clock.Now()); err != nil {
			t.Fatal(err)
		}
		stale = claim
	}
	job, err := s.GetJob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if job.State != store.JobQueued || job.MaxAttempts-job.Attempts != 2 {
		t.Fatalf("deferred job = %+v", job)
	}
	for i := 0; i < 2; i++ {
		claim, err := s.Claim(ctx, "same-owner", []string{"review.run"}, time.Minute)
		if err != nil || claim == nil {
			t.Fatalf("claim after deferral: %v %v", claim, err)
		}
		if err := s.Fail(ctx, *claim, errors.New("business failure"), nil); err != nil {
			t.Fatal(err)
		}
	}
	job, err = s.GetJob(ctx, id)
	if err != nil || job.State != store.JobFailed {
		t.Fatalf("business failures = %+v %v", job, err)
	}
}

func TestDeferCancelsWhenSuccessorQueued(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	s := openTestStore(t, clock)
	id, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:successor"})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := s.Claim(ctx, "owner", []string{"review.run"}, time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim: %v %v", claim, err)
	}
	if _, _, err := s.Enqueue(ctx, store.Job{Kind: "review.run", Key: "review:successor"}); err != nil {
		t.Fatal(err)
	}
	if err := s.Defer(ctx, *claim, clock.Now()); err != nil {
		t.Fatal(err)
	}
	job, err := s.GetJob(ctx, id)
	if err != nil || job.State != store.JobCancelled {
		t.Fatalf("old job = %+v %v", job, err)
	}
}
