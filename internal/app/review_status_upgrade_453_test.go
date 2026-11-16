package app

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// legacyStatusMarkerPrefix is the pre-#452 status note marker. Upgraded rows
// must keep updating that note instead of taking over the summary note.
const legacyStatusMarkerPrefix = "<!-- gitlab-ce-review:status"

// historicalStatusStore presents one pre-upgrade status row while delegating
// every other write to the real store.
type historicalStatusStore struct {
	*sqlite.Store
	row store.ReviewStatus
}

func (s *historicalStatusStore) GetReviewStatus(context.Context, int64, int64) (*store.ReviewStatus, error) {
	row := s.row
	return &row, nil
}

func (s *historicalStatusStore) ListReviewStatusesForVerification(context.Context, int64, int64) ([]store.ReviewStatus, error) {
	return []store.ReviewStatus{s.row}, nil
}

func TestStatusVerificationKeepsPreUpgradeSummaryNote(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	run, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	})
	if err != nil {
		t.Fatal(err)
	}
	summaryBody := review.SummaryMarkerPrefix + " v1 fp=historical -->\n\n## 评审结论\n\nPASS\n\n[P1] 保留的 finding 列表\n"
	legacyBody := legacyStatusMarkerPrefix + " v1 -->\n\n**AI 评审：评审运行中**\n\n- 状态：`running`\n"
	f.gitlab.notes = []gitlab.Note{
		{ID: 700, AuthorID: f.gitlab.currentUser.ID, Body: summaryBody},
		{ID: 701, AuthorID: f.gitlab.currentUser.ID, Body: legacyBody},
	}
	f.service.store = &historicalStatusStore{Store: f.db, row: store.ReviewStatus{
		ProjectID: 42, MRIID: 7, Generation: run.Generation, HeadSHA: f.head,
		Attempt: 1, Profile: "default", State: reviewStatusPass,
		Version: 3, AppliedVersion: 3, UpdatedAt: time.Now().UTC(),
	}}

	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatalf("ReconcileSideEffects: %v", err)
	}
	drainReviewJobs(t, f, "review.status")

	if len(f.gitlab.notes) != 2 {
		t.Fatalf("notes = %d, want the existing summary and legacy status notes", len(f.gitlab.notes))
	}
	if got := f.gitlab.notes[0].Body; got != summaryBody {
		t.Fatalf("status worker overwrote the published summary: %q", got)
	}
	if got := f.gitlab.notes[1]; !strings.HasPrefix(got.Body, legacyStatusMarkerPrefix) ||
		!strings.Contains(got.Body, reviewStatusPass) {
		t.Fatalf("legacy status note was not converged in place: %+v", got)
	}
}

// TestPublishedSummaryDeletesLegacyStatusNote proves the convergence target:
// once the merged row carries a final body, the status writer removes the
// bot's legacy status note, leaving exactly one review note (#475).
func TestPublishedSummaryDeletesLegacyStatusNote(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	run, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	})
	if err != nil {
		t.Fatal(err)
	}
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil || control.Generation != run.Generation {
		t.Fatalf("control = %+v, %v", control, err)
	}
	body := review.SummaryMarkerPrefix + " v1 fp=published -->\n\n## 评审结论\n\nPASS\n"
	if _, err := f.db.SetReviewStatusFinalBody(ctx, 42, 7, control.Generation, f.head, body); err != nil {
		t.Fatal(err)
	}
	f.gitlab.mu.Lock()
	f.gitlab.notes = append(f.gitlab.notes, gitlab.Note{
		ID: 701, AuthorID: f.gitlab.currentUser.ID,
		Body: legacyStatusMarkerPrefix + " v1 -->\n\n**AI 评审：评审运行中**\n",
	})
	f.gitlab.mu.Unlock()

	drainReviewJobs(t, f, "review.status")

	notes := f.gitlab.noteSnapshot()
	if len(notes) != 1 {
		t.Fatalf("notes = %+v, want the merged review note only", notes)
	}
	if strings.HasPrefix(notes[0].Body, legacyStatusMarkerPrefix) {
		t.Fatalf("legacy status note survived: %q", notes[0].Body)
	}
	if !strings.HasPrefix(notes[0].Body, reviewStatusMarkerPrefix) || !strings.Contains(notes[0].Body, "## 评审结论") {
		t.Fatalf("merged note = %q", notes[0].Body)
	}
}

