package app

import (
	"context"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// contextDiff has one unchanged line that both diff sides reach: old line 1
// and new line 1 are the same physical context line.
const contextDiff = "@@ -1,3 +1,3 @@\n a\n-b\n+c\n d\n"

func discussionPublications(t *testing.T, f *reviewFixture, workflowID string) []store.Publication {
	t.Helper()
	publications, err := f.db.ListPublications(context.Background(), workflowID)
	if err != nil {
		t.Fatalf("ListPublications: %v", err)
	}
	var out []store.Publication
	for _, publication := range publications {
		if publication.Kind == discussionPublishKind {
			out = append(out, publication)
		}
	}
	return out
}

func TestInlinePositionContextLineMergesBothSides(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "main.go", Diff: contextDiff}}
	oldSide := positionFinding("old side problem", "the unchanged line breaks on the old side")
	oldSide.Line, oldSide.Side = 1, "old"
	newSide := positionFinding("new side problem", "the unchanged line breaks on the new side")
	newSide.Line, newSide.Side = 1, "new"
	f.runner.findings = []review.FindingOut{oldSide, newSide}
	workflow := runReviewGeneration(t, f, "context-merge-gen1")

	if f.gitlab.discussionCreates != 1 {
		t.Fatalf("context line created %d discussions, want 1", f.gitlab.discussionCreates)
	}
	body := onlyDiscussion(t, f).Notes[0].Body
	if !strings.Contains(body, "old side problem") || !strings.Contains(body, "new side problem") {
		t.Fatalf("context thread lost one side: %q", body)
	}
	key := review.PositionAnchorKey("main.go", 1, review.SideNew,
		gitlab.Position{OldPath: "main.go", NewPath: "main.go", OldLine: 1, NewLine: 1})
	if !strings.HasPrefix(body, review.FindingMarkerPrefix+" key="+key+" -->") {
		t.Fatalf("context thread marker = %q, want the canonical new-side key", body)
	}
	publications := discussionPublications(t, f, workflow.ID)
	if len(publications) != 1 || publications[0].PublishKey != key {
		t.Fatalf("publications = %+v, want one canonical position row", publications)
	}
	if len(f.gitlab.notes) != 1 {
		t.Fatalf("summary notes = %+v, want one summary", f.gitlab.notes)
	}
}

func TestLegacyContextThreadIsAdoptedOnceForBothSides(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "main.go", Diff: contextDiff}}
	legacyKey := review.PublishKeyFor("main.go", "old wording", "old evidence")
	legacyBody := review.FindingMarkerPrefix + " key=" + legacyKey + " -->\n**old wording**\n"
	f.gitlab.discussions = []gitlab.Discussion{{
		ID: "legacy-context",
		Notes: []gitlab.Note{{
			ID: 500, AuthorID: 9, Body: legacyBody,
			Position: &gitlab.Position{OldPath: "main.go", NewPath: "main.go",
				OldLine: 1, NewLine: 1, HeadSHA: reviewTestHead},
		}},
	}}
	oldSide := positionFinding("old side problem", "the unchanged line breaks on the old side")
	oldSide.Line, oldSide.Side = 1, "old"
	newSide := positionFinding("new side problem", "the unchanged line breaks on the new side")
	newSide.Line, newSide.Side = 1, "new"
	f.runner.findings = []review.FindingOut{oldSide, newSide}
	runReviewGeneration(t, f, "context-legacy-gen1")

	if f.gitlab.discussionCreates != 0 {
		t.Fatalf("legacy context thread was duplicated: creates = %d", f.gitlab.discussionCreates)
	}
	if f.gitlab.discussionUpdates != 1 {
		t.Fatalf("legacy context thread updates = %d, want 1", f.gitlab.discussionUpdates)
	}
	discussion := onlyDiscussion(t, f)
	body := discussion.Notes[0].Body
	if !strings.Contains(body, "old side problem") || !strings.Contains(body, "new side problem") {
		t.Fatalf("legacy context thread lost one side: %q", body)
	}
	if strings.Contains(body, "old wording") {
		t.Fatalf("legacy context thread kept the superseded wording: %q", body)
	}
}

