package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

func testStatusReporter(api *reviewGitLab) *statusReporter {
	return newStatusReporter(api, nil)
}

func statusPhases(updates []string) []string {
	phases := make([]string, 0, len(updates))
	for _, body := range updates {
		phases = append(phases, parseStatusPhase(body))
	}
	return phases
}

func (f *reviewGitLab) awardSnapshot(noteID int64) []gitlab.Award {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitlab.Award(nil), f.awards[noteID]...)
}

// statusTableCells parses the first data row of the status table into a
// column-name map. It fails when the header or data row is missing.
func statusTableCells(t *testing.T, body string) map[string]string {
	t.Helper()
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != "| 阶段 | 状态 | 代次 | 提交 | 预计 | 详情 |" {
			continue
		}
		if i+2 >= len(lines) {
			t.Fatalf("status table has no data row: %q", body)
		}
		header := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		// An escaped pipe belongs to the detail cell, not a column boundary.
		row := strings.Split(strings.Trim(strings.TrimSpace(strings.ReplaceAll(lines[i+2], `\|`, "\x00")), "|"), "|")
		if len(header) != len(row) {
			t.Fatalf("status table header %v does not match row %v", header, row)
		}
		cells := make(map[string]string, len(header))
		for j := range header {
			cells[strings.TrimSpace(header[j])] = strings.ReplaceAll(strings.TrimSpace(row[j]), "\x00", `\|`)
		}
		return cells
	}
	t.Fatalf("status table missing: %q", body)
	return nil
}

// TestReviewStatusBodyRendersMarkdownTable pins the table layout and per-cell
// escaping of the single status note body (#475).
func TestReviewStatusBodyRendersMarkdownTable(t *testing.T) {
	body := reviewStatusBody(reviewStatusUpdate{
		State: reviewStatusRunning, HeadSHA: reviewTestHead, Generation: 7,
		EstimateMinutes: 3, Detail: "node a|b failed",
	})
	if !strings.HasPrefix(body, reviewStatusMarkerPrefix+" v2 -->") {
		t.Fatalf("marker = %q, want v2", strings.SplitN(body, "\n", 2)[0])
	}
	if !strings.Contains(body, "| 阶段 | 状态 | 代次 | 提交 | 预计 | 详情 |") ||
		!strings.Contains(body, "| --- | --- | --- | --- | --- | --- |") {
		t.Fatalf("status table header or divider missing: %q", body)
	}
	cells := statusTableCells(t, body)
	if cells["阶段"] != "进行中" || cells["状态"] != "`running`" || cells["代次"] != "`7`" ||
		cells["提交"] != "`"+shortSHA(reviewTestHead)+"`" || cells["预计"] != "约 3 分钟" {
		t.Fatalf("status table cells = %v", cells)
	}
	if !strings.Contains(body, `node a\|b failed`) {
		t.Fatalf("detail pipe was not escaped: %q", body)
	}

	empty := reviewStatusBody(reviewStatusUpdate{State: reviewStatusQueued})
	cells = statusTableCells(t, empty)
	if cells["代次"] != "-" || cells["提交"] != "-" || cells["预计"] != "-" || cells["详情"] != "-" {
		t.Fatalf("empty cells = %v", cells)
	}
}

// TestParseStatusPhaseReadsLegacyBulletBody keeps upgrade compatibility: a
// pre-table body is still parsed for the monotonic queued ack check.
func TestParseStatusPhaseReadsLegacyBulletBody(t *testing.T) {
	legacy := "<!-- gitlab-ce-review:summary v1 -->\n\n**AI 评审：评审运行中**\n\n- 阶段：`进行中`\n- 状态：`running`\n"
	if got := parseStatusPhase(legacy); got != "进行中" {
		t.Fatalf("legacy phase = %q, want 进行中", got)
	}
	table := reviewStatusBody(reviewStatusUpdate{State: reviewStatusPass})
	if got := parseStatusPhase(table); got != "已完成" {
		t.Fatalf("table phase = %q, want 已完成", got)
	}
}

// drainReviewSideEffects runs every queued status/reaction job until none is
// left. A retryable failure is failed so its bounded retry budget still
// applies; the caller decides when a failure is expected.
func drainReviewJobs(t *testing.T, f *reviewFixture, kinds ...string) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < 100; i++ {
		job, err := f.db.Claim(ctx, "side-effects-test", kinds, time.Minute)
		if err != nil {
			t.Fatalf("claim side effect: %v", err)
		}
		if job == nil {
			return
		}
		if err := f.service.Handle(ctx, job); err != nil {
			if failErr := f.db.Fail(ctx, *job, err, nil); failErr != nil {
				t.Fatalf("fail side effect: %v", failErr)
			}
			continue
		}
		if err := f.db.Complete(ctx, *job); err != nil {
			t.Fatalf("complete side effect: %v", err)
		}
	}
	t.Fatal("review side effects did not drain")
}

