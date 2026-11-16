package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/jobs"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

// TestMentionCommandReceiptKeepsStatusNotePermalink reproduces the receipt
// defect of #485: the accepted mention reply must link the single status note
// even though enqueueing the next generation rewrote the status row.
func TestMentionCommandReceiptKeepsStatusNotePermalink(t *testing.T) {
	ctx := context.Background()
	db := openTestStore(t)
	cipher := newTestCipher(t)
	seedProject(t, db, cipher, 42, "whsec", "{}", "default")

	first, err := db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: sha40,
		Payload: json.RawMessage(`{"trigger":"command","note_id":500}`),
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	row, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if err := db.MarkReviewStatusApplied(ctx, 42, 7, int64(row.Version), 8801, "hash", ""); err != nil {
		t.Fatalf("MarkReviewStatusApplied: %v", err)
	}

	api := defaultFakeGitLab()
	api.discussions = []gitlab.Discussion{{
		ID:    "disc-1",
		Notes: []gitlab.Note{{ID: 555, Body: "@review-bot 请再审一次", AuthorID: 12}},
	}}
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store:        db,
		Workspace:    workspace.NewManager(t.TempDir(), nil),
		GitLab:       api,
		Cipher:       cipher,
		Profiles:     testProfiles(),
		BotUsernames: []string{"review-bot"},
	})
	if err != nil {
		t.Fatalf("NewCommandWorker: %v", err)
	}
	if err := worker.Handle(ctx, persistedCommandJob(t, db, mentionCommandPayload("disc-1"))); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	reply := commandReplyNote(t, api, 555)
	want := "https://gitlab.example.com/group/repo/-/merge_requests/7#note_8801"
	if !strings.Contains(reply.Body, want) {
		t.Fatalf("mention receipt is missing the status permalink %q: %q", want, reply.Body)
	}
	current, err := db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus after enqueue: %v", err)
	}
	if current.Generation <= first.Generation || current.NoteID != 8801 {
		t.Fatalf("status after mention enqueue = %+v, want generation > %d and note 8801",
			current, first.Generation)
	}
}

// TestMentionWebhookToReviewPersistsTrigger is the end-to-end regression for
// #485: a mention command reaches review.run with its own trigger, the
// storage layer accepts it, the review executes, and the MR keeps one status
// note carrying the phase table.
func TestMentionWebhookToReviewPersistsTrigger(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)

	api := defaultFakeGitLab()
	api.mr = &gitlab.MergeRequest{
		IID: 7, State: "opened", TargetProjectID: 42,
		TargetBranch: "main", SHA: f.head,
	}
	api.discussions = []gitlab.Discussion{{
		ID:    "disc-1",
		Notes: []gitlab.Note{{ID: 555, Body: "@review-bot 请再审一次", AuthorID: 12}},
	}}
	worker, err := NewCommandWorker(CommandWorkerConfig{
		Store:        f.db,
		Workspace:    f.manager,
		GitLab:       api,
		Cipher:       newTestCipher(t),
		Profiles:     testProfiles(),
		BotUsernames: []string{"review-bot"},
	})
	if err != nil {
		t.Fatalf("NewCommandWorker: %v", err)
	}
	if err := worker.Handle(ctx, persistedCommandJob(t, f.db, mentionCommandPayload("disc-1"))); err != nil {
		t.Fatalf("command Handle: %v", err)
	}

	job, err := f.db.Claim(ctx, "acceptance-485", []string{"review.run"}, time.Minute)
	if err != nil {
		t.Fatalf("Claim review.run: %v", err)
	}
	if job == nil {
		t.Fatal("mention command did not queue a review.run job")
	}
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["trigger"] != "mention" {
		t.Fatalf("review.run trigger = %v, want mention", payload["trigger"])
	}
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("review Handle: %v", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("mention review workflow: %v", err)
	}
	if wf.Trigger != "mention" || wf.Status != "completed" {
		t.Fatalf("workflow = %+v, want completed mention workflow", wf)
	}
	drainReviewJobs(t, f, "review.status")

	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if !terminalReviewStatus(row.State) || row.AppliedVersion != row.Version {
		t.Fatalf("status did not converge: %+v", row)
	}
	notes := f.gitlab.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("notes = %d, want the single status note", len(notes))
	}
	if !strings.Contains(notes[0].Body, reviewStatusTableHeader) {
		t.Fatalf("status note is missing the phase table: %q", notes[0].Body)
	}
	if row.NoteID != notes[0].ID {
		t.Fatalf("status note id = %d, remote note = %d", row.NoteID, notes[0].ID)
	}
}

