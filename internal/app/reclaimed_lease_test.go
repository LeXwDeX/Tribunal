package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// A review.run job reclaimed after a restart shares its job ID with startup
// recovery. Whichever side owns the executor slot must keep the workflow; the
// other must stand down without running the agent.
func TestReclaimedReviewRunDefersWhileRecoveryOwnsJob(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "reclaimed-lease")

	dead, err := f.db.Claim(ctx, "dead-process:0", []string{"review.run"}, time.Hour)
	if err != nil || dead == nil || dead.ID != job.ID {
		t.Fatalf("claim as dead process: %+v, %v", dead, err)
	}
	if n, err := f.db.ReclaimForeignLeases(ctx, ""); err != nil || n != 1 {
		t.Fatalf("ReclaimForeignLeases = %d, %v; want 1", n, err)
	}
	reclaimed, err := f.db.Claim(ctx, "new-process:0", []string{"review.run"}, time.Hour)
	if err != nil || reclaimed == nil || reclaimed.ID != job.ID {
		t.Fatalf("reclaimed job not claimable immediately: %+v, %v", reclaimed, err)
	}

	if !f.executors.Acquire(job.ID) {
		t.Fatal("could not acquire job ownership for recovery")
	}
	err = f.service.RunReview(ctx, reclaimed)
	f.executors.Release(job.ID)
	if !errors.Is(err, jobs.ErrDeferred) {
		t.Fatalf("RunReview while recovery owns the job = %v, want ErrDeferred", err)
	}
	if f.runner.count() != 0 {
		t.Fatalf("agent ran %d times while recovery owned the job", f.runner.count())
	}
	if got, err := f.db.GetJob(ctx, job.ID); err != nil || got.State != store.JobRunning {
		t.Fatalf("job after deferral = %+v, %v", got, err)
	}
}
