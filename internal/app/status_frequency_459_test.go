package app

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// countingStatusGitLab counts the two remote lookups every status write makes.
// A self-heal visit that is skipped locally must not reach GitLab (#459).
type countingStatusGitLab struct {
	*reviewGitLab
	mu          sync.Mutex
	identities  int
	noteLookups int
}

func (g *countingStatusGitLab) CurrentUser(ctx context.Context, token string) (*gitlab.User, error) {
	g.mu.Lock()
	g.identities++
	g.mu.Unlock()
	return g.reviewGitLab.CurrentUser(ctx, token)
}

func (g *countingStatusGitLab) ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error) {
	g.mu.Lock()
	g.noteLookups++
	g.mu.Unlock()
	return g.reviewGitLab.ListMRNotes(ctx, token, projectID, iid)
}

func (g *countingStatusGitLab) calls() (int, int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.identities, g.noteLookups
}

func (g *countingStatusGitLab) reset() {
	g.mu.Lock()
	g.identities, g.noteLookups = 0, 0
	g.mu.Unlock()
}

// appliedTerminalStatus drives one MR to an applied PASS row through the real
// status job, so the low-frequency scan sees exactly what production stores.
func appliedTerminalStatus(t *testing.T, f *reviewFixture, mrIID int64) *store.ReviewStatus {
	t.Helper()
	ctx := context.Background()
	result, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: mrIID, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command"}`),
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: mrIID, Generation: result.Generation,
		HeadSHA: f.head, State: reviewStatusPass,
	}); err != nil || !ok {
		t.Fatalf("SetReviewStatus(pass): %v %v", ok, err)
	}
	drainReviewJobs(t, f, "review.status")
	row, err := f.db.GetReviewStatus(ctx, 42, mrIID)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.AppliedVersion != row.Version || row.LastError != "" {
		t.Fatalf("status is not applied: %+v", row)
	}
	return row
}

func claimStatusJob(t *testing.T, f *reviewFixture) *store.Job {
	t.Helper()
	job, err := f.db.Claim(context.Background(), "probe", []string{"review.status"}, time.Minute)
	if err != nil {
		t.Fatalf("claim status job: %v", err)
	}
	return job
}

// TestReconcileSkipsAppliedTerminalStatus is the job-rate regression: a
// terminal MR whose status was applied must not enqueue review.status on every
// five-minute reconciliation.
func TestReconcileSkipsAppliedTerminalStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	base := time.Now().UTC()
	f.service.now = func() time.Time { return base }

	counting := &countingStatusGitLab{reviewGitLab: f.gitlab}
	f.service.status = newStatusReporter(counting, nil)
	appliedTerminalStatus(t, f, 7)
	counting.reset()
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	if job := claimStatusJob(t, f); job != nil {
		t.Fatalf("applied terminal status re-enqueued: %+v", job)
	}
	if identities, lookups := counting.calls(); identities != 0 || lookups != 0 {
		t.Fatalf("reconcile called GitLab: identities=%d note lookups=%d", identities, lookups)
	}
}

// TestReconcileStillEnqueuesPendingStatus keeps the five-minute convergence
// guarantee for rows whose desired version is not applied.
func TestReconcileStillEnqueuesPendingStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	base := time.Now().UTC()
	f.service.now = func() time.Time { return base }

	applied := appliedTerminalStatus(t, f, 7)
	if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: applied.Generation,
		Attempt: applied.Attempt + 1, HeadSHA: f.head, State: reviewStatusRunning,
	}); err != nil || !ok {
		t.Fatalf("SetReviewStatus(running): %v %v", ok, err)
	}
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	job := claimStatusJob(t, f)
	if job == nil {
		t.Fatal("pending status was not re-enqueued within one reconciliation")
	}
	if err := f.service.RunStatusJob(ctx, job); err != nil {
		t.Fatalf("RunStatusJob: %v", err)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.AppliedVersion != row.Version || parseStatusPhase(f.gitlab.notes[0].Body) != reviewStatusPhase(reviewStatusRunning) {
		t.Fatalf("pending status did not converge: %+v", row)
	}
}

// TestSelfHealSkipsFreshVerification proves the local note receipt short
// circuit: an applied row verified within the interval is not visited and no
// GitLab lookup happens.
func TestSelfHealSkipsFreshVerification(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	counting := &countingStatusGitLab{reviewGitLab: f.gitlab}
	f.service.status = newStatusReporter(counting, nil)
	base := time.Now().UTC()
	f.service.now = func() time.Time { return base }
	applied := appliedTerminalStatus(t, f, 7)
	counting.reset()

	if err := f.service.ReconcileAppliedStatuses(ctx); err != nil {
		t.Fatalf("ReconcileAppliedStatuses: %v", err)
	}
	if job := claimStatusJob(t, f); job != nil {
		t.Fatalf("fresh applied row enqueued a self-heal job: %+v", job)
	}

	// A queued duplicate would also be a no-op without touching GitLab.
	payload, err := json.Marshal(map[string]any{"project_id": 42, "mr_iid": 7})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.db.Enqueue(ctx, store.Job{
		Kind: "review.status", Key: "review.status:42:7", Payload: payload, MaxAttempts: 3,
	}); err != nil {
		t.Fatalf("enqueue duplicate: %v", err)
	}
	job := claimStatusJob(t, f)
	if job == nil {
		t.Fatal("duplicate status job was not queued")
	}
	if err := f.service.RunStatusJob(ctx, job); err != nil {
		t.Fatalf("RunStatusJob: %v", err)
	}
	if identities, lookups := counting.calls(); identities != 0 || lookups != 0 {
		t.Fatalf("fresh self-heal reached GitLab: identities=%d note lookups=%d", identities, lookups)
	}
	if row, err := f.db.GetReviewStatus(ctx, 42, 7); err != nil || row.Version != applied.Version {
		t.Fatalf("self-heal changed the row: %+v, %v", row, err)
	}
}

