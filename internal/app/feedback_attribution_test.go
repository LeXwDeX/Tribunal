package app

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

// feedbackDiscussionNote returns the publisher note of the discussion recorded
// by publication. The publisher creates the thread with that first note.
func feedbackDiscussionNote(t *testing.T, f *reviewFixture, publication store.Publication) gitlab.Note {
	t.Helper()
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	for _, discussion := range f.gitlab.discussions {
		if discussion.ID == publication.GitlabID && len(discussion.Notes) > 0 {
			return discussion.Notes[0]
		}
	}
	t.Fatalf("discussion %q has no publisher note", publication.GitlabID)
	return gitlab.Note{}
}

func feedbackRowsFor(t *testing.T, f *reviewFixture, publicationID string) []store.Feedback {
	t.Helper()
	rows, _, err := f.db.ListFeedback(context.Background(), store.FeedbackFilter{PublicationID: publicationID})
	if err != nil {
		t.Fatalf("ListFeedback: %v", err)
	}
	return rows
}

func TestFeedbackDiscussionAttributesPersistedFinding(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-feedback-attribution")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, publication := feedbackPublication(t, f)
	finding := feedbackFindings(t, f, workflow.ID, publication.PublishKey)[0]
	publisherNote := feedbackDiscussionNote(t, f, publication)

	// A reviewer reply must not act as approval of the published finding.
	f.gitlab.mu.Lock()
	for i := range f.gitlab.discussions {
		if f.gitlab.discussions[i].ID != publication.GitlabID {
			continue
		}
		f.gitlab.discussions[i].Notes[0].Resolved = true
		f.gitlab.discussions[i].Notes = append(f.gitlab.discussions[i].Notes,
			gitlab.Note{ID: 9001, AuthorID: 12, Body: "reviewer reply", Resolvable: true})
	}
	f.gitlab.awards[publisherNote.ID] = []gitlab.Award{
		{ID: 1, Name: "thumbsup", UserID: 5},
		{ID: 2, Name: "thumbsdown", UserID: 7},
	}
	f.gitlab.awards[9001] = []gitlab.Award{{ID: 3, Name: "thumbsup", UserID: 6}}
	f.gitlab.mu.Unlock()

	service := newFeedbackService(t, f, f.git, f.manager)
	fixed := time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
	service.now = func() time.Time { return fixed }

	service.RunOnce(ctx)
	first := feedbackRowsFor(t, f, publication.ID)
	if len(first) != 3 {
		t.Fatalf("feedback rows = %+v, want both publisher awards and one resolve signal", first)
	}
	seen := make(map[string]store.Feedback, len(first))
	for _, row := range first {
		seen[row.Signal] = row
	}
	thumb, ok := seen["thumbsup"]
	if !ok {
		t.Fatalf("feedback rows = %+v, want publisher thumbsup", first)
	}
	if thumb.FindingID != finding.ID {
		t.Fatalf("thumbsup finding = %q, want %q", thumb.FindingID, finding.ID)
	}
	if thumb.GitlabUserID == nil || *thumb.GitlabUserID != 5 {
		t.Fatalf("thumbsup user = %v, want publisher-note user 5", thumb.GitlabUserID)
	}
	down, ok := seen["thumbsdown"]
	if !ok || down.FindingID != finding.ID {
		t.Fatalf("thumbsdown feedback = %+v, want finding %q", down, finding.ID)
	}
	if down.GitlabUserID == nil || *down.GitlabUserID != 7 {
		t.Fatalf("thumbsdown user = %v, want publisher-note user 7", down.GitlabUserID)
	}
	resolved, ok := seen["resolved_unchanged"]
	if !ok || resolved.FindingID != finding.ID {
		t.Fatalf("resolved feedback = %+v, want finding %q", seen, finding.ID)
	}

	service.RunOnce(ctx)
	second := feedbackRowsFor(t, f, publication.ID)
	if len(second) != len(first) {
		t.Fatalf("repeated pass rows = %+v, want %d stable rows", second, len(first))
	}
	for _, row := range second {
		if row.ID != seen[row.Signal].ID {
			t.Fatalf("repeated pass changed %s row identity: %+v", row.Signal, row)
		}
	}
}

