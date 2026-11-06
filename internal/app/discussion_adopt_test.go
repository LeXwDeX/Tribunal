package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// crashAfterDiscussion reproduces a SIGKILL after the discussion was posted and
// its intent recorded, but before the publication row was written.
func crashAfterDiscussion(t *testing.T, f *reviewFixture, jobID string) (*store.Workflow, string) {
	t.Helper()
	ctx := context.Background()
	wf := reviewWithFailedNote(t, f, jobID)
	real := f.publisher.store
	f.publisher.store = failedDiscussionPublication{real}
	if err := f.publisher.Publish(ctx, wf.ID); err == nil {
		t.Fatal("expected failed first publication")
	}
	f.publisher.store = real
	findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) != 1 || f.gitlab.discussionCreates != 1 {
		t.Fatalf("findings=%+v creates=%d err=%v", findings, f.gitlab.discussionCreates, err)
	}
	return wf, findings[0].PublishKey
}

func assertAdopted(t *testing.T, f *reviewFixture, wf *store.Workflow, key string) {
	t.Helper()
	ctx := context.Background()
	findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) != 1 || findings[0].PositionOK == nil || !*findings[0].PositionOK {
		t.Fatalf("finding must stay inline: %+v, %v", findings, err)
	}
	pub, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, key)
	if err != nil || pub.GitlabID != f.gitlab.discussions[0].ID || pub.WorkflowID != wf.ID {
		t.Fatalf("publication = %+v, %v", pub, err)
	}
	if f.gitlab.discussionCreates != 1 {
		t.Fatalf("duplicate discussion: %d", f.gitlab.discussionCreates)
	}
	for _, n := range f.gitlab.notes {
		if strings.Contains(n.Body, "无法定位") || strings.Contains(n.Body, "历史讨论内容") {
			t.Fatalf("summary reports adopted discussion as unlocatable: %s", n.Body)
		}
	}
}

func TestCrashAfterDiscussionIsAdoptedOnRecovery(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf, key := crashAfterDiscussion(t, f, "adopt-ok")
	restartFixturePublisher(t, f, f.gitlab)
	for attempt := 0; attempt < 2; attempt++ {
		if err := f.publisher.Publish(ctx, wf.ID); err != nil {
			t.Fatal(err)
		}
		assertAdopted(t, f, wf, key)
	}
}

func TestLostResponseDiscussionIsAdoptedOnRecovery(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "adopt-lost")
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.publisher.Publish(ctx, wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("first publication: %v", err)
	}
	restartFixturePublisher(t, f, f.gitlab)
	if err := f.publisher.Publish(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	findings, _ := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
	assertAdopted(t, f, wf, findings[0].PublishKey)
}

func TestAdoptionRejectsChangedContentOrOtherWorkflow(t *testing.T) {
	for name, mutate := range map[string]func(string) string{
		"content differs": func(b string) string { return strings.Replace(b, "\n", "\nedited by hand ", 2) },
		"other workflow": func(b string) string {
			i := strings.LastIndex(b, "<!-- gitlab-ce-review:workflow id=")
			if i < 0 {
				return b
			}
			return b[:i] + "<!-- gitlab-ce-review:workflow id=another-workflow -->"
		},
		"marker missing (older version)": func(b string) string {
			i := strings.LastIndex(b, "<!-- gitlab-ce-review:workflow id=")
			if i < 0 {
				return b
			}
			return strings.TrimSpace(b[:i]) + "\n"
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			wf, key := crashAfterDiscussion(t, f, "adopt-neg")
			f.gitlab.mu.Lock()
			f.gitlab.discussions[0].Notes[0].Body = mutate(f.gitlab.discussions[0].Notes[0].Body)
			f.gitlab.mu.Unlock()
			restartFixturePublisher(t, f, f.gitlab)
			for attempt := 0; attempt < 2; attempt++ {
				if err := f.publisher.Publish(ctx, wf.ID); err != nil {
					t.Fatal(err)
				}
				findings, _ := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
				if len(findings) != 1 || findings[0].PositionOK == nil || *findings[0].PositionOK {
					t.Fatalf("finding must fall back to summary: %+v", findings)
				}
				if _, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, key); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("unproven discussion attributed: %v", err)
				}
				if f.gitlab.discussionCreates != 1 || len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "历史讨论内容") {
					t.Fatalf("conservative path broken: creates=%d notes=%+v", f.gitlab.discussionCreates, f.gitlab.notes)
				}
			}
		})
	}
}

// Discussions authored by someone else are never adopted even if the text matches.
func TestAdoptionRejectsOtherAuthor(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf, key := crashAfterDiscussion(t, f, "adopt-author")
	f.gitlab.mu.Lock()
	f.gitlab.discussions[0].Notes[0].AuthorID += 99
	f.gitlab.mu.Unlock()
	restartFixturePublisher(t, f, f.gitlab)
	if err := f.publisher.Publish(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, key); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("foreign discussion attributed: %v", err)
	}
}

func TestAdoptionFetchFailureIsRetryableWithoutPosting(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf, key := crashAfterDiscussion(t, f, "adopt-fetch")
	restartFixturePublisher(t, f, &failedDiscussionList{f.gitlab})
	err := f.publisher.Publish(ctx, wf.ID)
	if err == nil {
		t.Fatal("fetch failure must be retryable")
	}
	if errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("confirmed intent read failure must be an ordinary error: %v", err)
	}
	if f.gitlab.discussionCreates != 1 {
		t.Fatalf("posted a duplicate: %d", f.gitlab.discussionCreates)
	}
	if _, perr := f.db.GetPublication(ctx, 42, 7, discussionPublishKind, key); !errors.Is(perr, store.ErrNotFound) {
		t.Fatalf("publication written on failure: %v", perr)
	}
	restartFixturePublisher(t, f, f.gitlab)
	if err := f.publisher.Publish(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	assertAdopted(t, f, wf, key)
}

// Two publisher instances (e.g. a retry racing a manual republish) contend for
// the same workflow; neither may post a duplicate and the discussion is adopted.
func TestRacingPublishersAdoptWithoutDuplicate(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	wf, key := crashAfterDiscussion(t, f, "adopt-race")
	restartFixturePublisher(t, f, f.gitlab)
	second, err := NewPublisher(PublisherConfig{
		Store: f.db, Workspace: f.manager, GitLab: f.gitlab, Git: f.service.git, Cipher: f.publisher.cipher,
		Engine: f.engine, ReviewDefaults: f.defaults, Profiles: f.profiles, PromptsHash: f.bundle.Hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, pub := range []*Publisher{f.publisher, second, f.publisher, second} {
		wg.Add(1)
		go func(pub *Publisher) {
			defer wg.Done()
			_ = pub.Publish(ctx, wf.ID)
		}(pub)
	}
	wg.Wait()
	if err := f.publisher.Publish(ctx, wf.ID); err != nil {
		t.Fatal(err)
	}
	assertAdopted(t, f, wf, key)
}