func TestInlinePositionAddedAndDeletedLinesStaySeparate(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "main.go", Diff: "@@ -1,1 +1,1 @@\n-old\n+new\n"}}
	deleted := positionFinding("deleted line problem", "the removed line was the only guard")
	deleted.Line, deleted.Side = 1, "old"
	added := positionFinding("added line problem", "the new line skips validation")
	added.Line, added.Side = 1, "new"
	f.runner.findings = []review.FindingOut{deleted, added}
	runReviewGeneration(t, f, "non-context-sides-gen1")

	if f.gitlab.discussionCreates != 2 {
		t.Fatalf("added and deleted lines created %d discussions, want 2", f.gitlab.discussionCreates)
	}
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	if len(f.gitlab.discussions) != 2 {
		t.Fatalf("discussions = %d, want 2", len(f.gitlab.discussions))
	}
	titles := 0
	for _, discussion := range f.gitlab.discussions {
		body := discussion.Notes[0].Body
		if strings.Contains(body, "deleted line problem") || strings.Contains(body, "added line problem") {
			titles++
		}
	}
	if titles != 2 {
		t.Fatalf("sides were merged: %+v", f.gitlab.discussions)
	}
}

func TestAdoptionMatchesTheGroupThatHoldsTheLegacyKey(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	// A legacy workflow owns the fingerprint row and the finding it proved. It
	// publishes no inline thread of its own; the historical thread is created
	// below without a note position, so adoption must prove the source.
	f.runner.findings = []review.FindingOut{}
	legacyJob := f.seedReview(t, "legacy-category")
	if err := f.service.Handle(ctx, legacyJob); err != nil {
		t.Fatalf("legacy Handle: %v", err)
	}
	legacyWorkflow, err := f.db.GetWorkflowByJob(ctx, legacyJob.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	holder := positionFinding("correctness holder", "the holder body")
	legacyKey := review.FindingPublishKey(testDossier(t, reviewTestHead), "main.go", review.SideNew, holder.Title, holder.Evidence, "legacy")
	legacyFinding := store.Finding{
		ID: "legacy-holder", WorkflowID: legacyWorkflow.ID, OriginNode: "legacy",
		Severity: "P2", Category: "correctness", Path: "main.go", Line: intPtr(2),
		Side: "new", Title: holder.Title, Body: holder.Body, Evidence: holder.Evidence,
		PublishKey: legacyKey,
	}
	if err := f.db.ReplaceFindings(ctx, legacyWorkflow.ID, []store.Finding{legacyFinding}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	legacyBody := review.FindingMarkerPrefix + " key=" + legacyKey + " -->\n**legacy wording**\n"
	legacyDiscussion, err := f.gitlab.CreateDiscussion(ctx, "token", 42, 7,
		gitlab.NewDiscussion{Body: legacyBody})
	if err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	if _, err := f.db.UpsertPublication(ctx, store.Publication{
		ProjectID: 42, MRIID: 7, Kind: discussionPublishKind, PublishKey: legacyKey,
		GitlabID: legacyDiscussion.ID, WorkflowID: legacyWorkflow.ID, Fingerprint: "legacy",
	}); err != nil {
		t.Fatalf("UpsertPublication: %v", err)
	}

	security := positionFinding("security first", "the first finding has another category")
	security.Category = "security"
	f.runner.findings = []review.FindingOut{security, holder}
	creates := f.gitlab.discussionCreates
	workflow := runReviewGeneration(t, f, "legacy-category-current")
	if f.gitlab.discussionCreates != creates {
		t.Fatalf("legacy thread was duplicated: creates = %d", f.gitlab.discussionCreates-creates)
	}
	if f.gitlab.discussionUpdates != 1 {
		t.Fatalf("legacy thread updates = %d, want 1", f.gitlab.discussionUpdates)
	}
	body := onlyDiscussion(t, f).Notes[0].Body
	if !strings.Contains(body, "security first") || !strings.Contains(body, "correctness holder") {
		t.Fatalf("adopted thread lacks a finding: %q", body)
	}
	publications := discussionPublications(t, f, workflow.ID)
	if len(publications) != 1 || publications[0].GitlabID != legacyDiscussion.ID {
		t.Fatalf("publications = %+v, want the adopted thread under the position key", publications)
	}
	if len(f.gitlab.notes) != 1 || strings.Contains(f.gitlab.notes[0].Body, "历史讨论内容") {
		t.Fatalf("summary reported an unproven historical thread: %+v", f.gitlab.notes)
	}
}

func TestAdoptionSettlesUnconfirmedPositionIntent(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	key := review.PositionPublishKey("main.go", 2, review.SideNew)
	if created, id, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, key); err != nil || !created || id != "" {
		t.Fatalf("ReserveDiscussionIntent = (%v, %q, %v)", created, id, err)
	}
	legacyKey := review.PublishKeyFor("main.go", "old wording", "old evidence")
	f.gitlab.discussions = []gitlab.Discussion{{
		ID: "legacy-position-intent",
		Notes: []gitlab.Note{{
			ID: 500, AuthorID: 9,
			Body:     review.FindingMarkerPrefix + " key=" + legacyKey + " -->\n**old wording**\n",
			Position: &gitlab.Position{NewPath: "main.go", NewLine: 2, HeadSHA: reviewTestHead},
		}},
	}}
	f.runner.findings = []review.FindingOut{positionFinding("bad assignment", "the assignment is wrong")}
	runReviewGeneration(t, f, "intent-adopt-gen1")
	if f.gitlab.discussionCreates != 0 || f.gitlab.discussionUpdates != 1 {
		t.Fatalf("creates = %d, updates = %d, want adoption only", f.gitlab.discussionCreates, f.gitlab.discussionUpdates)
	}
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil {
		t.Fatalf("HasUnknownDiscussionIntent: %v", err)
	}
	if unknown {
		t.Fatal("adoption left the position intent unresolved")
	}
}

func TestAdoptionLeavesOtherUnconfirmedIntents(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	key := review.PositionPublishKey("main.go", 2, review.SideNew)
	otherKey := review.PositionPublishKey("main.go", 3, review.SideNew)
	for _, intentKey := range []string{key, otherKey} {
		if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, intentKey); err != nil || !created {
			t.Fatalf("ReserveDiscussionIntent(%s) = (%v, %v)", intentKey, created, err)
		}
	}
	legacyKey := review.PublishKeyFor("main.go", "old wording", "old evidence")
	f.gitlab.discussions = []gitlab.Discussion{{
		ID: "legacy-other-intent",
		Notes: []gitlab.Note{{
			ID: 501, AuthorID: 9,
			Body:     review.FindingMarkerPrefix + " key=" + legacyKey + " -->\n**old wording**\n",
			Position: &gitlab.Position{NewPath: "main.go", NewLine: 2, HeadSHA: reviewTestHead},
		}},
	}}
	f.runner.findings = []review.FindingOut{positionFinding("bad assignment", "the assignment is wrong")}
	runReviewGeneration(t, f, "intent-other-gen1")
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil || !unknown {
		t.Fatalf("HasUnknownDiscussionIntent = (%v, %v), want true for the untouched intent", unknown, err)
	}
	if created, id, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, otherKey); err != nil || created || id != "" {
		t.Fatalf("other intent changed: created=%v id=%q err=%v", created, id, err)
	}
}

