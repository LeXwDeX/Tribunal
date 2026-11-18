package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type mrSerialActorKey struct{}

type mrSerialWrite struct {
	actor  string
	kind   string
	sha    string
	state  string
	target string
	body   string
	noteID int64
}

type mrSerialGitLab struct {
	*reviewGitLab
	arrived       chan struct{}
	release       chan struct{}
	updateArrived chan struct{}
	updateRelease chan struct{}
	mu            sync.Mutex
	writes        []mrSerialWrite
}

func (g *mrSerialGitLab) GetMergeRequest(ctx context.Context, token string, projectID, iid int64) (*gitlab.MergeRequest, error) {
	if ctx.Value(mrSerialActorKey{}) == "A" && g.arrived != nil {
		select {
		case <-g.arrived:
		default:
			close(g.arrived)
		}
		select {
		case <-g.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return g.reviewGitLab.GetMergeRequest(ctx, token, projectID, iid)
}

func (g *mrSerialGitLab) record(w mrSerialWrite) {
	g.mu.Lock()
	g.writes = append(g.writes, w)
	g.mu.Unlock()
}

func (g *mrSerialGitLab) CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error) {
	note, err := g.reviewGitLab.CreateMRNote(ctx, token, projectID, iid, body)
	if err == nil && strings.HasPrefix(body, review.SummaryMarkerPrefix) {
		g.record(mrSerialWrite{
			actor: fmt.Sprint(ctx.Value(mrSerialActorKey{})), kind: "summary-create", noteID: note.ID, body: body,
		})
	}
	return note, err
}

func (g *mrSerialGitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	if g.updateRelease != nil && ctx.Value(mrSerialActorKey{}) == "A" {
		select {
		case <-g.updateArrived:
		default:
			close(g.updateArrived)
		}
		select {
		case <-g.updateRelease:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	err := g.reviewGitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
	if err == nil && strings.HasPrefix(body, review.SummaryMarkerPrefix) {
		g.record(mrSerialWrite{
			actor: fmt.Sprint(ctx.Value(mrSerialActorKey{})), kind: "summary-update", noteID: noteID, body: body,
		})
	}
	return err
}

func (g *mrSerialGitLab) SetCommitStatus(ctx context.Context, token string, projectID int64, sha string, status gitlab.CommitStatus) error {
	err := g.reviewGitLab.SetCommitStatus(ctx, token, projectID, sha, status)
	if err == nil {
		g.record(mrSerialWrite{
			actor: fmt.Sprint(ctx.Value(mrSerialActorKey{})), kind: "commit-status",
			sha: sha, state: status.State, target: status.TargetURL,
		})
	}
	return err
}

func (g *mrSerialGitLab) snapshot() []mrSerialWrite {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]mrSerialWrite(nil), g.writes...)
}

func mrSerialActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, mrSerialActorKey{}, actor)
}

func mrSerialWait(t *testing.T, ch <-chan error, what string) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(30 * time.Second):
		t.Fatalf("%s did not finish", what)
		return nil
	}
}

func mrSerialWritesBy(writes []mrSerialWrite, actor string) []mrSerialWrite {
	var out []mrSerialWrite
	for _, write := range writes {
		if write.actor == actor {
			out = append(out, write)
		}
	}
	return out
}

// mrRegistryRefs exposes the test-only view of the keyed-lock reference count:
// one holder plus any waiters on the same merge request.
func mrRegistryRefs(locks *workflowLocks, projectID, mrIID int64) int {
	key := reviewMRLockKey(projectID, mrIID)
	locks.mu.Lock()
	defer locks.mu.Unlock()
	entry := locks.entries[key]
	if entry == nil {
		return 0
	}
	return entry.refs
}

// mrLockRefs exposes the publisher registry's reference count.
func (p *Publisher) mrLockRefs(projectID, mrIID int64) int {
	return mrRegistryRefs(p.mrLocks, projectID, mrIID)
}

// mrSerialWaitRefs waits until exactly want references exist for the MR lock.
// Two references mean the holder plus one waiter, so the test no longer relies
// on a timeout to infer that a publication is queued behind the lock.
func mrSerialWaitRefs(t *testing.T, ctx context.Context, p *Publisher, want int) {
	t.Helper()
	for {
		if got := p.mrLockRefs(42, 7); got == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("MR lock refs = %d, want %d: %v", p.mrLockRefs(42, 7), want, ctx.Err())
		case <-time.After(2 * time.Millisecond):
		}
	}
}