// TestReviewRunRetryExhaustionSettlesFailedStatus reproduces #485: after the
// last attempt fails with a retryable internal error the worker dead-letters
// the job; the status row must reach terminal failed instead of staying at
// running.
func TestReviewRunRetryExhaustionSettlesFailedStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := seedExhaustibleReview(t, f)
	f.gitlab.mrErr = errors.New("gitlab is unavailable")

	err := f.service.Handle(ctx, job)
	if err == nil {
		t.Fatal("retryable upstream failure settled the job as done")
	}
	// The job worker marks the final attempt failed; the handler already wrote
	// the terminal status in the same attempt.
	if err := f.db.Fail(ctx, *job, err, nil); err != nil {
		t.Fatalf("Fail: %v", err)
	}
	stored, err := f.db.GetJob(ctx, job.ID)
	if err != nil {
		t.Fatalf("GetJob: %v", err)
	}
	if stored.State != store.JobFailed {
		t.Fatalf("job state = %q, want failed", stored.State)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.State != reviewStatusFailed || row.Detail != reviewExecutionFailedReason ||
		row.AppliedVersion != row.Version {
		t.Fatalf("status after exhausted retries = %+v", row)
	}
	notes := f.gitlab.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("notes = %d, want one terminal status note", len(notes))
	}
	if phase := parseStatusPhase(notes[0].Body); phase != reviewStatusPhase(reviewStatusFailed) {
		t.Fatalf("status note phase = %q, want failed: %q", phase, notes[0].Body)
	}
}

// seedExhaustibleReview queues one review.run job whose first attempt is also
// its last, so a retryable error exercises the terminal settlement.
func seedExhaustibleReview(t *testing.T, f *reviewFixture) *jobs.Job {
	t.Helper()
	return seedReviewJob(t, f, 1, json.RawMessage(`{"trigger":"mr_update","profile":"default"}`))
}

// seedReviewJob queues and claims one review.run job with the given budget and
// command payload.
func seedReviewJob(t *testing.T, f *reviewFixture, maxAttempts int, payload json.RawMessage) *jobs.Job {
	t.Helper()
	result, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload:     payload,
		MaxAttempts: maxAttempts,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	job, err := f.db.Claim(context.Background(), "acceptance-485", []string{"review.run"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("Claim = %+v, %v", job, err)
	}
	if job.ID != result.JobID {
		t.Fatalf("claimed job %s, want %s", job.ID, result.JobID)
	}
	return job
}

// supersedingGitLab activates a successor generation while the first run is
// collecting inputs, then fails retryably.
type supersedingGitLab struct {
	*reviewGitLab
	db   *sqlite.Store
	head string
	once sync.Once
}

func (g *supersedingGitLab) GetMergeRequest(ctx context.Context, _ string, projectID, iid int64) (*gitlab.MergeRequest, error) {
	g.once.Do(func() {
		_, _ = g.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
			ProjectID: projectID, MRIID: iid, HeadSHA: g.head,
			Payload:     json.RawMessage(`{"trigger":"mr_update","profile":"default"}`),
			MaxAttempts: 3,
		})
	})
	return nil, errors.New("gitlab is unavailable")
}

// TestReviewRunExhaustionLeavesSuccessorGeneration proves the exhausted-run
// settlement is fenced: a successor generation keeps its queued status.
func TestReviewRunExhaustionLeavesSuccessorGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := seedExhaustibleReview(t, f)
	f.service.gitlab = &supersedingGitLab{reviewGitLab: f.gitlab, db: f.db, head: f.head}

	if err := f.service.Handle(ctx, job); err == nil {
		t.Fatal("retryable upstream failure settled the job as done")
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.Generation != 2 || row.State != reviewStatusQueued {
		t.Fatalf("successor status was overwritten: %+v", row)
	}
	if notes := f.gitlab.noteSnapshot(); len(notes) != 0 {
		t.Fatalf("exhausted run wrote %d notes over the successor", len(notes))
	}
}

// TestReviewRunPermanentFailureKeepsStatusPath pins the unchanged permanent
// path: it still settles through the durable stale goal and its status job
// rather than the retry-exhaustion branch.
func TestReviewRunPermanentFailureKeepsStatusPath(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	f.gitlab.mr.State = "merged"
	job := f.seedReview(t, "permanent")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainReviewJobs(t, f, "review.status")
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.State != reviewStatusStale || row.Detail != "review is no longer current" ||
		row.AppliedVersion != row.Version {
		t.Fatalf("permanent failure status = %+v", row)
	}
}