func drainReviewSideEffects(t *testing.T, f *reviewFixture) {
	t.Helper()
	drainReviewJobs(t, f, "review.status", "review.reaction")
}

func TestStatusWriterUpsertsSingleNote(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	reporter := testStatusReporter(api)

	noteID, hash, _, err := reporter.writeStatus(context.Background(), 42, 7, "token", reviewStatusUpdate{
		State: reviewStatusRunning, HeadSHA: reviewTestHead, Generation: 3, EstimateMinutes: 5,
	})
	if err != nil || noteID <= 0 || hash == "" {
		t.Fatalf("first writeStatus = %d, %q, %v", noteID, hash, err)
	}
	sameID, sameHash, _, err := reporter.writeStatus(context.Background(), 42, 7, "token", reviewStatusUpdate{
		State: reviewStatusRunning, HeadSHA: reviewTestHead, Generation: 3, EstimateMinutes: 5,
	})
	if err != nil || sameID != noteID || sameHash != hash {
		t.Fatalf("idempotent writeStatus = %d, %q, %v", sameID, sameHash, err)
	}
	if got := api.noteCount(); got != 1 {
		t.Fatalf("status notes = %d, want 1", got)
	}
	if len(api.statusUpdates) != 1 {
		t.Fatalf("status writes = %d, want one identical write skipped", len(api.statusUpdates))
	}
	if !strings.HasPrefix(api.notes[0].Body, reviewStatusMarkerLine) {
		t.Fatalf("status marker missing: %q", api.notes[0].Body)
	}
	cells := statusTableCells(t, api.notes[0].Body)
	if cells["阶段"] != reviewStatusPhase(reviewStatusRunning) || cells["状态"] != "`running`" ||
		cells["代次"] != "`3`" || cells["提交"] != "`"+shortSHA(reviewTestHead)+"`" ||
		cells["预计"] != "约 5 分钟" {
		t.Fatalf("status table cells = %v, body: %q", cells, api.notes[0].Body)
	}
	if !strings.Contains(api.notes[0].Body, "| 阶段 | 状态 | 代次 | 提交 | 预计 | 详情 |") {
		t.Fatalf("status table header missing: %q", api.notes[0].Body)
	}

	if _, _, _, err := reporter.writeStatus(context.Background(), 42, 7, "token", reviewStatusUpdate{
		State: reviewStatusPublishing, HeadSHA: reviewTestHead, Generation: 3,
	}); err != nil {
		t.Fatalf("publishing writeStatus: %v", err)
	}
	if got := api.noteCount(); got != 1 {
		t.Fatalf("status notes after update = %d, want 1", got)
	}
	if len(api.statusUpdates) != 2 || !strings.Contains(api.notes[0].Body, reviewStatusPublishing) {
		t.Fatalf("status update = %q, writes = %d", api.notes[0].Body, len(api.statusUpdates))
	}
}

func TestStatusWriterIgnoresForeignNoteAndRecreatesAfterDelete(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	api.notes = []gitlab.Note{{
		ID: 1, AuthorID: 3, Body: reviewStatusMarkerLine + "\nforeign status",
	}}
	reporter := testStatusReporter(api)
	noteID, _, _, err := reporter.writeStatus(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusRunning})
	if err != nil || noteID == 1 {
		t.Fatalf("writeStatus = %d, %v; want a new bot-owned note", noteID, err)
	}
	if got := api.noteCount(); got != 2 {
		t.Fatalf("notes = %d, want a new bot-owned status note", got)
	}
	if api.summaryUpdates != 0 {
		t.Fatalf("foreign status note was updated %d times", api.summaryUpdates)
	}
	// A user deleting the bot's note is repaired by the next retry.
	api.mu.Lock()
	var kept []gitlab.Note
	for _, note := range api.notes {
		if note.ID != noteID {
			kept = append(kept, note)
		}
	}
	api.notes = kept
	api.mu.Unlock()
	recreated, _, _, err := reporter.writeStatus(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusPublishing})
	if err != nil || recreated == 0 {
		t.Fatalf("recreate writeStatus = %d, %v", recreated, err)
	}

	api.statusNoteErr = errors.New("status unavailable")
	if _, _, _, err := reporter.writeStatus(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusFailed}); err == nil {
		t.Fatal("a status GitLab failure was swallowed")
	}
}