// TestOlderGenerationCannotRepublishAfterNewerGeneration covers the normal
// publication path: an older workflow blocks after its control check while a
// newer generation publishes, then resumes. The old generation must not write
// the summary or commit status after the newer review owns the MR.
func TestOlderGenerationCannotRepublishAfterNewerGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.publisher.publicURL = "https://console.invalid"
	profile := f.profiles["default"]
	profile.Challenge.Only = []string{"info"}
	f.profiles["default"] = profile

	aJob := f.seedReview(t, "serial-a")
	if err := f.service.RunReview(ctx, aJob); err != nil {
		t.Fatalf("run A: %v", err)
	}
	a, err := f.db.GetWorkflowByJob(ctx, aJob.ID)
	if err != nil || a.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("A workflow=%+v err=%v", a, err)
	}
	aDossier, err := f.db.GetWorkflowDossier(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}

	// B blocks the MR: its non-PASS verdict must own the final summary.
	run := f.runner.Run
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		result, err := run(ctx, spec)
		if err != nil {
			return nil, err
		}
		if out, ok := result.Output.(*review.CodeReviewOut); ok && len(out.Findings) > 0 {
			out.Findings[0].Severity = review.SeverityP1
		}
		return result, nil
	}

	g := &mrSerialGitLab{
		reviewGitLab: f.gitlab, arrived: make(chan struct{}), release: make(chan struct{}),
	}
	f.publisher.gitlab = g
	dispatcher, err := NewDispatcher(DispatcherConfig{
		Store: f.db, Workspace: f.manager, GitLab: f.gitlab,
		Cipher: f.publisher.cipher, Profiles: f.profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishID, err := dispatcher.RepublishReview(ctx, a.ID)
	if err != nil {
		t.Fatalf("enqueue A republish: %v", err)
	}
	aPublish, err := f.db.Claim(ctx, "worker-A-publish", []string{"review.publish"}, time.Minute)
	if err != nil || aPublish == nil || aPublish.ID != publishID {
		t.Fatalf("claim A review.publish: job=%v err=%v", aPublish, err)
	}

	aDone := make(chan error, 1)
	go func() {
		aDone <- f.service.RepublishReview(mrSerialActor(ctx, "A"), aPublish)
	}()
	select {
	case <-g.arrived:
	case err := <-aDone:
		t.Fatalf("A exited before the post-control barrier: %v", err)
	case <-ctx.Done():
		t.Fatalf("A did not reach the barrier: %v", ctx.Err())
	}

	bJobID, err := dispatcher.StartReview(ctx, 42, 7, "default", true)
	if err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	bRun, err := f.db.Claim(ctx, "worker-B-run", []string{"review.run"}, time.Minute)
	if err != nil || bRun == nil || bRun.ID != bJobID {
		t.Fatalf("claim B review.run: job=%v err=%v", bRun, err)
	}
	bDone := make(chan error, 1)
	go func() {
		bDone <- f.service.RunReview(mrSerialActor(ctx, "B"), bRun)
	}()

	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation <= aDossier.Generation {
		t.Fatalf("control=%+v A generation=%d err=%v", control, aDossier.Generation, err)
	}
	// B has entered the publication path and is waiting on the same MR lock.
	mrSerialWaitRefs(t, ctx, f.publisher, 2)
	close(g.release)

	if err := mrSerialWait(t, aDone, "A republish"); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("A republish error = %v, want ErrStalePublication", err)
	}
	if err := mrSerialWait(t, bDone, "B review"); err != nil {
		t.Fatalf("B review: %v", err)
	}
	if err := f.db.Complete(ctx, *aPublish); err != nil {
		t.Fatalf("complete A publish job: %v", err)
	}
	if err := f.db.Complete(ctx, *bRun); err != nil {
		t.Fatalf("complete B job: %v", err)
	}

	writes := g.snapshot()
	if old := mrSerialWritesBy(writes, "A"); len(old) != 0 {
		t.Fatalf("older generation wrote after the newer generation: %+v", old)
	}
	bw := mrSerialWritesBy(writes, "B")
	var bSummary, bStatus *mrSerialWrite
	for i := range bw {
		switch bw[i].kind {
		case "summary-create", "summary-update":
			bSummary = &bw[i]
		case "commit-status":
			bStatus = &bw[i]
		}
	}
	if bSummary == nil || bStatus == nil || !strings.Contains(bSummary.body, "BLOCKING") {
		t.Fatalf("B writes = %+v", bw)
	}
	b, err := f.db.GetWorkflowByJob(ctx, bRun.ID)
	if err != nil || b.ID == a.ID || b.Verdict != review.VerdictBlocking || b.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("B workflow=%+v err=%v", b, err)
	}
	if bStatus.sha != f.head || bStatus.state != "failed" || !strings.Contains(bStatus.target, b.ID) {
		t.Fatalf("B commit status = %+v", *bStatus)
	}
	summary, err := f.db.GetPublication(ctx, 42, 7, summaryPublishKind, summaryPublicationKey)
	if err != nil || summary.WorkflowID != b.ID || summary.BodyHash != bodyHash(bSummary.body) {
		t.Fatalf("final summary=%+v err=%v", summary, err)
	}
	status, err := f.db.GetPublication(ctx, 42, 7, statusPublishKind, commitStatusPublishKey(f.head, "failed"))
	if err != nil || status.WorkflowID != b.ID {
		t.Fatalf("final status=%+v err=%v", status, err)
	}
	aAfter, err := f.db.GetWorkflow(ctx, a.ID)
	if err != nil || aAfter.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("A state after stale republish=%+v err=%v", aAfter, err)
	}
}