// activatingControlStore activates a successor review exactly when the
// publisher reads the control just before storing the final body.
type activatingControlStore struct {
	ReviewStore
	db   *sqlite.Store
	head string
	once sync.Once
	err  error
}

func (s *activatingControlStore) GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error) {
	s.once.Do(func() {
		_, s.err = s.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
			ProjectID: projectID, MRIID: mrIID, HeadSHA: s.head,
		})
	})
	if s.err != nil {
		return nil, s.err
	}
	return s.ReviewStore.GetReviewControl(ctx, projectID, mrIID)
}

func TestPublisherRejectsSummaryFromSupersededGeneration(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	current, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	})
	if err != nil {
		t.Fatal(err)
	}
	nextHead := strings.Repeat("b", 40)
	f.publisher.store = &activatingControlStore{ReviewStore: f.db, db: f.db, head: nextHead}
	body := review.SummaryMarkerPrefix + " v1 fp=old -->\n\n## 过期评审结论\n\n[P1] 旧 finding\n"

	// The successor activates between the caller's recheck and this write.
	_, err = f.publisher.upsertSummary(ctx, 42, 7, "token", "old-fp", body, "",
		current.Generation, f.head, func() error { return nil })
	if !IsPermanentJob(err) || !errors.Is(err, ErrStalePublication) {
		t.Fatalf("upsertSummary error = %v, want a permanent stale publication", err)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if row.Generation != current.Generation+1 {
		t.Fatalf("successor generation = %d, want %d", row.Generation, current.Generation+1)
	}
	if row.FinalBody != "" {
		t.Fatalf("old summary reached the successor row: %+v", row)
	}
	if f.gitlab.summaryCreates != 0 {
		t.Fatalf("old summary was posted %d times", f.gitlab.summaryCreates)
	}
}

func newMergeWebhook(t *testing.T, f *reviewFixture) *WebhookService {
	t.Helper()
	service, err := NewWebhookService(WebhookServiceConfig{
		Store:          f.db,
		Workspace:      f.manager,
		Cipher:         newTestCipher(t),
		Profiles:       f.profiles,
		ReviewDefaults: f.defaults,
	})
	if err != nil {
		t.Fatalf("NewWebhookService: %v", err)
	}
	return service
}

func sendMergeAction(t *testing.T, service *WebhookService, action string) WebhookResult {
	t.Helper()
	body := mrBody(42, 7, func(b map[string]any) { patchAttr(b, "action", action) })
	result, err := service.Handle(context.Background(), WebhookRequest{
		Event: "Merge Request Hook", Token: "whsec", UUID: uuid.NewString(), Body: body,
	})
	if err != nil {
		t.Fatalf("Handle %s: %v", action, err)
	}
	return result
}

// publishTerminalReview stores a completed review and drains its status note,
// leaving the fixture in the shape of an MR whose result is already published.
func publishTerminalReview(t *testing.T, f *reviewFixture) string {
	t.Helper()
	ctx := context.Background()
	job := f.seedReview(t, "job-merge-published")
	claimed, err := f.db.Claim(ctx, "worker", []string{"review.run"}, time.Minute)
	if err != nil || claimed == nil || claimed.ID != job.ID {
		t.Fatalf("claim review.run = %+v, %v", claimed, err)
	}
	if err := f.db.Complete(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: control.Generation, HeadSHA: f.head,
		State: reviewStatusPass,
	}); err != nil {
		t.Fatal(err)
	}
	body := review.SummaryMarkerPrefix + " v1 fp=published -->\n\n## 评审结论\n\nPASS\n\n[P1] 已发布的 finding 列表\n"
	if _, err := f.db.SetReviewStatusFinalBody(ctx, 42, 7, control.Generation, f.head, body); err != nil {
		t.Fatal(err)
	}
	drainReviewSideEffects(t, f)
	return summaryNoteBody(t, f)
}