func TestStatusWriterDeletesLegacyStatusNoteAfterMerge(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	botID := api.currentUser.ID
	api.notes = []gitlab.Note{
		{ID: 10, AuthorID: botID, Body: reviewLegacyStatusMarkerLine + "\nold status"},
		{ID: 11, AuthorID: botID, Body: reviewStatusMarkerLine + "\nold merged"},
		{ID: 12, AuthorID: botID + 1, Body: reviewLegacyStatusMarkerLine + "\nforeign"},
	}
	reporter := testStatusReporter(api)
	final := review.SummaryMarkerPrefix + " v1 fp=x -->\n\n## 评审结论\n\nPASS\n"
	update := reviewStatusUpdate{
		State: reviewStatusPass, HeadSHA: reviewTestHead, Generation: 4,
		FinalBody: final, CleanupLegacy: true,
	}
	if _, _, legacy, err := reporter.writeStatus(context.Background(), 42, 7, "token", update); err != nil {
		t.Fatalf("writeStatus: %v", err)
	} else if len(legacy) != 1 || legacy[0] != 10 {
		t.Fatalf("legacy notes = %v, want the bot's pre-upgrade note only", legacy)
	}
	// The caller deletes the reported notes after recording the merged note id.
	if err := reporter.deleteLegacyStatusNotes(context.Background(), 42, 7, "token", []int64{10}); err != nil {
		t.Fatalf("deleteLegacyStatusNotes: %v", err)
	}
	notes := api.noteSnapshot()
	if len(notes) != 2 {
		t.Fatalf("notes = %+v, want the merged note and the foreign legacy note", notes)
	}
	if api.noteDeletes != 1 {
		t.Fatalf("note deletes = %d, want one legacy delete", api.noteDeletes)
	}
	for _, note := range notes {
		if note.ID == 10 {
			t.Fatalf("bot legacy note survived convergence: %+v", notes)
		}
	}
	if !strings.HasPrefix(notes[0].Body, reviewStatusMarkerLine) || !strings.Contains(notes[0].Body, "## 评审结论") {
		t.Fatalf("merged note was not converged: %q", notes[0].Body)
	}
	// A repeated identical write is still a no-op and reports nothing to delete.
	if _, _, legacy, err := reporter.writeStatus(context.Background(), 42, 7, "token", update); err != nil {
		t.Fatalf("idempotent writeStatus: %v", err)
	} else if len(legacy) != 0 {
		t.Fatalf("idempotent write reported legacy notes: %v", legacy)
	}
	if api.noteDeletes != 1 {
		t.Fatalf("idempotent write deleted notes again: %d", api.noteDeletes)
	}
}

func (f *reviewGitLab) noteSnapshot() []gitlab.Note {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]gitlab.Note(nil), f.notes...)
}

func TestReactionWriterConvergesOwnAwards(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	api.awards[555] = []gitlab.Award{
		{ID: 1, Name: reviewAwardEyes, UserID: 9, Username: "review-bot"},
		{ID: 2, Name: reviewAwardEyes, UserID: 3, Username: "dev"},
	}
	reporter := testStatusReporter(api)
	if err := reporter.applyReaction(context.Background(), 42, 7, "token", 555, reviewAwardPass); err != nil {
		t.Fatalf("applyReaction pass: %v", err)
	}
	if err := reporter.applyReaction(context.Background(), 42, 7, "token", 555, reviewAwardPass); err != nil {
		t.Fatalf("idempotent applyReaction pass: %v", err)
	}
	awards := api.awardSnapshot(555)
	var botEyes, otherEyes, passes int
	for _, award := range awards {
		switch {
		case award.Name == reviewAwardEyes && award.UserID == 9:
			botEyes++
		case award.Name == reviewAwardEyes && award.UserID == 3:
			otherEyes++
		case award.Name == reviewAwardPass && award.UserID == 9:
			passes++
		}
	}
	if botEyes != 0 || otherEyes != 1 || passes != 1 {
		t.Fatalf("awards = %+v, want no bot eyes, one foreign eye, one pass", awards)
	}
	if err := reporter.applyReaction(context.Background(), 42, 7, "token", 555, reviewAwardNone); err != nil {
		t.Fatalf("applyReaction none: %v", err)
	}
	awards = api.awardSnapshot(555)
	if len(awards) != 1 || awards[0].UserID != 3 {
		t.Fatalf("awards after none = %+v, want only the foreign eye", awards)
	}
	if got := api.noteCount(); got != 0 {
		t.Fatalf("notes = %d, reactions must not write a status note", got)
	}
}