// TestFailureSummaryCannotOverwriteNewerGeneration covers the failure path:
// a failed older workflow must not publish its failure summary after a newer
// generation published the MR.
func TestFailureSummaryCannotOverwriteNewerGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.defaults.PostFailureNote = false
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	aJob := f.seedReview(t, "failure-a")
	if err := f.service.RunReview(ctx, aJob); err != nil {
		t.Fatalf("run A: %v", err)
	}
	a, err := f.db.GetWorkflowByJob(ctx, aJob.ID)
	if err != nil || a.Status != "failed" {
		t.Fatalf("A workflow=%+v err=%v", a, err)
	}
	aDossier, err := f.db.GetWorkflowDossier(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}

	f.service.agentRun = f.runner.Run
	g := &mrSerialGitLab{reviewGitLab: f.gitlab, arrived: make(chan struct{}), release: make(chan struct{})}
	f.publisher.gitlab = g
	dispatcher, err := NewDispatcher(DispatcherConfig{
		Store: f.db, Workspace: f.manager, GitLab: f.gitlab,
		Cipher: f.publisher.cipher, Profiles: f.profiles,
	})
	if err != nil {
		t.Fatal(err)
	}

	aDone := make(chan error, 1)
	go func() {
		aDone <- f.publisher.PublishFailure(mrSerialActor(ctx, "A"), a.ID, "review execution failed")
	}()
	select {
	case <-g.arrived:
	case err := <-aDone:
		t.Fatalf("A exited before the post-control barrier: %v", err)
	case <-ctx.Done():
		t.Fatalf("A did not reach the barrier: %v", ctx.Err())
	}

	bJobID, err := dispatcher.StartReview(ctx, 42, 7, "default", true)
	if err != nil {
		t.Fatalf("enqueue B: %v", err)
	}
	bRun, err := f.db.Claim(ctx, "worker-B-run", []string{"review.run"}, time.Minute)
	if err != nil || bRun == nil || bRun.ID != bJobID {
		t.Fatalf("claim B review.run: job=%v err=%v", bRun, err)
	}
	bDone := make(chan error, 1)
	go func() {
		bDone <- f.service.RunReview(mrSerialActor(ctx, "B"), bRun)
	}()

	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation <= aDossier.Generation {
		t.Fatalf("control=%+v A generation=%d err=%v", control, aDossier.Generation, err)
	}
	mrSerialWaitRefs(t, ctx, f.publisher, 2)
	close(g.release)

	if err := mrSerialWait(t, aDone, "A failure publish"); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("A failure publish error = %v, want ErrStalePublication", err)
	}
	if err := mrSerialWait(t, bDone, "B review"); err != nil {
		t.Fatalf("B review: %v", err)
	}
	if err := f.db.Complete(ctx, *bRun); err != nil {
		t.Fatalf("complete B job: %v", err)
	}

	writes := g.snapshot()
	if old := mrSerialWritesBy(writes, "A"); len(old) != 0 {
		t.Fatalf("failed older generation wrote after the newer generation: %+v", old)
	}
	b, err := f.db.GetWorkflowByJob(ctx, bRun.ID)
	if err != nil || b.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("B workflow=%+v err=%v", b, err)
	}
	summary, err := f.db.GetPublication(ctx, 42, 7, summaryPublishKind, summaryPublicationKey)
	if err != nil || summary.WorkflowID != b.ID {
		t.Fatalf("final summary=%+v err=%v", summary, err)
	}
	body := ""
	f.gitlab.mu.Lock()
	for i := range f.gitlab.notes {
		if f.gitlab.notes[i].ID == mustNoteID(t, summary.GitlabID) {
			body = f.gitlab.notes[i].Body
		}
	}
	f.gitlab.mu.Unlock()
	if strings.Contains(body, "评审未完成") {
		t.Fatalf("failure summary replaced the newer summary: %q", body)
	}
}

func mustNoteID(t *testing.T, value string) int64 {
	t.Helper()
	id, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		t.Fatalf("note id %q: %v", value, err)
	}
	return id
}

// TestStandaloneSummaryRejectsNewerGeneration covers the empty-review entry:
// publishStandaloneSummaryLocked is a real summary writer, so it must recheck
// the review generation before the write.
func TestStandaloneSummaryRejectsNewerGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.seedReview(t, "standalone")
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.publisher.publishStandaloneSummaryLocked(ctx, 42, 7, control.Generation, control.HeadSHA, "token", "first", review.SummaryMarkerPrefix+"first"); err != nil {
		t.Fatalf("first standalone summary: %v", err)
	}
	f.seedReview(t, "standalone-next")
	before := len(f.gitlab.notes)
	err = f.publisher.publishStandaloneSummaryLocked(ctx, 42, 7, control.Generation, control.HeadSHA, "token", "stale", review.SummaryMarkerPrefix+"stale")
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("standalone summary error = %v, want ErrStalePublication", err)
	}
	if len(f.gitlab.notes) != before || !strings.Contains(f.gitlab.notes[0].Body, "first") {
		t.Fatalf("stale standalone summary changed notes: %+v", f.gitlab.notes)
	}
}

