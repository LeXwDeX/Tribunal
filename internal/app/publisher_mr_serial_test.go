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

// mrLockRefs exposes the test-only view of the existing keyed-lock reference
// count: one holder plus any waiters on the same merge request.
func (p *Publisher) mrLockRefs(projectID, mrIID int64) int {
	key := fmt.Sprintf("%d:%d", projectID, mrIID)
	p.mrLocks.mu.Lock()
	defer p.mrLocks.mu.Unlock()
	entry := p.mrLocks.entries[key]
	if entry == nil {
		return 0
	}
	return entry.refs
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
	if len(f.gitlab.notes) != before || f.gitlab.notes[0].Body != review.SummaryMarkerPrefix+"first" {
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

// TestInFlightSummaryUpdateIsFollowedByNewerGeneration blocks the older
// generation inside the real UpdateMRNote call, after its final pre-write
// recheck. The newer generation must wait on the same MR lock. The in-flight
// old write completes, then the newer generation overwrites it; the old status
// write is rejected as stale.
func TestInFlightSummaryUpdateIsFollowedByNewerGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
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
	if writes := g.snapshot(); len(writes) != 0 {
		close(g.updateRelease)
		t.Fatalf("writes completed while the old summary write was still in flight: %+v", writes)
	}
	close(g.updateRelease)

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
	if len(writes) != 3 {
		t.Fatalf("writes = %+v, want A summary, B summary, B status", writes)
	}
	want := []struct{ actor, kind string }{{"A", "summary-update"}, {"B", "summary-update"}, {"B", "commit-status"}}
	for i := range want {
		if writes[i].actor != want[i].actor || writes[i].kind != want[i].kind {
			t.Fatalf("write[%d] = %+v, want %+v", i, writes[i], want[i])
		}
	}
	if !strings.Contains(writes[1].body, "BLOCKING") || writes[2].state != "failed" {
		t.Fatalf("newer generation did not write the blocking result: %+v", writes[1:])
	}
	b, err := f.db.GetWorkflowByJob(ctx, bRun.ID)
	if err != nil || b.Verdict != review.VerdictBlocking || b.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("B workflow=%+v err=%v", b, err)
	}
	summary, err := f.db.GetPublication(ctx, 42, 7, summaryPublishKind, summaryPublicationKey)
	if err != nil || summary.WorkflowID != b.ID || summary.BodyHash != bodyHash(writes[1].body) {
		t.Fatalf("final summary=%+v err=%v", summary, err)
	}
	status, err := f.db.GetPublication(ctx, 42, 7, statusPublishKind, commitStatusPublishKey(f.head, "failed"))
	if err != nil || status.WorkflowID != b.ID {
		t.Fatalf("final status=%+v err=%v", status, err)
	}
	f.gitlab.mu.Lock()
	body := ""
	for i := range f.gitlab.notes {
		if f.gitlab.notes[i].ID == mustNoteID(t, summary.GitlabID) {
			body = f.gitlab.notes[i].Body
		}
	}
	f.gitlab.mu.Unlock()
	if body != writes[1].body {
		t.Fatalf("remote summary = %q, want the newer generation body", body)
	}
	aAfter, err := f.db.GetWorkflow(ctx, a.ID)
	if err != nil || aAfter.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("A state after stale republish=%+v err=%v", aAfter, err)
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