// TestReconcileKeepsFingerprintThreadOnSummaryFallback covers a finding that
// fell back to the summary while its remote thread still carries the
// fingerprint marker. Reconcile must keep that fingerprint key current instead
// of resolving the live thread under an unpublished position key.
func TestReconcileKeepsFingerprintThreadOnSummaryFallback(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.runner.findings = []review.FindingOut{}
	legacyJob := f.seedReview(t, "legacy-reconcile")
	if err := f.service.Handle(ctx, legacyJob); err != nil {
		t.Fatalf("legacy Handle: %v", err)
	}
	legacyWorkflow, err := f.db.GetWorkflowByJob(ctx, legacyJob.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	finding := positionFinding("bad assignment", "the assignment is wrong")
	fingerprintKey := review.FindingPublishKey(testDossier(t, reviewTestHead), "main.go", review.SideNew,
		finding.Title, finding.Evidence, "legacy")
	f.gitlab.discussions = []gitlab.Discussion{{
		ID: "fingerprint-thread",
		Notes: []gitlab.Note{{
			ID: 600, AuthorID: 9, Resolvable: true,
			// No position metadata: the thread is only reachable by its marker.
			Body: review.FindingMarkerPrefix + " key=" + fingerprintKey + " -->\n**bad assignment**\n",
		}},
	}}
	if _, err := f.db.UpsertPublication(ctx, store.Publication{
		ProjectID: 42, MRIID: 7, Kind: discussionPublishKind, PublishKey: fingerprintKey,
		GitlabID: "missing-thread", WorkflowID: legacyWorkflow.ID, Fingerprint: "legacy",
	}); err != nil {
		t.Fatalf("UpsertPublication: %v", err)
	}
	f.runner.findings = []review.FindingOut{finding}
	runReviewGeneration(t, f, "reconcile-fallback-gen1")

	for _, resolved := range f.gitlab.discussionResolves {
		if resolved {
			t.Fatal("reconcile resolved a live fingerprint thread left in the summary")
		}
	}
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	if len(f.gitlab.discussions) != 1 || f.gitlab.discussions[0].ID != "fingerprint-thread" {
		t.Fatalf("discussions = %+v, want the untouched fingerprint thread", f.gitlab.discussions)
	}
	if !strings.HasPrefix(f.gitlab.discussions[0].Notes[0].Body, review.FindingMarkerPrefix+" key="+fingerprintKey+" -->") {
		t.Fatalf("fingerprint thread was rewritten: %q", f.gitlab.discussions[0].Notes[0].Body)
	}
}

func TestAdoptionScansLegacyRowsAfterWordingChange(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.runner.findings = []review.FindingOut{}
	legacyJob := f.seedReview(t, "legacy-reword")
	if err := f.service.Handle(ctx, legacyJob); err != nil {
		t.Fatalf("legacy Handle: %v", err)
	}
	legacyWorkflow, err := f.db.GetWorkflowByJob(ctx, legacyJob.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	legacyKey := review.PublishKeyFor("main.go", "old wording", "old evidence")
	legacyFinding := store.Finding{
		ID: "legacy-reworded", WorkflowID: legacyWorkflow.ID, OriginNode: "legacy",
		Severity: "P2", Category: "correctness", Path: "main.go", Line: intPtr(2),
		Side: "new", Title: "old wording", Body: "old body", Evidence: "old evidence",
		PublishKey: legacyKey,
	}
	if err := f.db.ReplaceFindings(ctx, legacyWorkflow.ID, []store.Finding{legacyFinding}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	legacyDiscussion, err := f.gitlab.CreateDiscussion(ctx, "token", 42, 7, gitlab.NewDiscussion{
		Body: review.FindingMarkerPrefix + " key=" + legacyKey + " -->\n**old wording**\n",
	})
	if err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	if _, err := f.db.UpsertPublication(ctx, store.Publication{
		ProjectID: 42, MRIID: 7, Kind: discussionPublishKind, PublishKey: legacyKey,
		GitlabID: legacyDiscussion.ID, WorkflowID: legacyWorkflow.ID, Fingerprint: "legacy",
	}); err != nil {
		t.Fatalf("UpsertPublication: %v", err)
	}

	f.runner.findings = []review.FindingOut{positionFinding("bad assignment", "the assignment is wrong")}
	creates := f.gitlab.discussionCreates
	runReviewGeneration(t, f, "reword-current-gen1")
	if f.gitlab.discussionCreates != creates {
		t.Fatalf("reworded finding stacked a duplicate: creates = %d", f.gitlab.discussionCreates-creates)
	}
	discussion := onlyDiscussion(t, f)
	if discussion.ID != legacyDiscussion.ID {
		t.Fatalf("adopted discussion = %q, want %q", discussion.ID, legacyDiscussion.ID)
	}
	if body := discussion.Notes[0].Body; !strings.Contains(body, "bad assignment") || strings.Contains(body, "old wording") {
		t.Fatalf("legacy thread was not updated for the new wording: %q", body)
	}
}