// TestMRPublicationLocksAreIndependent proves that the MR lock keys on
// (project_id, mr_iid): another MR can publish while one is held, and an
// abandoned waiter disappears after cancellation.
func TestMRPublicationLocksAreIndependent(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	unlock, err := f.publisher.lockMR(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}

	other := make(chan error, 1)
	go func() {
		unlockOther, err := f.publisher.lockMR(ctx, 42, 8)
		if err == nil {
			unlockOther()
		}
		other <- err
	}()
	select {
	case err := <-other:
		if err != nil {
			t.Fatalf("different MR lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("different MR waited for the held MR lock")
	}

	waiterCtx, waiterCancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer waiterCancel()
	if _, err := f.publisher.lockMR(waiterCtx, 42, 7); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("same MR waiter error = %v, want deadline exceeded", err)
	}
	unlock()
	unlockAgain, err := f.publisher.lockMR(ctx, 42, 7)
	if err != nil {
		t.Fatalf("lock after a cancelled waiter: %v", err)
	}
	unlockAgain()
}

func mrSerialBumpControl(t *testing.T, f *reviewFixture) func() {
	t.Helper()
	return func() {
		_, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
			ProjectID: 42, MRIID: 7, HeadSHA: f.head,
			Payload:     json.RawMessage(`{"trigger":"manual"}`),
			MaxAttempts: 3,
		})
		if err != nil {
			t.Errorf("bump review control: %v", err)
		}
	}
}

type summaryUpdateRetryGitLab struct {
	*reviewGitLab
	mu      sync.Mutex
	updates int
	bump    func()
}

func (g *summaryUpdateRetryGitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	g.mu.Lock()
	g.updates++
	first := g.updates == 1
	g.mu.Unlock()
	if first {
		g.bump()
		return errSummaryTransport
	}
	return g.reviewGitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
}

func (g *summaryUpdateRetryGitLab) attempts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.updates
}

// TestSummaryUpdateRetryRechecksGeneration fails the first UpdateMRNote, then
// changes the control generation. The retry must not send the old summary again.
func TestSummaryUpdateRetryRechecksGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "summary-update-retry")
	if err := f.publisher.Publish(ctx, wf.ID); err != nil {
		t.Fatalf("initial publish: %v", err)
	}
	// Force the republish to rewrite the note, so the first update reaches the
	// retrying transport below.
	staleSummaryBody514(t, f.gitlab)
	api := &summaryUpdateRetryGitLab{reviewGitLab: f.gitlab, bump: mrSerialBumpControl(t, f)}
	f.publisher.gitlab = api
	err := f.publisher.Republish(ctx, wf.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("republish error = %v, want ErrStalePublication", err)
	}
	if got := api.attempts(); got != 1 {
		t.Fatalf("summary update attempts = %d, want 1", got)
	}
}

type statusRetryGitLab struct {
	*reviewGitLab
	mu      sync.Mutex
	updates int
	bump    func()
}

func (g *statusRetryGitLab) SetCommitStatus(ctx context.Context, token string, projectID int64, sha string, status gitlab.CommitStatus) error {
	g.mu.Lock()
	g.updates++
	first := g.updates == 1
	g.mu.Unlock()
	if first {
		g.bump()
		return errSummaryTransport
	}
	return g.reviewGitLab.SetCommitStatus(ctx, token, projectID, sha, status)
}

func (g *statusRetryGitLab) attempts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.updates
}

// TestCommitStatusRetryRechecksGeneration fails the first SetCommitStatus,
// then changes the control generation. The retry must not send the old status.
func TestCommitStatusRetryRechecksGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	wf := reviewWithFailedNote(t, f, "status-retry")
	api := &statusRetryGitLab{reviewGitLab: f.gitlab, bump: mrSerialBumpControl(t, f)}
	f.publisher.gitlab = api
	err := f.publisher.Publish(ctx, wf.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("publish error = %v, want ErrStalePublication", err)
	}
	if got := api.attempts(); got != 1 {
		t.Fatalf("commit status attempts = %d, want 1", got)
	}
	for _, state := range []string{"success", "failed"} {
		if _, err := f.db.GetPublication(ctx, 42, 7, statusPublishKind, commitStatusPublishKey(f.head, state)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("status publication %s after stale attempt: %v", state, err)
		}
	}
}

