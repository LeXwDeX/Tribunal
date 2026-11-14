package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type permanentDiscussionGitLab struct {
	*reviewGitLab
	status int
}

func (g *permanentDiscussionGitLab) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	return nil, &gitlab.Error{Op: "create_discussion", Status: g.status}
}

func TestPermanentDiscussionErrorsClearIntentAndFallBack(t *testing.T) {
	for _, status := range []int{400, 401, 403, 404, 422} {
		t.Run(itoa(int64(status)), func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			api := &permanentDiscussionGitLab{reviewGitLab: f.gitlab, status: status}
			f.service.gitlab = api
			f.publisher.gitlab = api
			f.publisher.retries = 1
			job := f.seedReview(t, "permanent-discussion")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatalf("RunReview: %v", err)
			}
			unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
			if err != nil || unknown {
				t.Fatalf("unknown intent = %v, err = %v; a definitive rejection must clear it", unknown, err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil {
				t.Fatalf("GetWorkflowByJob: %v", err)
			}
			if wf.PublicationState != store.WorkflowPublicationPublished {
				t.Fatalf("publication state = %q, want a terminal published summary", wf.PublicationState)
			}
			bodies := strings.Join(f.gitlab.noteBodies(), "\n")
			if !strings.Contains(bodies, "bad assignment") {
				t.Fatalf("finding missing from the fallback summary: %s", bodies)
			}
		})
	}
}

func TestUnknownDiscussionAbsenceClearsIntentAndSettles(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "unknown-discussion-absent")
	findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) == 0 {
		t.Fatalf("findings = %+v, err = %v", findings, err)
	}
	key := findings[0].PublishKey
	if key == "" {
		t.Fatal("finding has no publish key")
	}
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, key); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	// The marker scan proves no matching discussion exists remotely.
	f.publisher.now = func() time.Time { return time.Now().Add(25 * time.Hour) }
	if err := f.publisher.Republish(ctx, wf.ID); err != nil {
		t.Fatalf("Republish: %v", err)
	}
	unknown, err := f.db.HasUnknownDiscussionIntent(ctx, 42, 7)
	if err != nil || unknown {
		t.Fatalf("unknown intent = %v, err = %v", unknown, err)
	}
	if got := workflowPublication(t, f, wf.ID); got.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("state = %q, want a terminal summary publication", got.PublicationState)
	}
	if got := countPublicationEffects(f).discussions; got != 0 {
		t.Fatalf("discussions = %d, want none without a proven absence retry", got)
	}
}

func TestDiscussionTargetsConvergeOnlyOnCompleteReviews(t *testing.T) {
	bot := int64(9)
	discussion := func(id, key string, resolved bool) gitlab.Discussion {
		return gitlab.Discussion{ID: id, Notes: []gitlab.Note{{
			ID: 1, AuthorID: bot, Resolvable: true, Resolved: resolved,
			Body: review.FindingMarkerPrefix + " key=" + key + " -->\n**body**\n",
		}}}
	}
	keys := map[string]struct{}{"pk_" + strings.Repeat("a", 32): {}}
	present := "pk_" + strings.Repeat("a", 32)
	absent := "pk_" + strings.Repeat("b", 32)
	discussions := []gitlab.Discussion{
		discussion("present-resolved", present, true),
		discussion("absent-open", absent, false),
		discussion("absent-resolved", absent, true),
		{ID: "foreign", Notes: []gitlab.Note{{ID: 2, AuthorID: 77, Body: review.FindingMarkerPrefix + " key=" + absent + " -->"}}},
	}
	targets := discussionTargets(discussions, bot, keys, true)
	got := map[string]bool{}
	for _, target := range targets {
		got[target.ID] = target.Resolved
	}
	if len(targets) != 2 || got["present-resolved"] != false || got["absent-open"] != true {
		t.Fatalf("targets = %+v", targets)
	}
	if _, ok := got["foreign"]; ok {
		t.Fatalf("foreign discussion touched: %+v", targets)
	}
	if len(discussionTargets(discussions, bot, keys, false)) != 0 {
		t.Fatal("an incomplete review must not resolve discussions")
	}
}

func lifecycleFinding(title, evidence string) []review.FindingOut {
	return []review.FindingOut{{
		Path: "main.go", Line: 2, Side: "new", Severity: review.SeverityP2,
		Category: "correctness", Title: title, Body: "the assignment is wrong",
		Evidence: evidence, Confidence: review.ConfidenceHigh,
	}}
}

func TestPublisherResolvesAndReopensBotDiscussions(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	first := f.seedReview(t, "lifecycle-first")
	if err := f.service.RunReview(ctx, first); err != nil {
		t.Fatalf("first review: %v", err)
	}
	f.gitlab.mu.Lock()
	created := len(f.gitlab.discussions)
	f.gitlab.mu.Unlock()
	if created == 0 {
		t.Fatal("first review published no discussion")
	}

	// A different finding in the next complete review makes the old thread
	// disappear from the current result: it must converge to resolved.
	f.runner.mu.Lock()
	f.runner.findings = lifecycleFinding("different issue", "different := 2")
	f.runner.mu.Unlock()
	second := f.seedReview(t, "lifecycle-second")
	if err := f.service.RunReview(ctx, second); err != nil {
		t.Fatalf("second review: %v", err)
	}
	f.gitlab.mu.Lock()
	resolved := append([]bool(nil), f.gitlab.discussionResolves...)
	firstStillOpen := false
	for _, discussion := range f.gitlab.discussions {
		if len(discussion.Notes) > 0 && strings.Contains(discussion.Notes[0].Body, "bad assignment") && !discussion.Notes[0].Resolved {
			firstStillOpen = true
		}
	}
	f.gitlab.mu.Unlock()
	if len(resolved) == 0 || !resolved[len(resolved)-1] {
		t.Fatalf("resolutions = %v, want the stale discussion resolved", resolved)
	}
	if firstStillOpen {
		t.Fatal("stale discussion stayed open")
	}

	// The original finding returns: the resolved thread is reopened instead of
	// creating a second one.
	f.runner.mu.Lock()
	f.runner.findings = lifecycleFinding("bad assignment", "bad := 1")
	f.runner.mu.Unlock()
	third := f.seedReview(t, "lifecycle-third")
	if err := f.service.RunReview(ctx, third); err != nil {
		t.Fatalf("third review: %v", err)
	}
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	reopened := false
	for _, value := range f.gitlab.discussionResolves {
		if !value {
			reopened = true
		}
	}
	if !reopened {
		t.Fatalf("resolutions = %v, want a reopened thread", f.gitlab.discussionResolves)
	}
	matching := 0
	for _, discussion := range f.gitlab.discussions {
		if len(discussion.Notes) > 0 && strings.Contains(discussion.Notes[0].Body, "bad assignment") {
			matching++
		}
	}
	if matching != 1 {
		t.Fatalf("bad assignment discussions = %d, want the existing thread reused", matching)
	}
}

func TestPublicationUnknownExpiry(t *testing.T) {
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	if publicationUnknownExpired(now, now.Add(-publicationUnknownTerminalAge+time.Minute)) {
		t.Fatal("a fresh unknown outcome expired")
	}
	if !publicationUnknownExpired(now, now.Add(-publicationUnknownTerminalAge-time.Minute)) {
		t.Fatal("an old unknown outcome did not expire")
	}
	if publicationUnknownExpired(now, time.Time{}) {
		t.Fatal("a missing timestamp expired")
	}
}