func TestReactionWriterClassifiesNoteGoneAndUpstreamFailure(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	api.awardErr = &gitlab.Error{Op: "list_awards", Status: 404}
	reporter := testStatusReporter(api)
	if err := reporter.applyReaction(context.Background(), 42, 7, "token", 555, reviewAwardEyes); !errors.Is(err, errReactionNoteGone) {
		t.Fatalf("missing note error = %v, want errReactionNoteGone", err)
	}
	api.awardErr = errors.New("award lookup failed")
	if err := reporter.applyReaction(context.Background(), 42, 7, "token", 555, reviewAwardFail); err == nil {
		t.Fatal("an upstream award failure was swallowed")
	}
	api.awardErr = nil
	api.addAwardErr = &gitlab.Error{Op: "add_award", Status: 500}
	if err := reporter.applyReaction(context.Background(), 42, 7, "token", 555, reviewAwardFail); err == nil {
		t.Fatal("an upstream award creation failure was swallowed")
	}
}

func TestSanitizeStatusDetail(t *testing.T) {
	raw := "node failed:\n\tglpat-secret-token https://gitlab.example.com/private " + strings.Repeat("x", 300)
	got := sanitizeStatusDetail(raw)
	if strings.Contains(got, "glpat-secret-token") || strings.Contains(got, "https://") {
		t.Fatalf("detail leaked sensitive text: %q", got)
	}
	if strings.ContainsAny(got, "\n\t") {
		t.Fatalf("detail contains control characters: %q", got)
	}
	if len([]rune(got)) > reviewStatusDetailRunes {
		t.Fatalf("detail is not bounded: %d runes", len([]rune(got)))
	}
	if !strings.Contains(got, "[redacted]") || !strings.Contains(got, "[url]") {
		t.Fatalf("detail was not redacted: %q", got)
	}
}

func TestReviewStatusLifecycleAndCommandReaction(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := f.seedReview(t, "job-status-lifecycle")
	var payload map[string]any
	if err := json.Unmarshal(job.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	payload["note_id"] = float64(555)
	job.Payload = mustJSONBody(t, payload)

	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if status.State != reviewStatusPass {
		t.Fatalf("durable status = %+v, want PASS", status)
	}
	reaction, err := f.db.GetReviewReaction(context.Background(), 42, 7, 555)
	if err != nil || reaction.Desired != reviewAwardPass {
		t.Fatalf("durable reaction = %+v, %v", reaction, err)
	}
	drainReviewSideEffects(t, f)

	if got := f.gitlab.noteCount(); got != 1 {
		t.Fatalf("notes = %d, want the single review note", got)
	}
	phases := statusPhases(f.gitlab.statusUpdates)
	if len(phases) == 0 || phases[len(phases)-1] != reviewStatusPhase(reviewStatusPass) {
		t.Fatalf("status phases = %v, want the final PASS phase", phases)
	}
	awards := f.gitlab.awardSnapshot(555)
	if len(awards) != 1 || awards[0].Name != reviewAwardPass || awards[0].UserID != f.gitlab.currentUser.ID {
		t.Fatalf("command reaction = %+v", awards)
	}
}

func TestReviewStatusFailureDoesNotBlockReview(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := f.seedReview(t, "job-status-retryable")
	beforeRuns := f.runner.count()

	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("status failure changed the review outcome: %v", err)
	}
	// The review note is shared with the status worker (#451); fail only the
	// later status edit so the review itself has already succeeded.
	f.gitlab.statusNoteErr = errors.New("status API unavailable")
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if workflow.Verdict == "" {
		t.Fatal("review verdict was not published")
	}
	if got := f.runner.count(); got != beforeRuns+1 {
		t.Fatalf("model runs = %d, want exactly one review run", got)
	}
	status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if status.AppliedVersion != 0 {
		t.Fatalf("failed status write was marked applied: %+v", status)
	}
	claimed, err := f.db.Claim(context.Background(), "status-worker", []string{"review.status"}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim status job = %+v, %v", claimed, err)
	}
	if err := f.service.Handle(context.Background(), claimed); err == nil {
		t.Fatal("a transient status failure must return an error for a bounded retry")
	}
	if got := f.runner.count(); got != beforeRuns+1 {
		t.Fatalf("status retry re-ran the model: %d", got)
	}
}