type summaryCreateRetryGitLab struct {
	*reviewGitLab
	mu      sync.Mutex
	creates int
	bump    func()
}

func (g *summaryCreateRetryGitLab) CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error) {
	g.mu.Lock()
	g.creates++
	first := g.creates == 1
	g.mu.Unlock()
	if first {
		g.bump()
		return nil, errSummaryTransport
	}
	return g.reviewGitLab.CreateMRNote(ctx, token, projectID, iid, body)
}

func (g *summaryCreateRetryGitLab) attempts() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.creates
}

// TestSummaryCreateRetryRechecksGeneration pins the existing create-loop
// behavior: the second create attempt must not run after the generation changed.
func TestSummaryCreateRetryRechecksGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "summary-create-retry")
	api := &summaryCreateRetryGitLab{reviewGitLab: f.gitlab, bump: mrSerialBumpControl(t, f)}
	f.publisher.gitlab = api
	err := f.publisher.Publish(ctx, wf.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("publish error = %v, want ErrStalePublication", err)
	}
	if got := api.attempts(); got != 1 {
		t.Fatalf("summary create attempts = %d, want 1", got)
	}
	if len(f.gitlab.notes) != 0 {
		t.Fatalf("stale summary was created: %+v", f.gitlab.notes)
	}
}

