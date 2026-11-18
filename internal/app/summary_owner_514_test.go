package app

import (
	"context"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// authorGuard514GitLab enforces GitLab's rule that only the author may edit a
// note: every other identity receives 403.
type authorGuard514GitLab struct {
	*reviewGitLab
	forbidden []int64
}

func (g *authorGuard514GitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	g.mu.Lock()
	var author int64
	for _, note := range g.notes {
		if note.ID == noteID {
			author = note.AuthorID
			break
		}
	}
	g.mu.Unlock()
	if author != 0 && author != g.currentUser.ID {
		g.forbidden = append(g.forbidden, noteID)
		return &gitlab.Error{Op: "update_note", Status: 403}
	}
	return g.reviewGitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
}

func noteByID514(t *testing.T, api *reviewGitLab, id int64) gitlab.Note {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	for _, note := range api.notes {
		if note.ID == id {
			return note
		}
	}
	t.Fatalf("note %d not found", id)
	return gitlab.Note{}
}

// staleSummaryBody514 changes the bot's summary note so the next publish sees
// a different body and takes the update path (the unified path skips a note
// that already matches the rendered body, #514).
func staleSummaryBody514(t *testing.T, api *reviewGitLab) {
	t.Helper()
	api.mu.Lock()
	defer api.mu.Unlock()
	for i := range api.notes {
		if strings.HasPrefix(api.notes[i].Body, review.SummaryMarkerPrefix) {
			api.notes[i].Body = review.SummaryMarkerPrefix + " v2 stale"
			return
		}
	}
	t.Fatal("summary note not found")
}

// TestSummaryCredentialSwitchUsesOwnMarker514 covers the unified path after a
// credential switch: the persisted summary binding belongs to the previous
// bot, so GitLab answers 403 when the new identity edits it. The publisher
// must look the note up by marker and current identity instead, then rebind.
func TestSummaryCredentialSwitchUsesOwnMarker514(t *testing.T) {
	ctx := context.Background()
	f, wf, before, old := publishedSummary164(t, false)
	if old.AuthorID != 9 {
		t.Fatalf("first summary author = %d, want 9", old.AuthorID)
	}
	f.gitlab.currentUser = &gitlab.User{ID: 20, Username: "shared-bot"}
	api := &authorGuard514GitLab{reviewGitLab: f.gitlab}
	f.publisher.gitlab, f.publisher.retries = api, 1
	record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	bound, err := bindOutbound(ctx, f.manager.Get(42))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.publisher.upsertSummary(bound, 42, 7, "token", before.Fingerprint, review.SummaryMarkerPrefix+"switched", wf.ID, record.Generation, f.head, func() error { return nil }); err != nil {
		t.Fatalf("summary after credential switch: %v", err)
	}
	if len(api.forbidden) != 0 {
		t.Fatalf("publisher edited another author's note: %v", api.forbidden)
	}
	pub := summaryPublication164(t, f)
	if pub.GitlabID == before.GitlabID || pub.WorkflowID != wf.ID {
		t.Fatalf("binding was not rebound: before=%+v after=%+v", before, pub)
	}
	reboundID, err := strconv.ParseInt(pub.GitlabID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	rebound := noteByID514(t, f.gitlab, reboundID)
	if rebound.AuthorID != 20 || !strings.HasPrefix(rebound.Body, review.SummaryMarkerPrefix) {
		t.Fatalf("rebound note = %+v", rebound)
	}
	if got := noteByID514(t, f.gitlab, old.ID); got.Body != old.Body || got.AuthorID != old.AuthorID {
		t.Fatalf("previous bot's note changed: before=%+v after=%+v", old, got)
	}
}

// TestSummaryLegacyBindingStillUsedWithoutControl514 keeps the compatibility
// path: without a review control row the persisted binding is still updated,
// and a 404 on it falls back to the marker scan.
func TestSummaryLegacyBindingStillUsedWithoutControl514(t *testing.T) {
	ctx := context.Background()
	f, wf, before, _ := publishedSummary164(t, false)
	record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	// Hide the control so upsertSummary takes the legacy compatibility path.
	f.publisher.store = noControl514Store{ReviewStore: f.db}
	api := &deletedSummary164GitLab{reviewGitLab: f.gitlab, updateErr: &gitlab.Error{Op: "update_note", Status: 404}}
	f.publisher.gitlab, f.publisher.retries = api, 1
	bound, err := bindOutbound(ctx, f.manager.Get(42))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.publisher.upsertSummary(bound, 42, 7, "token", before.Fingerprint, review.SummaryMarkerPrefix+"legacy", wf.ID, record.Generation, f.head, func() error { return nil }); !gitlab.IsNotFound(err) {
		t.Fatalf("error = %v, want original 404", err)
	}
	if len(api.updates) != 1 || api.updates[0] != noteIDFromBinding514(t, before) || api.scans != 1 || api.creates != 0 {
		t.Fatalf("legacy path did not use the binding: updates=%v scans=%d creates=%d", api.updates, api.scans, api.creates)
	}
	if after := summaryPublication164(t, f); *after != *before {
		t.Fatalf("legacy path changed the binding: before=%+v after=%+v", before, after)
	}
}

type noControl514Store struct {
	ReviewStore
}

func (s noControl514Store) GetReviewControl(context.Context, int64, int64) (*store.ReviewControl, error) {
	return nil, store.ErrNotFound
}

func noteIDFromBinding514(t *testing.T, pub *store.Publication) int64 {
	t.Helper()
	id, err := strconv.ParseInt(pub.GitlabID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return id
}