// TestReviewRunSupersededKeepsSuccessorStatus pins the unchanged superseded
// path: an older generation never rewrites the successor's queued row.
func TestReviewRunSupersededKeepsSuccessorStatus(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := f.seedReview(t, "superseded")
	if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload:     json.RawMessage(`{"trigger":"mr_update","profile":"default"}`),
		MaxAttempts: 3,
	}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.Generation != 2 || row.State != reviewStatusQueued {
		t.Fatalf("superseded run rewrote the successor status: %+v", row)
	}
	if notes := f.gitlab.noteSnapshot(); len(notes) != 0 {
		t.Fatalf("superseded run wrote %d notes", len(notes))
	}
}

// TestStatusWriterReassignsNoteIDToMergedNote proves the #475 ownership
// transition still updates the durable id: a legacy row keeps its legacy note
// until the status writer creates the merged note, then the id follows it.
func TestStatusWriterReassignsNoteIDToMergedNote(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	run, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.gitlab.mu.Lock()
	f.gitlab.notes = append(f.gitlab.notes, gitlab.Note{
		ID: 701, AuthorID: f.gitlab.currentUser.ID,
		Body: legacyStatusMarkerPrefix + " v1 -->\n\n**AI 评审：评审运行中**\n",
	})
	f.gitlab.mu.Unlock()
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if err := f.db.MarkReviewStatusApplied(ctx, 42, 7, int64(row.Version), 701, "legacy-hash", ""); err != nil {
		t.Fatalf("MarkReviewStatusApplied legacy: %v", err)
	}
	body := review.SummaryMarkerPrefix + " v1 fp=published -->\n\n## 评审结论\n\nPASS\n"
	if _, err := f.db.SetReviewStatusFinalBody(ctx, 42, 7, run.Generation, f.head, body); err != nil {
		t.Fatalf("SetReviewStatusFinalBody: %v", err)
	}
	if row, err = f.db.GetReviewStatus(ctx, 42, 7); err != nil || row.NoteID != 701 {
		t.Fatalf("legacy note id was not preserved: %+v, %v", row, err)
	}
	drainReviewJobs(t, f, "review.status")

	notes := f.gitlab.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("notes = %+v, want the merged note only", notes)
	}
	row, err = f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.NoteID != notes[0].ID || notes[0].ID == 701 {
		t.Fatalf("status note id = %d, merged note = %d (legacy was 701)", row.NoteID, notes[0].ID)
	}
	if row.AppliedVersion != row.Version {
		t.Fatalf("merged note was not recorded as applied: %+v", row)
	}
}

// cancellingGitLab cancels the handler context while collecting inputs and
// then fails, which is how a lost lease or a stopping process reaches the
// handler.
type cancellingGitLab struct {
	*reviewGitLab
	cancel context.CancelFunc
}

func (g *cancellingGitLab) GetMergeRequest(context.Context, string, int64, int64) (*gitlab.MergeRequest, error) {
	g.cancel()
	return nil, context.Canceled
}

// TestReviewRunExhaustionSettlesAfterContextCancel reproduces the cancellation
// edge of #485: the worker does not call Fail after cancelling a handler, and
// recovery dead-letters an expired final attempt without running it again, so
// the terminal failed status must be written through a detached context.
func TestReviewRunExhaustionSettlesAfterContextCancel(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := seedExhaustibleReview(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.service.gitlab = &cancellingGitLab{reviewGitLab: f.gitlab, cancel: cancel}

	err := f.service.Handle(ctx, job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle error = %v, want context.Canceled", err)
	}
	row, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.State != reviewStatusFailed || row.Detail != reviewExecutionFailedReason ||
		row.AppliedVersion != row.Version {
		t.Fatalf("cancelled final attempt left status = %+v", row)
	}
	notes := f.gitlab.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("notes = %d, want one terminal status note", len(notes))
	}
	if phase := parseStatusPhase(notes[0].Body); phase != reviewStatusPhase(reviewStatusFailed) {
		t.Fatalf("status note phase = %q, want failed: %q", phase, notes[0].Body)
	}
}

// TestReviewRunCancellationBeforeExhaustionKeepsRetryable proves the detached
// settlement stays behind the budget fence: a cancelled attempt that still has
// retries keeps its running row for the retry.
func TestReviewRunCancellationBeforeExhaustionKeepsRetryable(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := seedReviewJob(t, f, 3, json.RawMessage(`{"trigger":"mr_update","profile":"default"}`))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.service.gitlab = &cancellingGitLab{reviewGitLab: f.gitlab, cancel: cancel}

	err := f.service.Handle(ctx, job)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Handle error = %v, want context.Canceled", err)
	}
	row, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.State != reviewStatusRunning || row.AppliedVersion >= row.Version {
		t.Fatalf("retryable cancellation settled the row: %+v", row)
	}
	if notes := f.gitlab.noteSnapshot(); len(notes) != 0 {
		t.Fatalf("retryable cancellation wrote %d notes", len(notes))
	}
}