// TestStandaloneSummaryWaitsForPublicationLock proves that the standalone
// summary entry waits on the same MR lock as a normal publication entry.
func TestStandaloneSummaryWaitsForPublicationLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	aJob := f.seedReview(t, "standalone-lock-a")
	if err := f.service.RunReview(ctx, aJob); err != nil {
		t.Fatalf("run A: %v", err)
	}
	a, err := f.db.GetWorkflowByJob(ctx, aJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}

	g := &mrSerialGitLab{
		reviewGitLab: f.gitlab, arrived: make(chan struct{}), release: make(chan struct{}),
	}
	f.publisher.gitlab = g
	dispatcher, err := NewDispatcher(DispatcherConfig{
		Store: f.db, Workspace: f.manager, GitLab: f.gitlab,
		Cipher: f.publisher.cipher, Profiles: f.profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishID, err := dispatcher.RepublishReview(ctx, a.ID)
	if err != nil {
		t.Fatalf("enqueue A republish: %v", err)
	}
	aPublish, err := f.db.Claim(ctx, "worker-A-publish", []string{"review.publish"}, time.Minute)
	if err != nil || aPublish == nil || aPublish.ID != publishID {
		t.Fatalf("claim A review.publish: job=%v err=%v", aPublish, err)
	}
	aDone := make(chan error, 1)
	go func() {
		aDone <- f.service.RepublishReview(mrSerialActor(ctx, "A"), aPublish)
	}()
	select {
	case <-g.arrived:
	case err := <-aDone:
		t.Fatalf("A exited before the barrier: %v", err)
	case <-ctx.Done():
		t.Fatalf("A did not reach the barrier: %v", ctx.Err())
	}

	standaloneDone := make(chan error, 1)
	go func() {
		standaloneDone <- f.publisher.publishStandaloneSummaryLocked(ctx, 42, 7,
			control.Generation, control.HeadSHA, "token", "standalone", review.SummaryMarkerPrefix+"standalone")
	}()
	mrSerialWaitRefs(t, ctx, f.publisher, 2)
	close(g.release)
	if err := mrSerialWait(t, aDone, "A republish"); err != nil {
		t.Fatalf("A republish: %v", err)
	}
	if err := mrSerialWait(t, standaloneDone, "standalone summary"); err != nil {
		t.Fatalf("standalone summary: %v", err)
	}
	f.gitlab.mu.Lock()
	body := f.gitlab.notes[len(f.gitlab.notes)-1].Body
	f.gitlab.mu.Unlock()
	if !strings.Contains(body, "standalone") {
		t.Fatalf("final summary = %q, want the standalone summary", body)
	}
}

// TestInFlightSummaryUpdateIsNotInterleavedByNewerGeneration blocks the older
// generation inside the real UpdateMRNote call, after its final pre-write
// recheck. The successor arrives through webhook ingress, which does not take
// the per-MR lock (it must stay inside the 900ms processing budget), so the
// generation bump lands underneath the in-flight write. The old generation may
// finish only that single write and then stops at its next recheck; the
// successor rechecks under the same lock and writes after it (#475).
func TestInFlightSummaryUpdateIsNotInterleavedByNewerGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.publisher.publicURL = "https://console.invalid"
	profile := f.profiles["default"]
	profile.Challenge.Only = []string{"info"}
	f.profiles["default"] = profile

	aJob := f.seedReview(t, "midwrite-a")
	if err := f.service.RunReview(ctx, aJob); err != nil {
		t.Fatalf("run A: %v", err)
	}
	a, err := f.db.GetWorkflowByJob(ctx, aJob.ID)
	if err != nil {
		t.Fatal(err)
	}
	aDossier, err := f.db.GetWorkflowDossier(ctx, a.ID)
	if err != nil {
		t.Fatal(err)
	}

	run := f.runner.Run
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		result, err := run(ctx, spec)
		if err != nil {
			return nil, err
		}
		if out, ok := result.Output.(*review.CodeReviewOut); ok && len(out.Findings) > 0 {
			out.Findings[0].Severity = review.SeverityP1
		}
		return result, nil
	}

	g := &mrSerialGitLab{
		reviewGitLab:  f.gitlab,
		updateArrived: make(chan struct{}),
		updateRelease: make(chan struct{}),
	}
	// Force A's republish to rewrite the existing summary note; an identical
	// body would skip the update and never reach the in-flight barrier (#514).
	staleSummaryBody514(t, f.gitlab)
	f.publisher.gitlab = g
	dispatcher, err := NewDispatcher(DispatcherConfig{
		Store: f.db, Workspace: f.manager, GitLab: f.gitlab,
		Cipher: f.publisher.cipher, Profiles: f.profiles,
	})
	if err != nil {
		t.Fatal(err)
	}
	publishID, err := dispatcher.RepublishReview(ctx, a.ID)
	if err != nil {
		t.Fatalf("enqueue A republish: %v", err)
	}
	aPublish, err := f.db.Claim(ctx, "worker-A-publish", []string{"review.publish"}, time.Minute)
	if err != nil || aPublish == nil || aPublish.ID != publishID {
		t.Fatalf("claim A review.publish: job=%v err=%v", aPublish, err)
	}
	aDone := make(chan error, 1)
	go func() {
		aDone <- f.service.RepublishReview(mrSerialActor(ctx, "A"), aPublish)
	}()
	select {
	case <-g.updateArrived:
	case err := <-aDone:
		t.Fatalf("A exited before the in-flight summary write: %v", err)
	case <-ctx.Done():
		t.Fatalf("A did not reach the in-flight summary write: %v", ctx.Err())
	}

	// The successor event arrives while the old summary write is in flight.
	// Ingress only records the delivery and bumps the generation; it must not
	// wait for the held MR lock.
	webhookDefaults := f.defaults
	webhookDefaults.AllowRepoConfig = false
	webhooks, err := NewWebhookService(WebhookServiceConfig{
		Store: f.db, Workspace: f.manager, Cipher: newTestCipher(t),
		Profiles: f.profiles, ReviewDefaults: webhookDefaults,
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	ingressDone := make(chan struct{})
	var ingressResult WebhookResult
	var ingressErr error
	go func() {
		ingressResult, ingressErr = webhooks.Handle(ctx, WebhookRequest{
			Event: "Merge Request Hook", Token: "whsec", UUID: uuid.NewString(),
			Body: mrBody(42, 7, func(body map[string]any) {
				patchAttr(body, "action", "reopen")
				patchAttr(body, "last_commit", map[string]any{"id": f.head})
				patchMR(body, "last_commit", map[string]any{"id": f.head})
			}),
		})
		close(ingressDone)
	}()
	select {
	case <-ingressDone:
	case err := <-aDone:
		t.Fatalf("A exited before the in-flight summary write: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("successor ingress waited for the MR lock instead of bumping the generation")
	}
	if ingressErr != nil || !ingressResult.Accepted {
		t.Fatalf("successor ingress = %+v, %v", ingressResult, ingressErr)
	}
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != aDossier.Generation+1 {
		t.Fatalf("generation did not advance under the in-flight write: %+v, %v", control, err)
	}
	if writes := g.snapshot(); len(writes) != 0 {
		close(g.updateRelease)
		t.Fatalf("writes completed while the old summary write was still in flight: %+v", writes)
	}
	close(g.updateRelease)

	// The release lets A finish exactly its in-flight write; its next recheck
	// sees the successor generation and settles as stale.
	if err := mrSerialWait(t, aDone, "A republish"); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("A republish error = %v, want ErrStalePublication", err)
	}

	bRun, err := f.db.Claim(ctx, "worker-B-run", []string{"review.run"}, time.Minute)
	if err != nil || bRun == nil || bRun.ID != ingressResult.JobID {
		t.Fatalf("claim B review.run: job=%v err=%v want %s", bRun, err, ingressResult.JobID)
	}
	bDone := make(chan error, 1)
	go func() {
		bDone <- f.service.RunReview(mrSerialActor(ctx, "B"), bRun)
	}()
	if err := mrSerialWait(t, bDone, "B review"); err != nil {
		t.Fatalf("B review: %v", err)
	}
	if err := f.db.Complete(ctx, *aPublish); err != nil {
		t.Fatalf("complete A publish job: %v", err)
	}
	if err := f.db.Complete(ctx, *bRun); err != nil {
		t.Fatalf("complete B job: %v", err)
	}

	writes := g.snapshot()
	if len(writes) != 3 {
		t.Fatalf("writes = %+v, want A summary, B summary, B commit status", writes)
	}
	if writes[0].actor != "A" || writes[0].kind != "summary-update" {
		t.Fatalf("first write = %+v, want A's in-flight summary", writes[0])
	}
	if writes[1].actor != "B" || (writes[1].kind != "summary-update" && writes[1].kind != "summary-create") {
		t.Fatalf("second write = %+v, want B's summary after A stopped", writes[1])
	}
	if writes[2].actor != "B" || writes[2].kind != "commit-status" || writes[2].state != "failed" {
		t.Fatalf("third write = %+v, want B's failed commit status", writes[2])
	}
	b, err := f.db.GetWorkflowByJob(ctx, bRun.ID)
	if err != nil || b.Verdict != review.VerdictBlocking || b.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("B workflow=%+v err=%v", b, err)
	}
	aAfter, err := f.db.GetWorkflow(ctx, a.ID)
	if err != nil || aAfter.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("A did not stop at the generation bump: %+v, %v", aAfter, err)
	}
	summary, err := f.db.GetPublication(ctx, 42, 7, summaryPublishKind, summaryPublicationKey)
	if err != nil || summary.WorkflowID != b.ID || summary.BodyHash != bodyHash(writes[1].body) {
		t.Fatalf("final summary=%+v err=%v, want B's body hash", summary, err)
	}
	f.gitlab.mu.Lock()
	var body string
	for i := range f.gitlab.notes {
		if f.gitlab.notes[i].ID == mustNoteID(t, summary.GitlabID) {
			body = f.gitlab.notes[i].Body
		}
	}
	f.gitlab.mu.Unlock()
	if body != writes[1].body {
		t.Fatalf("remote summary = %q, want the successor's last write %q", body, writes[1].body)
	}
	if !strings.Contains(body, "BLOCKING") {
		t.Fatalf("remote summary = %q, want the successor result", body)
	}
	status, err := f.db.GetPublication(ctx, 42, 7, statusPublishKind, commitStatusPublishKey(f.head, "failed"))
	if err != nil || status.WorkflowID != b.ID {
		t.Fatalf("final status=%+v err=%v", status, err)
	}
	if !strings.Contains(writes[2].target, b.ID) {
		t.Fatalf("B commit status target = %q, want workflow %s", writes[2].target, b.ID)
	}
	statusRow, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if cells := statusTableCells(t, body); cells["代次"] != "`"+itoa(statusRow.Generation)+"`" {
		t.Fatalf("remote generation cell = %q, want %d: %q", cells["代次"], statusRow.Generation, body)
	}
}