func TestFeedbackKeepsUnlinkedSignalWhenFindingMissing(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-feedback-missing-finding")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, publication := feedbackPublication(t, f)
	findings := feedbackFindings(t, f, workflow.ID, publication.PublishKey)
	findings[0].PublishKey = "other-publication-key"
	if err := f.db.ReplaceFindings(ctx, workflow.ID, findings); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	note := feedbackDiscussionNote(t, f, publication)

	f.gitlab.mu.Lock()
	for i := range f.gitlab.discussions {
		if f.gitlab.discussions[i].ID == publication.GitlabID {
			f.gitlab.discussions[i].Notes[0].Resolved = true
		}
	}
	f.gitlab.awards[note.ID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	f.gitlab.mu.Unlock()

	service := newFeedbackService(t, f, f.git, f.manager)
	service.now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
	service.RunOnce(ctx)

	rows := feedbackRowsFor(t, f, publication.ID)
	if len(rows) != 1 || rows[0].Signal != "thumbsup" {
		t.Fatalf("feedback rows = %+v, want the raw award kept without a finding", rows)
	}
	if rows[0].FindingID != "" {
		t.Fatalf("award linked unknown finding %q", rows[0].FindingID)
	}
}

func TestFeedbackDoesNotAttributeToMismatchedPublication(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-feedback-mismatched-publication")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, publication := feedbackPublication(t, f)
	note := feedbackDiscussionNote(t, f, publication)
	project, err := f.db.GetProject(ctx, 42)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}

	f.gitlab.mu.Lock()
	for i := range f.gitlab.discussions {
		if f.gitlab.discussions[i].ID == publication.GitlabID {
			f.gitlab.discussions[i].Notes[0].Resolved = true
		}
	}
	f.gitlab.awards[note.ID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	f.gitlab.mu.Unlock()

	mismatched := publication
	mismatched.MRIID++
	mr := &gitlab.MergeRequest{IID: 7, State: "opened", SHA: reviewTestHead}
	service := newFeedbackService(t, f, f.git, f.manager)
	service.now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
	// The award is still a valid signal for the publication, but the resolve
	// needs diff evidence from the matching merge request.
	service.collectDiscussion(ctx, "token", project, mismatched, *workflow, mr, 7)

	rows := feedbackRowsFor(t, f, publication.ID)
	if len(rows) != 1 || rows[0].Signal != "thumbsup" {
		t.Fatalf("feedback rows = %+v, want one raw award", rows)
	}
	if rows[0].FindingID != "" {
		t.Fatalf("mismatched publication linked finding %q", rows[0].FindingID)
	}
}

func TestFeedbackSummaryAwardsStayWorkflowLevel(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-feedback-summary-award")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, discussion := feedbackPublication(t, f)
	publications, err := f.db.ListPublications(ctx, workflow.ID)
	if err != nil {
		t.Fatalf("ListPublications: %v", err)
	}
	var summary store.Publication
	for _, candidate := range publications {
		if candidate.Kind == summaryPublishKind {
			summary = candidate
			break
		}
	}
	if summary.ID == "" {
		t.Fatal("summary publication not found")
	}
	summaryNoteID, err := strconv.ParseInt(summary.GitlabID, 10, 64)
	if err != nil || summaryNoteID <= 0 {
		t.Fatalf("summary note id = %q", summary.GitlabID)
	}

	f.gitlab.mu.Lock()
	f.gitlab.awards[summaryNoteID] = []gitlab.Award{{ID: 1, Name: "thumbsup", UserID: 5}}
	f.gitlab.mu.Unlock()

	service := newFeedbackService(t, f, f.git, f.manager)
	service.now = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
	service.RunOnce(ctx)

	rows := feedbackRowsFor(t, f, summary.ID)
	if len(rows) != 1 || rows[0].Signal != "thumbsup" {
		t.Fatalf("summary feedback = %+v", rows)
	}
	if rows[0].FindingID != "" {
		t.Fatalf("summary award linked finding %q", rows[0].FindingID)
	}
	if rows := feedbackRowsFor(t, f, discussion.ID); len(rows) != 0 {
		t.Fatalf("summary award leaked into discussion feedback: %+v", rows)
	}
}