func TestMergeRequestNeverReviewedCreatesNothing(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	service := newMergeWebhook(t, f)

	for _, action := range []string{"close", "merge"} {
		result := sendMergeAction(t, service, action)
		if result.Ignored != "nothing_to_cancel" {
			t.Fatalf("%s result = %+v, want nothing_to_cancel", action, result)
		}
	}
	if _, err := f.db.GetReviewControl(context.Background(), 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("close/merge created a review control: %v", err)
	}
	if _, err := f.db.GetReviewStatus(context.Background(), 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("close/merge created a review status: %v", err)
	}
	job, err := f.db.Claim(context.Background(), "worker", []string{"review.cancel"}, time.Minute)
	if err != nil || job != nil {
		t.Fatalf("cancel job = %+v, %v; want none", job, err)
	}
	if len(f.gitlab.notes) != 0 {
		t.Fatalf("close/merge posted comments: %+v", f.gitlab.notes)
	}
}

func summaryNoteBody(t *testing.T, f *reviewFixture) string {
	t.Helper()
	for _, note := range f.gitlab.notes {
		if strings.HasPrefix(note.Body, review.SummaryMarkerPrefix) {
			return note.Body
		}
	}
	t.Fatal("published review note is missing")
	return ""
}

// markStatusRowHistorical rewrites the current row into the shape of a row
// migrated from before #452: it owns no merged note body, so the status writer
// must keep updating the legacy status-marker note (#453).
func markStatusRowHistorical(t *testing.T, f *reviewFixture) {
	t.Helper()
	raw, err := sql.Open("sqlite", f.dbPath)
	if err != nil {
		t.Fatalf("open raw store: %v", err)
	}
	defer raw.Close()
	if _, err := raw.ExecContext(context.Background(),
		`UPDATE review_status SET final_body_ready = 0, final_body = '' WHERE project_id = 42 AND mr_iid = 7`); err != nil {
		t.Fatalf("mark review status historical: %v", err)
	}
}

// TestTerminalStatusVisitKeepsPublishedSummaryNote covers the terminal
// fallbacks for rows migrated from before #452. Their published summary lives
// in a separate note, so a terminal visit must update the legacy status note
// and leave that summary byte-for-byte unchanged (#475).
func TestTerminalStatusVisitKeepsPublishedSummaryNote(t *testing.T) {
	summaryBody := review.SummaryMarkerPrefix + " v1 fp=historical -->\n\n## 评审结论\n\nPASS\n\n[P1] 保留的 finding 列表\n"
	legacyBody := legacyStatusMarkerPrefix + " v1 -->\n\n**AI 评审：评审运行中**\n\n- 状态：`running`\n"

	cases := []struct {
		name   string
		cancel bool
		settle func(t *testing.T, f *reviewFixture, generation int64)
		detail string
	}{
		{
			name: "missing control",
			settle: func(t *testing.T, f *reviewFixture, _ int64) {
				f.service.store = &overrideStatusStore{ReviewStore: f.db, controlErr: store.ErrNotFound}
			},
			detail: "review control is missing",
		},
		{
			name: "head mismatch",
			settle: func(t *testing.T, f *reviewFixture, generation int64) {
				f.service.store = &overrideStatusStore{ReviewStore: f.db, control: &store.ReviewControl{
					ProjectID: 42, MRIID: 7, Generation: generation, HeadSHA: strings.Repeat("c", 40),
				}}
			},
			detail: "review superseded",
		},
		{
			name:   "cancelled",
			cancel: true,
			settle: func(*testing.T, *reviewFixture, int64) {},
			detail: "review cancelled",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.service.status = testStatusReporter(f.gitlab)
			result, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: f.head})
			if err != nil {
				t.Fatal(err)
			}
			generation := result.Generation
			if tc.cancel {
				if _, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, Cancel: true}); err != nil {
					t.Fatal(err)
				}
				control, err := f.db.GetReviewControl(ctx, 42, 7)
				if err != nil || !control.Cancelled {
					t.Fatalf("control = %+v, %v; want a cancelled generation", control, err)
				}
				generation = control.Generation
			}
			markStatusRowHistorical(t, f)
			f.gitlab.mu.Lock()
			f.gitlab.notes = []gitlab.Note{
				{ID: 700, AuthorID: f.gitlab.currentUser.ID, Body: summaryBody},
				{ID: 701, AuthorID: f.gitlab.currentUser.ID, Body: legacyBody},
			}
			f.gitlab.mu.Unlock()
			tc.settle(t, f, generation)
			drainReviewJobs(t, f, "review.status")

			notes := f.gitlab.noteSnapshot()
			if len(notes) != 2 {
				t.Fatalf("notes = %d, want the summary and one legacy status note: %+v", len(notes), notes)
			}
			summary, legacy := noteByID(notes, 700), noteByID(notes, 701)
			if summary == nil || summary.Body != summaryBody {
				t.Fatalf("published summary changed: %+v", summary)
			}
			if legacy == nil || !strings.HasPrefix(legacy.Body, legacyStatusMarkerPrefix) {
				t.Fatalf("legacy status note = %+v", legacy)
			}
			if phase := parseStatusPhase(legacy.Body); phase != reviewStatusPhase(reviewStatusStale) {
				t.Fatalf("legacy status note did not reach a terminal phase: %q", legacy.Body)
			}
			if !strings.Contains(legacy.Body, tc.detail) {
				t.Fatalf("legacy status note = %q, want detail %q", legacy.Body, tc.detail)
			}
			row, err := f.db.GetReviewStatus(ctx, 42, 7)
			if err != nil || row.AppliedVersion < row.Version {
				t.Fatalf("terminal visit was not recorded: %+v, %v", row, err)
			}
		})
	}
}