// TestSelfHealConvergesAfterInterval covers the low-frequency repair: once the
// per-row interval expired the status is verified again and a deleted note is
// recreated.
func TestSelfHealConvergesAfterInterval(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	base := time.Now().UTC()
	f.service.now = func() time.Time { return base }
	applied := appliedTerminalStatus(t, f, 7)

	f.gitlab.mu.Lock()
	f.gitlab.notes = nil
	f.gitlab.mu.Unlock()

	base = base.Add(reviewStatusSelfHealInterval + time.Minute)
	if err := f.service.ReconcileAppliedStatuses(ctx); err != nil {
		t.Fatalf("ReconcileAppliedStatuses: %v", err)
	}
	job := claimStatusJob(t, f)
	if job == nil {
		t.Fatal("stale applied row was not enqueued for self-heal")
	}
	if err := f.service.RunStatusJob(ctx, job); err != nil {
		t.Fatalf("RunStatusJob: %v", err)
	}
	notes, err := f.gitlab.ListMRNotes(ctx, "token", 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(notes) != 1 || parseStatusPhase(notes[0].Body) != reviewStatusPhase(reviewStatusPass) {
		t.Fatalf("status note was not restored: %+v", notes)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.Version != applied.Version || row.LastVerifiedAt.IsZero() ||
		!row.LastVerifiedAt.After(applied.LastVerifiedAt) {
		t.Fatalf("self-heal did not advance verification: before=%+v after=%+v", applied, row)
	}
}

// TestSelfHealTerminalFailureAdvancesClockWithoutSettlingError keeps a failed
// self-heal visit from stamping a terminal error onto an already applied row:
// the next visit happens after the interval, not every hour.
func TestSelfHealTerminalFailureAdvancesClockWithoutSettlingError(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	base := time.Now().UTC()
	f.service.now = func() time.Time { return base }
	applied := appliedTerminalStatus(t, f, 7)

	base = base.Add(reviewStatusSelfHealInterval + time.Minute)
	f.gitlab.mu.Lock()
	f.gitlab.notes[0].Body = reviewStatusMarkerLine + "\nstale"
	f.gitlab.statusNoteErr = &gitlab.Error{Op: "update_note", Status: 403}
	f.gitlab.mu.Unlock()
	if err := f.service.ReconcileAppliedStatuses(ctx); err != nil {
		t.Fatalf("ReconcileAppliedStatuses: %v", err)
	}
	job := claimStatusJob(t, f)
	if job == nil {
		t.Fatal("stale applied row was not enqueued")
	}
	if err := f.service.RunStatusJob(ctx, job); err != nil {
		t.Fatalf("RunStatusJob: %v", err)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.LastError != "" {
		t.Fatalf("self-heal stamped a terminal error: %+v", row)
	}
	if row.LastVerifiedAt.IsZero() || !row.LastVerifiedAt.After(applied.LastVerifiedAt) {
		t.Fatalf("self-heal failure did not advance verification: before=%+v after=%+v", applied, row)
	}
	if err := f.service.ReconcileAppliedStatuses(ctx); err != nil {
		t.Fatal(err)
	}
	if job := claimStatusJob(t, f); job != nil {
		t.Fatalf("failed self-heal retried within the interval: %+v", job)
	}
}

// TestSelfHealSkipsCancelledStatus stops status re-verification for cancelled
// merge requests (#459).
func TestSelfHealSkipsCancelledStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	base := time.Now().UTC()
	f.service.now = func() time.Time { return base }

	result, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: result.Generation,
		HeadSHA: f.head, State: reviewStatusCancelled,
	}); err != nil || !ok {
		t.Fatalf("SetReviewStatus(cancelled): %v %v", ok, err)
	}
	drainReviewJobs(t, f, "review.status")

	base = base.Add(reviewStatusSelfHealInterval + time.Minute)
	if err := f.service.ReconcileAppliedStatuses(ctx); err != nil {
		t.Fatalf("ReconcileAppliedStatuses: %v", err)
	}
	if job := claimStatusJob(t, f); job != nil {
		t.Fatalf("cancelled status was re-verified: %+v", job)
	}
}