func TestReviewStatusRejectsSupersededGeneration(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx := context.Background()
	first, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"command"}`), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
		Payload: json.RawMessage(`{"trigger":"mr_update"}`), MaxAttempts: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.service.setReviewStatus(ctx, 42, 7, first.Generation, 1, f.head, "", reviewStatusRunning, "old generation")
	f.service.setReviewReaction(ctx, 42, 7, 555, first.Generation, reviewAwardPass)
	status, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if status.Generation != second.Generation || status.State != reviewStatusQueued {
		t.Fatalf("a superseded worker wrote the status: %+v", status)
	}
	if _, err := f.db.GetReviewReaction(ctx, 42, 7, 555); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("a superseded worker created a reaction: %v", err)
	}
}

func TestReviewStatusForVerdict(t *testing.T) {
	cases := []struct {
		verdict  string
		state    string
		reaction string
	}{
		{review.VerdictPass, reviewStatusPass, reviewAwardPass},
		{review.VerdictBlocking, reviewStatusBlocking, reviewAwardFail},
		{review.VerdictIncomplete, reviewStatusIncomplete, reviewAwardFail},
		{"", reviewStatusIncomplete, reviewAwardFail},
	}
	for _, tc := range cases {
		state, reaction := reviewStatusForVerdict(tc.verdict)
		if state != tc.state || reaction != tc.reaction {
			t.Errorf("reviewStatusForVerdict(%q) = %q, %q; want %q, %q",
				tc.verdict, state, reaction, tc.state, tc.reaction)
		}
	}
}

func TestReviewStatusAutomaticHookHasNoReaction(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	job := f.seedReview(t, "job-status-automatic")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	drainReviewSideEffects(t, f)
	if len(f.gitlab.awards) != 0 {
		t.Fatalf("automatic review fabricated reactions: %+v", f.gitlab.awards)
	}
	statusNotes := 0
	for _, note := range f.gitlab.notes {
		if strings.HasPrefix(note.Body, reviewStatusMarkerPrefix) {
			statusNotes++
		}
	}
	if statusNotes != 1 {
		t.Fatalf("status notes = %d, want one", statusNotes)
	}
}

func TestStatusWriterDoesNotRegressQueuedOverRunning(t *testing.T) {
	api := newReviewGitLab(reviewTestHead)
	reporter := testStatusReporter(api)
	if _, _, _, err := reporter.writeStatus(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusRunning}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := reporter.writeStatus(context.Background(), 42, 7, "token", reviewStatusUpdate{State: reviewStatusQueued}); err != nil {
		t.Fatal(err)
	}
	if got := api.noteCount(); got != 1 {
		t.Fatalf("status notes = %d", got)
	}
	if phase := parseStatusPhase(api.notes[0].Body); phase != reviewStatusPhase(reviewStatusRunning) {
		t.Fatalf("status regressed to %q", phase)
	}
	if len(api.statusUpdates) != 1 {
		t.Fatalf("status writes = %d, want the queued write skipped", len(api.statusUpdates))
	}
}

type durationStore struct {
	*sqlite.Store
	durations []time.Duration
}

func (s *durationStore) RecentReviewDurations(context.Context, int64, string, time.Time, int) ([]time.Duration, error) {
	return append([]time.Duration(nil), s.durations...), nil
}

func TestEstimateReviewMinutesUsesMedianAndBounds(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	ctx := context.Background()
	st := &durationStore{Store: f.db}
	f.service.store = st
	st.durations = []time.Duration{9 * time.Minute, 2 * time.Minute, 4 * time.Minute}
	if got := f.service.estimateReviewMinutes(ctx, 42, "default"); got != 4 {
		t.Fatalf("estimate = %d, want the median 4", got)
	}
	st.durations = []time.Duration{45 * time.Minute}
	if got := f.service.estimateReviewMinutes(ctx, 42, "deep"); got != 30 {
		t.Fatalf("clamped estimate = %d, want 30", got)
	}
	st.durations = []time.Duration{10 * time.Second}
	if got := f.service.estimateReviewMinutes(ctx, 42, "deep"); got != 1 {
		t.Fatalf("minimum estimate = %d, want 1", got)
	}
	st.durations = nil
	if got := f.service.estimateReviewMinutes(ctx, 42, ""); got != 5 {
		t.Fatalf("no-sample estimate = %d, want 5", got)
	}
}

func TestCommandReplyBodyNeutralizesQuickActions(t *testing.T) {
	reply := commandReplyBody(555, "help", "ok\n/merge\n  /indented\n```\n/fenced\n```")
	for _, line := range strings.Split(reply, "\n") {
		if strings.HasPrefix(line, "/") && line != "/fenced" {
			t.Errorf("line %q would run as a GitLab quick action:\n%s", line, reply)
		}
	}
	if !strings.Contains(reply, "&#47;merge") || !strings.Contains(reply, "\n/fenced\n") {
		t.Fatalf("command reply = %q, want /merge neutralized and fenced /fenced kept", reply)
	}
}