func noteByID(notes []gitlab.Note, id int64) *gitlab.Note {
	for i := range notes {
		if notes[i].ID == id {
			return &notes[i]
		}
	}
	return nil
}

func TestMergeRequestPreservesPublishedFindings(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	publishTerminalReview(t, f)
	before, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	service := newMergeWebhook(t, f)

	result := sendMergeAction(t, service, "merge")
	if !result.Accepted {
		t.Fatalf("merge result = %+v, want an accepted cancel", result)
	}
	job, err := f.db.Claim(context.Background(), "worker", []string{"review.cancel"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("cancel job = %+v, %v; want one", job, err)
	}
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("cancel handle: %v", err)
	}
	drainReviewSideEffects(t, f)

	after, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation <= before.Generation || after.State != reviewStatusCancelled {
		t.Fatalf("merge did not settle a cancelled successor: before %+v after %+v", before, after)
	}
	if after.FinalBody != before.FinalBody || !after.FinalBodyReady {
		t.Fatalf("merge cleared the published body: before %+v after %+v", before, after)
	}
	control, err := f.db.GetReviewControl(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !control.Cancelled || control.Generation != after.Generation {
		t.Fatalf("control = %+v", control)
	}
	body := summaryNoteBody(t, f)
	if !strings.Contains(body, "[P1] 已发布的 finding 列表") || !strings.Contains(body, "## 评审结论") {
		t.Fatalf("merge removed the published conclusions: %q", body)
	}
	if parseStatusPhase(body) != reviewStatusPhase(reviewStatusCancelled) || !strings.Contains(body, "评审已取消") {
		t.Fatalf("merge did not update the lifecycle header: %q", body)
	}
}

func TestCommandCancelKeepsPublishedFindings(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	publishTerminalReview(t, f)
	before, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	cancelJob := cancelJobWithNote(t, f, "job-cancel-published", 777)
	if err := f.service.Handle(context.Background(), cancelJob); err != nil {
		t.Fatalf("cancel handle: %v", err)
	}
	drainReviewSideEffects(t, f)

	after, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation <= before.Generation || after.State != reviewStatusCancelled {
		t.Fatalf("cancel did not advance the status: before %+v after %+v", before, after)
	}
	if after.FinalBody != before.FinalBody || !after.FinalBodyReady {
		t.Fatalf("cancel cleared the published body: before %+v after %+v", before, after)
	}
	body := summaryNoteBody(t, f)
	if !strings.Contains(body, "[P1] 已发布的 finding 列表") || !strings.Contains(body, "## 评审结论") {
		t.Fatalf("cancel removed the published conclusions: %q", body)
	}
	if parseStatusPhase(body) != reviewStatusPhase(reviewStatusCancelled) {
		t.Fatalf("cancel did not update the lifecycle header: %q", body)
	}
}

func TestNewReviewAfterCancelResetsPublishedFindings(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	publishTerminalReview(t, f)
	cancelJob := cancelJobWithNote(t, f, "job-cancel-before-new-review", 777)
	if err := f.service.Handle(context.Background(), cancelJob); err != nil {
		t.Fatalf("cancel handle: %v", err)
	}
	drainReviewSideEffects(t, f)
	if body := summaryNoteBody(t, f); !strings.Contains(body, "[P1] 已发布的 finding 列表") {
		t.Fatalf("cancel dropped the published findings: %q", body)
	}

	nextHead := strings.Repeat("c", 40)
	next, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: nextHead,
	})
	if err != nil {
		t.Fatal(err)
	}
	reset, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if reset.Generation != next.Generation || reset.FinalBody != "" {
		t.Fatalf("new review kept the previous body: %+v", reset)
	}
	drainReviewJobs(t, f, "review.status")
	if body := summaryNoteBody(t, f); strings.Contains(body, "[P1] 已发布的 finding 列表") {
		t.Fatalf("new review reused the previous findings: %q", body)
	}
	if body := summaryNoteBody(t, f); parseStatusPhase(body) != reviewStatusPhase(reviewStatusQueued) {
		t.Fatalf("new review did not reset the note: %q", body)
	}
}