// TestWebhookIngressDoesNotWaitForMergeRequestLock pins the ingress side of
// the 900ms processing budget: ingress records the delivery and generation
// bump without taking the per-MR lock in either registry, so a held lock
// cannot stall it (#475).
func TestWebhookIngressDoesNotWaitForMergeRequestLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	webhookDefaults := f.defaults
	webhookDefaults.AllowRepoConfig = false
	webhooks, err := NewWebhookService(WebhookServiceConfig{
		Store: f.db, Workspace: f.manager, Cipher: newTestCipher(t),
		Profiles: f.profiles, ReviewDefaults: webhookDefaults,
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	// Hold both the fixture's private registry and the package default: a
	// regression that re-adds the ingress lock through either registry must
	// block here instead of passing silently.
	publisherUnlock, err := f.publisher.mrLocks.lock(ctx, reviewMRLockKey(42, 7))
	if err != nil {
		t.Fatal(err)
	}
	defer publisherUnlock()
	defaultUnlock, err := (&defaultMRLocks).lock(ctx, reviewMRLockKey(42, 7))
	if err != nil {
		t.Fatal(err)
	}
	defer defaultUnlock()
	done := make(chan struct{})
	var result WebhookResult
	var handleErr error
	go func() {
		result, handleErr = webhooks.Handle(ctx, WebhookRequest{
			Event: "Merge Request Hook", Token: "whsec", UUID: uuid.NewString(),
			Body: mrBody(42, 7, func(body map[string]any) {
				patchAttr(body, "action", "reopen")
				patchAttr(body, "last_commit", map[string]any{"id": f.head})
				patchMR(body, "last_commit", map[string]any{"id": f.head})
			}),
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("ingress waited for a held MR lock instead of completing its DB-only work")
	}
	// Both locks are still held and only the test holds a reference: the
	// ingress neither waited on nor lingered in either registry.
	if refs := mrRegistryRefs(f.publisher.mrLocks, 42, 7); refs != 1 {
		t.Fatalf("publisher registry refs = %d, want only the test holder", refs)
	}
	if refs := mrRegistryRefs(&defaultMRLocks, 42, 7); refs != 1 {
		t.Fatalf("default registry refs = %d, want only the test holder", refs)
	}
	if handleErr != nil || !result.Accepted {
		t.Fatalf("ingress = %+v, %v", result, handleErr)
	}
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 1 {
		t.Fatalf("control = %+v, %v", control, err)
	}
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || status.Generation != control.Generation || status.State != reviewStatusQueued {
		t.Fatalf("queued status = %+v, %v", status, err)
	}
	if notes := f.gitlab.noteSnapshot(); len(notes) != 0 {
		t.Fatalf("ingress wrote to GitLab: %+v", notes)
	}
}

// TestDispatcherStartReviewWaitsForMergeRequestLock pins the manual dispatch
// half of the serialization: the generation bump waits while another note
// writer holds the per-MR lock (#475).
func TestDispatcherStartReviewWaitsForMergeRequestLock(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	dispatcher, err := NewDispatcher(DispatcherConfig{
		Store: f.db, Workspace: f.manager, GitLab: f.gitlab,
		Cipher: f.publisher.cipher, Profiles: f.profiles,
		MRLocks: f.publisher.mrLocks,
	})
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := f.publisher.mrLocks.lock(ctx, reviewMRLockKey(42, 7))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	var jobID string
	var startErr error
	go func() {
		jobID, startErr = dispatcher.StartReview(ctx, 42, 7, "default", true)
		close(done)
	}()
	mrSerialWaitRefs(t, ctx, f.publisher, 2)
	if _, err := f.db.GetReviewControl(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
		unlock()
		t.Fatalf("dispatch bumped the generation while the MR lock was held: %v", err)
	}
	unlock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("dispatch did not finish: %v", ctx.Err())
	}
	if startErr != nil || jobID == "" {
		t.Fatalf("StartReview = %q, %v", jobID, startErr)
	}
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != 1 {
		t.Fatalf("control = %+v, %v", control, err)
	}
	if _, err := f.db.GetJob(ctx, jobID); err != nil {
		t.Fatalf("dispatched job %s: %v", jobID, err)
	}
}

// TestSupersededFallbackNoteIsNotAppended proves the post-failure fallback
// note (rejected token or invalid repository configuration) shares the per-MR
// lock and rechecks the generation: an older generation may not append a note
// after its successor's note (#475).
func TestSupersededFallbackNoteIsNotAppended(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	bound, err := bindOutbound(ctx, f.manager.Get(42))
	if err != nil {
		t.Fatal(err)
	}
	first, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head})
	if err != nil {
		t.Fatal(err)
	}
	f.service.status = testStatusReporter(f.gitlab)
	if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head}); err != nil {
		t.Fatal(err)
	}
	drainReviewJobs(t, f, "review.status")
	before := len(f.gitlab.noteSnapshot())
	if before == 0 {
		t.Fatal("successor wrote no status note")
	}

	f.service.noteTokenRejected(bound, "token", false, 42, 7, first.Generation, f.head)
	f.service.noteInvalidRepoConfig(bound, "token", 42, 7, first.Generation, f.head)

	if notes := f.gitlab.noteSnapshot(); len(notes) != before {
		t.Fatalf("superseded generation appended a note: %d -> %d: %+v", before, len(notes), notes)
	}
}