// TestReviewRunExhaustedSettlementSkipsCompletedWorkflow pins the guard that
// leaves a completed run to publication recovery instead of declaring it
// failed.
func TestReviewRunExhaustedSettlementSkipsCompletedWorkflow(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := f.seedReview(t, "completed")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || !terminalReviewStatus(before.State) {
		t.Fatalf("completed run status = %+v, %v", before, err)
	}
	// Make the row look like a final attempt that failed after the workflow
	// completed; the guard must leave it to publication recovery.
	if written, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: before.Generation,
		Attempt: before.Attempt + 1, HeadSHA: f.head, Profile: "default",
		State: reviewStatusRunning,
	}); err != nil || !written {
		t.Fatalf("SetReviewStatus running = %v, %v", written, err)
	}
	f.service.settleExhaustedRun(ctx, reviewRunPayload{
		ProjectID: 42, MRIID: 7, Generation: before.Generation,
		HeadSHA: f.head, Profile: "default",
	})
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.State != reviewStatusRunning {
		t.Fatalf("completed workflow was declared failed: %+v", row)
	}
	notes := f.gitlab.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("notes = %d, want the completed review note", len(notes))
	}
	if phase := parseStatusPhase(notes[0].Body); phase == reviewStatusPhase(reviewStatusFailed) {
		t.Fatalf("completed review note was rewritten as failed: %q", notes[0].Body)
	}
}

// TestReviewRunExhaustionClearsSourceNoteEyes covers the settlement reaction:
// the source note must lose its eyes when the final attempt fails.
func TestReviewRunExhaustionClearsSourceNoteEyes(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := seedReviewJob(t, f, 1, json.RawMessage(`{"trigger":"command","note_id":555}`))
	reaction, err := f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardEyes {
		t.Fatalf("source note reaction before run = %+v, %v", reaction, err)
	}
	f.gitlab.mrErr = errors.New("gitlab is unavailable")

	if err := f.service.Handle(ctx, job); err == nil {
		t.Fatal("retryable upstream failure settled the job as done")
	}
	reaction, err = f.db.GetReviewReaction(ctx, 42, 7, 555)
	if err != nil {
		t.Fatalf("GetReviewReaction: %v", err)
	}
	if reaction.Desired != reviewAwardNone {
		t.Fatalf("source note reaction after exhaustion = %+v, want none", reaction)
	}
}

// TestReviewRunFailureSettlementFallsBackToStatusJob proves the durable-goal
// fallback: when the terminal note cannot be written, the failed row is
// already durable and its status job converges the note later.
func TestReviewRunFailureSettlementFallsBackToStatusJob(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := seedExhaustibleReview(t, f)
	// The run fails and the status note write fails as well.
	f.gitlab.mrErr = errors.New("gitlab is unavailable")
	f.gitlab.mu.Lock()
	f.gitlab.noteErr = errors.New("gitlab notes are unavailable")
	f.gitlab.mu.Unlock()

	if err := f.service.Handle(ctx, job); err == nil {
		t.Fatal("retryable upstream failure settled the job as done")
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.State != reviewStatusFailed || row.AppliedVersion >= row.Version {
		t.Fatalf("failed goal was not persisted for the status job: %+v", row)
	}
	if notes := f.gitlab.noteSnapshot(); len(notes) != 0 {
		t.Fatalf("status note was written while GitLab was unavailable: %d", len(notes))
	}
	// The upstream recovers; the queued status job converges the failed note.
	f.gitlab.mu.Lock()
	f.gitlab.noteErr = nil
	f.gitlab.mu.Unlock()
	drainReviewJobs(t, f, "review.status")

	row, err = f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus after drain: %v", err)
	}
	if row.State != reviewStatusFailed || row.AppliedVersion != row.Version || row.NoteID <= 0 {
		t.Fatalf("status job did not converge: %+v", row)
	}
	notes := f.gitlab.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("notes = %d, want one recovered status note", len(notes))
	}
	if phase := parseStatusPhase(notes[0].Body); phase != reviewStatusPhase(reviewStatusFailed) {
		t.Fatalf("recovered status phase = %q: %q", phase, notes[0].Body)
	}
}