func TestManualCancelAPIPreservesPublishedFindings(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	workflowID := seedReviewWorkflow(t, f.db)
	control, err := f.db.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: control.Generation, HeadSHA: sha40,
		State: reviewStatusBlocking,
	}); err != nil {
		t.Fatal(err)
	}
	body := review.SummaryMarkerPrefix + " v1 fp=manual -->\n\n## 评审结论\n\nBLOCKING\n\n[P1] 手工取消前的 finding\n"
	if _, err := f.db.SetReviewStatusFinalBody(ctx, 42, 7, control.Generation, sha40, body); err != nil {
		t.Fatal(err)
	}
	drainReviewSideEffects(t, f)
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}

	dispatcher := newTestDispatcher(t, f.db, f.gitlab)
	jobID, err := dispatcher.CancelReview(ctx, workflowID)
	if err != nil {
		t.Fatalf("CancelReview: %v", err)
	}
	job, err := f.db.Claim(ctx, "worker", []string{"review.cancel"}, time.Minute)
	if err != nil || job == nil || job.ID != jobID {
		t.Fatalf("claim cancel = %+v, %v", job, err)
	}
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("cancel handle: %v", err)
	}
	drainReviewSideEffects(t, f)

	after, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if after.Generation <= before.Generation || after.State != reviewStatusCancelled {
		t.Fatalf("manual cancel did not advance the status: before %+v after %+v", before, after)
	}
	if after.FinalBody != before.FinalBody || !after.FinalBodyReady {
		t.Fatalf("manual cancel cleared the published body: before %+v after %+v", before, after)
	}
	note := summaryNoteBody(t, f)
	if !strings.Contains(note, "[P1] 手工取消前的 finding") || !strings.Contains(note, "## 评审结论") {
		t.Fatalf("manual cancel removed the published conclusions: %q", note)
	}
	if parseStatusPhase(note) != reviewStatusPhase(reviewStatusCancelled) {
		t.Fatalf("manual cancel did not update the lifecycle header: %q", note)
	}
}

func TestMergeRequestCancelsActiveReview(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.seedReview(t, "job-merge-active")
	service := newMergeWebhook(t, f)

	result := sendMergeAction(t, service, "close")
	if !result.Accepted {
		t.Fatalf("close result = %+v, want an accepted cancel", result)
	}
	control, err := f.db.GetReviewControl(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !control.Cancelled {
		t.Fatalf("active review was not cancelled: %+v", control)
	}
	job, err := f.db.Claim(context.Background(), "worker", []string{"review.cancel"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("cancel job = %+v, %v; want one", job, err)
	}
}