// TestRepeatedFailurePublishKeepsPublishedState verifies that a stale failure
// publish cannot move an already published failure summary back to another
// state or overwrite the existing remote note.
func TestRepeatedFailurePublishKeepsPublishedState(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	job := f.seedReview(t, "failure-repeat")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("run failed review: %v", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("published failure workflow=%+v err=%v", wf, err)
	}
	before, err := f.db.GetPublication(ctx, 42, 7, summaryPublishKind, summaryPublicationKey)
	if err != nil {
		t.Fatalf("failure summary publication: %v", err)
	}
	beforeNotes := len(f.gitlab.notes)

	mrSerialBumpControl(t, f)()
	err = f.publisher.PublishFailure(ctx, wf.ID, "review execution failed")
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("repeated failure publish error = %v, want ErrStalePublication", err)
	}
	after, err := f.db.GetWorkflow(ctx, wf.ID)
	if err != nil || after.PublicationState != store.WorkflowPublicationPublished || after.PublicationError != "" {
		t.Fatalf("repeated failure publish changed state: %+v err=%v", after, err)
	}
	afterSummary, err := f.db.GetPublication(ctx, 42, 7, summaryPublishKind, summaryPublicationKey)
	if err != nil || afterSummary.WorkflowID != before.WorkflowID || afterSummary.BodyHash != before.BodyHash {
		t.Fatalf("repeated failure publish changed summary: %+v err=%v", afterSummary, err)
	}
	if len(f.gitlab.notes) != beforeNotes {
		t.Fatalf("repeated failure publish changed notes: %d -> %d", beforeNotes, len(f.gitlab.notes))
	}
	for _, state := range []string{"success", "failed"} {
		if _, err := f.db.GetPublication(ctx, 42, 7, statusPublishKind, commitStatusPublishKey(f.head, state)); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("failure publish wrote status %s: %v", state, err)
		}
	}
}
