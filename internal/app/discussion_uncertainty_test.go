package app

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type lostDiscussionResponse struct{ *reviewGitLab }

type failedDiscussionList struct{ *reviewGitLab }

func (g *failedDiscussionList) ListDiscussions(context.Context, string, int64, int64) ([]gitlab.Discussion, error) {
	return nil, &gitlab.Error{Op: "list_discussions", Status: 404}
}

type blockedRecoveryGitLab struct {
	*reviewGitLab
	*blockedOutbound
}

func (g *blockedRecoveryGitLab) ListDiscussions(ctx context.Context, token string, projectID, mrIID int64) ([]gitlab.Discussion, error) {
	g.wait(ctx)
	return g.reviewGitLab.ListDiscussions(ctx, token, projectID, mrIID)
}

func (g *lostDiscussionResponse) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	if _, err := g.reviewGitLab.CreateDiscussion(ctx, token, projectID, iid, d); err != nil {
		return nil, err
	}
	return nil, errors.New("response lost")
}

func restartFixturePublisher(t *testing.T, f *reviewFixture, api GitLabClient) {
	t.Helper()
	p, err := NewPublisher(PublisherConfig{
		Store: f.db, Workspace: f.manager, GitLab: api, Git: f.service.git, Cipher: f.publisher.cipher,
		Engine: f.engine, ReviewDefaults: f.defaults, Profiles: f.profiles, PromptsHash: f.bundle.Hash,
	})
	if err != nil {
		t.Fatal(err)
	}
	f.publisher = p
	f.service.publisher = p
}

func TestDiscussionCrashBeforeSendStaysPendingAfterRestart(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "crash-before-send")
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) != 1 || findings[0].PublishKey == "" {
		t.Fatalf("findings = %+v, %v", findings, err)
	}
	if created, _, err := f.db.ReserveDiscussionIntent(context.Background(), 42, 7, findings[0].PublishKey); err != nil || !created {
		t.Fatalf("reserve = %v, %v", created, err)
	}
	restartFixturePublisher(t, f, &delayedDiscussionsGitLab{f.gitlab})
	if err := f.service.PublishPending(context.Background()); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("restart = %v, want unknown", err)
	}
	if got := countPublicationEffects(f).discussions; got != 0 {
		t.Fatalf("restart created %d discussions", got)
	}
	if kinds := publicationKindCounts(t, f, wf.ID); kinds[discussionPublishKind] != 0 {
		t.Fatalf("unconfirmed publication exposed: %v", kinds)
	}
	if got := workflowPublication(t, f, wf.ID); got.PublicationState != store.WorkflowPublicationPending || got.PublicationError != publicationUnknownReason {
		t.Fatalf("state = %q / %q", got.PublicationState, got.PublicationError)
	}
}

type failedDiscussionPublication struct{ ReviewStore }

func (s failedDiscussionPublication) UpsertPublication(ctx context.Context, p store.Publication) (*store.Publication, error) {
	if p.Kind == discussionPublishKind {
		return nil, errors.New("simulated lost publication write")
	}
	return s.ReviewStore.UpsertPublication(ctx, p)
}

type blockedDiscussionPublication struct {
	ReviewStore
	*blockedOutbound
}

func (s blockedDiscussionPublication) UpsertPublication(ctx context.Context, p store.Publication) (*store.Publication, error) {
	if p.Kind == discussionPublishKind {
		s.wait(ctx)
	}
	return s.ReviewStore.UpsertPublication(ctx, p)
}

func TestDiscussionPublicationWriteRespectsPolicyCancellation(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "publication-policy-change")
	block := newBlockedOutbound(t)
	f.publisher.store = blockedDiscussionPublication{ReviewStore: f.db, blockedOutbound: block}
	done := make(chan error, 1)
	go func() { done <- f.publisher.Publish(context.Background(), wf.ID) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "discussion publication write")
	rejected := errors.New("policy write rejected")
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- f.manager.Get(42).ChangeOutbound(context.Background(), func(context.Context) error { return rejected })
	}()
	select {
	case <-block.cancelled:
	case err := <-writeDone:
		t.Errorf("policy writer passed the publication write: %v", err)
		block.unblock()
		done <- <-done
		return
	case <-time.After(3 * time.Second):
		t.Error("policy writer did not cancel publication write")
		block.unblock()
		done <- <-done
		return
	}
	block.unblock()
	if err := <-writeDone; !errors.Is(err, rejected) {
		t.Fatalf("policy change = %v", err)
	}
	err := <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("publication = %v", err)
	}
	if kinds := publicationKindCounts(t, f, wf.ID); kinds[discussionPublishKind] != 0 {
		t.Fatalf("cancelled write recorded a publication: %v", kinds)
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPending {
		t.Fatalf("cancelled write state = %q", got)
	}
	f.publisher.store = f.db
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("fresh retry: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("retry created %d discussions", got)
	}
}

type failedDiscussionConfirmation struct{ ReviewStore }

func (s failedDiscussionConfirmation) ConfirmDiscussionIntent(context.Context, int64, int64, string, string) error {
	return errors.New("simulated lost confirmation write")
}

func TestDiscussionCrashBeforeReceiptWriteRecoversOnlyVisibleMarker(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "crash-before-receipt")
	f.publisher.store = failedDiscussionConfirmation{f.db}
	if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("confirmation write = %v", err)
	}
	restartFixturePublisher(t, f, &delayedDiscussionsGitLab{f.gitlab})
	if err := f.service.PublishPending(context.Background()); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("delayed recovery = %v", err)
	}
	if kinds := publicationKindCounts(t, f, wf.ID); kinds[discussionPublishKind] != 0 {
		t.Fatalf("unconfirmed publication exposed: %v", kinds)
	}
	f.publisher.gitlab = f.gitlab
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("visible recovery: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("recovery created %d discussions", got)
	}
}

func TestDiscussionCrashAfterReceiptBeforePublicationWrite(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "crash-after-response")
	f.publisher.store = failedDiscussionPublication{f.db}
	if err := f.publisher.Publish(context.Background(), wf.ID); err == nil {
		t.Fatal("publication write unexpectedly succeeded")
	}
	if kinds := publicationKindCounts(t, f, wf.ID); kinds[discussionPublishKind] != 0 {
		t.Fatalf("failed publication write exposed a discussion: %v", kinds)
	}
	restartFixturePublisher(t, f, &delayedDiscussionsGitLab{f.gitlab})
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("restart: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("restart created %d discussions", got)
	}
}

func TestDiscussionUnknownSurvivesGenerationSwitch(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "old-generation")
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("first publication = %v", err)
	}
	f.publisher.gitlab = &delayedDiscussionsGitLab{f.gitlab}
	job := f.seedReview(t, "new-generation")
	if err := f.service.RunReview(context.Background(), job); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("new generation = %v", err)
	}
	status, err := f.db.GetReviewStatus(context.Background(), 42, 7)
	if err != nil || status.State != reviewStatusPublishing {
		t.Fatalf("unknown review status = %+v, %v", status, err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("generation switch created %d discussions", got)
	}
	restartFixturePublisher(t, f, f.gitlab)
	if err := f.service.PublishPending(context.Background()); err != nil && !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("recover: %v", err)
	}
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("settle older generation: %v", err)
	}
	newWF, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil || newWF.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("recovered workflow = %+v, %v", newWF, err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("recovery created %d discussions", got)
	}
}

func TestDiscussionReceiptAfterGenerationSwitchDoesNotPublishStaleWorkflow(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "receipt-old-generation")
	block := newBlockedOutbound(t)
	f.publisher.gitlab = &acceptedDiscussionGitLab{reviewGitLab: f.gitlab, blockedOutbound: block}
	done := make(chan error, 1)
	go func() { done <- f.publisher.Publish(context.Background(), wf.ID) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "accepted discussion")
	job := f.seedReview(t, "receipt-new-generation")
	block.unblock()
	err := <-done
	done <- err
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("old publication = %v", err)
	}
	if kinds := publicationKindCounts(t, f, wf.ID); kinds[discussionPublishKind] != 0 {
		t.Fatalf("old generation recorded a published discussion: %v", kinds)
	}
	f.publisher.gitlab = &delayedDiscussionsGitLab{f.gitlab}
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("new generation: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("new generation created %d discussions", got)
	}
}

func TestDiscussionUnknownDoesNotBecomeFailedOrSkippedOnLaterErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*reviewFixture)
	}{
		{name: "missing merge request", change: func(f *reviewFixture) { f.gitlab.mrErr = &gitlab.Error{Op: "get_merge_request", Status: 404} }},
		{name: "changed diff", change: func(f *reviewFixture) { f.gitlab.version.HeadSHA = "different" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			wf := reviewWithFailedNote(t, f, "unknown-later-error")
			f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
			if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
				t.Fatalf("first publication = %v", err)
			}
			f.publisher.gitlab = f.gitlab
			tc.change(f)
			if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
				t.Errorf("later publication = %v, want unknown", err)
			}
			if got := workflowPublication(t, f, wf.ID); got.PublicationState != store.WorkflowPublicationPending || got.PublicationError != publicationUnknownReason {
				t.Errorf("state = %q / %q, want pending / unknown", got.PublicationState, got.PublicationError)
			}
			if got := countPublicationEffects(f).discussions; got != 1 {
				t.Errorf("created %d discussions", got)
			}
		})
	}
}

func TestDiscussionUnknownSurvivesCancellationSkip(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "unknown-cancel")
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("publication = %v", err)
	}
	if err := f.service.skipPublication(context.Background(), wf.ID, "review cancelled"); err != nil {
		t.Fatal(err)
	}
	got := workflowPublication(t, f, wf.ID)
	if got.PublicationState != store.WorkflowPublicationPending || got.PublicationError != publicationUnknownReason {
		t.Fatalf("cancelled unknown = %q / %q", got.PublicationState, got.PublicationError)
	}
}

func TestDiscussionUnknownSurvivesJobDetachment(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "unknown-detach")
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("publication = %v", err)
	}
	dossier, err := f.db.GetWorkflowDossier(context.Background(), wf.ID)
	if err != nil || dossier.JobID == "" {
		t.Fatalf("dossier = %+v, %v", dossier, err)
	}
	if err := f.db.DetachWorkflowJob(context.Background(), wf.ID, dossier.JobID); err != nil {
		t.Fatal(err)
	}
	got := workflowPublication(t, f, wf.ID)
	if got.PublicationState != store.WorkflowPublicationPending || got.PublicationError != publicationUnknownReason {
		t.Fatalf("detached unknown = %q / %q", got.PublicationState, got.PublicationError)
	}
}

func TestDiscussionUnknownReconciliationDoesNotReportVerdict(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	job := f.seedReview(t, "unknown-reconcile")
	if err := f.service.RunReview(ctx, job); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("review = %v", err)
	}
	leased, err := f.db.Claim(ctx, "finished-worker", []string{"review.run"}, time.Minute)
	if err != nil || leased == nil {
		t.Fatalf("claim = %+v, %v", leased, err)
	}
	if err := f.db.Complete(ctx, *leased); err != nil {
		t.Fatal(err)
	}
	before, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || before.State != reviewStatusPublishing {
		t.Fatalf("before reconciliation = %+v, %v", before, err)
	}
	f.service.store = &stateListStore{Store: f.db, statuses: []store.ReviewStatus{*before}}
	if err := f.service.ReconcileSideEffects(ctx); err != nil {
		t.Fatal(err)
	}
	after, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil || after.State != reviewStatusPublishing {
		t.Fatalf("unknown reconciliation = %+v, %v; want publishing", after, err)
	}
}

func TestDiscussionRecoveryRequiresOneBotFirstNote(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	key := review.PublishKeyFor("main.go", "title", "evidence")
	marker := review.FindingMarkerPrefix + " key=" + key + " -->\nbody"
	for _, tc := range []struct {
		name        string
		discussions []gitlab.Discussion
		wantID      string
	}{
		{name: "missing"},
		{name: "foreign author", discussions: []gitlab.Discussion{{ID: "foreign", Notes: []gitlab.Note{{ID: 1, AuthorID: 42, Body: marker}}}}},
		{name: "system note", discussions: []gitlab.Discussion{{ID: "system", Notes: []gitlab.Note{{ID: 1, AuthorID: 9, System: true, Body: marker}}}}},
		{name: "marker in reply", discussions: []gitlab.Discussion{{ID: "reply", Notes: []gitlab.Note{{ID: 1, AuthorID: 9, Body: "first"}, {ID: 2, AuthorID: 9, Body: marker}}}}},
		{name: "invalid note id", discussions: []gitlab.Discussion{{ID: "invalid", Notes: []gitlab.Note{{AuthorID: 9, Body: marker}}}}},
		{name: "unique", discussions: []gitlab.Discussion{{ID: "one", Notes: []gitlab.Note{{ID: 1, AuthorID: 9, Body: marker}}}}, wantID: "one"},
		{name: "multiple", discussions: []gitlab.Discussion{{ID: "one", Notes: []gitlab.Note{{ID: 1, AuthorID: 9, Body: marker}}}, {ID: "two", Notes: []gitlab.Note{{ID: 2, AuthorID: 9, Body: marker}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.gitlab.discussions = tc.discussions
			id, err := f.publisher.recoverDiscussion(context.Background(), "token", 42, 7, key)
			if id != tc.wantID || (tc.wantID == "" && !errors.Is(err, errDiscussionUnknown)) || (tc.wantID != "" && err != nil) {
				t.Fatalf("recovery = %q, %v", id, err)
			}
		})
	}
}

type scopedDiscussionList struct {
	*reviewGitLab
	byScope map[[2]int64][]gitlab.Discussion
}

func (g *scopedDiscussionList) ListDiscussions(_ context.Context, _ string, projectID, mrIID int64) ([]gitlab.Discussion, error) {
	return g.byScope[[2]int64{projectID, mrIID}], nil
}

func TestDiscussionRecoveryChecksProjectAndMRBoundary(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	key := review.PublishKeyFor("main.go", "title", "evidence")
	marker := review.FindingMarkerPrefix + " key=" + key + " -->\nbody"
	match := []gitlab.Discussion{{ID: "other-scope", Notes: []gitlab.Note{{ID: 1, AuthorID: 9, Body: marker}}}}
	f.publisher.gitlab = &scopedDiscussionList{reviewGitLab: f.gitlab, byScope: map[[2]int64][]gitlab.Discussion{{42, 8}: match, {43, 7}: match}}
	if _, err := f.publisher.recoverDiscussion(context.Background(), "token", 42, 7, key); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("other project/MR marker accepted: %v", err)
	}
	for _, scope := range [][2]int64{{42, 8}, {43, 7}} {
		if id, err := f.publisher.recoverDiscussion(context.Background(), "token", scope[0], scope[1], key); err != nil || id != "other-scope" {
			t.Fatalf("own marker in %v = %q, %v", scope, id, err)
		}
	}
}

func TestDiscussionReregisteredProjectDoesNotExposeOldInstanceReceipt(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	oldWF := reviewWithFailedNote(t, f, "old-project-instance")
	findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: oldWF.ID})
	if err != nil || len(findings) != 1 {
		t.Fatalf("old findings = %+v, %v", findings, err)
	}
	key := findings[0].PublishKey
	if created, _, err := f.db.ReserveDiscussionIntent(ctx, 42, 7, key); err != nil || !created {
		t.Fatalf("reserve old intent = %v, %v", created, err)
	}
	if err := f.db.ConfirmDiscussionIntent(ctx, 42, 7, key, "old-instance-id"); err != nil {
		t.Fatal(err)
	}
	if err := f.db.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	seedProject(t, f.db, f.publisher.cipher, 42, "whsec", "{}", "default")
	marker := review.FindingMarkerPrefix + " key=" + key + " -->\nbody"
	f.gitlab.discussions = []gitlab.Discussion{{ID: "new-instance-id", Notes: []gitlab.Note{{ID: 1, AuthorID: 9, Body: marker}}}}
	newWF := reviewWithFailedNote(t, f, "new-project-instance")
	if err := f.publisher.Publish(ctx, newWF.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("conflicting instance publication = %v", err)
	}
	publications, err := f.db.ListPublications(ctx, newWF.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range publications {
		if p.Kind == discussionPublishKind || p.GitlabID == "old-instance-id" || p.GitlabID == "new-instance-id" {
			t.Fatalf("unverified cross-instance ID exposed: %+v", p)
		}
	}
	if got := workflowPublication(t, f, newWF.ID); got.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("conflicting instance state = %q", got.PublicationState)
	}
	if got := countPublicationEffects(f).discussions; got != 0 {
		t.Fatalf("reregistration created %d discussions", got)
	}
}

func TestDiscussionLostResponseDoesNotRetryAndRejectsAmbiguousMarkers(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "uncertain-discussion")
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.publisher.Publish(context.Background(), wf.ID); err == nil {
		t.Fatal("lost response must not succeed")
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("lost response caused %d creates", got)
	}
	if kinds := publicationKindCounts(t, f, wf.ID); kinds[discussionPublishKind] != 0 {
		t.Fatalf("unconfirmed publication exposed: %v", kinds)
	}
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) != 1 || findings[0].Evidence == "" {
		t.Fatalf("unconfirmed findings = %+v, %v", findings, err)
	}
	if _, err := f.db.SetFindingAnnotation(context.Background(), wf.ID, findings[0].ID, store.FindingAnnotationUncertain, "reviewer"); err != nil {
		t.Fatal(err)
	}
	f.gitlab.mu.Lock()
	duplicate := f.gitlab.discussions[0]
	duplicate.ID = "second-match"
	f.gitlab.discussions = append(f.gitlab.discussions, duplicate)
	f.gitlab.mu.Unlock()
	f.publisher.gitlab = f.gitlab
	if err := f.publisher.Republish(context.Background(), wf.ID); err == nil {
		t.Fatal("manual republish accepted ambiguous matching discussions")
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("republish created %d discussions", got)
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPending {
		t.Fatalf("uncertain state = %q", got)
	}
	if kinds := publicationKindCounts(t, f, wf.ID); kinds[discussionPublishKind] != 0 {
		t.Fatalf("ambiguous publication exposed: %v", kinds)
	}
	after, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(after) != 1 || after[0].Evidence != findings[0].Evidence || after[0].Annotation != store.FindingAnnotationUncertain {
		t.Fatalf("finding evidence or annotation lost: %+v, %v", after, err)
	}
}

func TestDiscussionListErrorIsUnknown(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "list-error")
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("first publication = %v", err)
	}
	f.publisher.gitlab = &failedDiscussionList{f.gitlab}
	if err := f.publisher.Republish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("list failure = %v", err)
	}
	if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPending {
		t.Fatalf("list failure state = %q", got)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("list failure created %d discussions", got)
	}
}

func TestDiscussionRecoveryRespectsPolicyCancellation(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "recovery-policy-change")
	f.publisher.gitlab = &lostDiscussionResponse{f.gitlab}
	if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, errDiscussionUnknown) {
		t.Fatalf("first publication = %v", err)
	}
	block := newBlockedOutbound(t)
	f.publisher.gitlab = &blockedRecoveryGitLab{reviewGitLab: f.gitlab, blockedOutbound: block}
	done := make(chan error, 1)
	go func() { done <- f.publisher.Publish(context.Background(), wf.ID) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "discussion recovery list")
	rejected := errors.New("policy write rejected")
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- f.manager.Get(42).ChangeOutbound(context.Background(), func(context.Context) error { return rejected })
	}()
	select {
	case <-block.cancelled:
	case err := <-writeDone:
		t.Errorf("policy writer passed active recovery: %v", err)
		block.unblock()
		done <- <-done
		return
	case <-time.After(3 * time.Second):
		t.Error("policy writer did not cancel recovery")
		block.unblock()
		done <- <-done
		return
	}
	block.unblock()
	if err := <-writeDone; !errors.Is(err, rejected) {
		t.Fatalf("policy change = %v", err)
	}
	err := <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("recovery = %v", err)
	}
	if kinds := publicationKindCounts(t, f, wf.ID); kinds[discussionPublishKind] != 0 {
		t.Fatalf("stale recovery published: %v", kinds)
	}
	f.publisher.gitlab = f.gitlab
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("fresh retry: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("retry created %d discussions", got)
	}
}

func TestDiscussionAcceptedResponseSurvivesPolicyCancellationWithDelayedList(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "accepted-cancelled")
	block := newBlockedOutbound(t)
	f.publisher.gitlab = &acceptedDiscussionGitLab{reviewGitLab: f.gitlab, blockedOutbound: block}
	done := make(chan error, 1)
	go func() { done <- f.publisher.Publish(context.Background(), wf.ID) }()
	t.Cleanup(func() { block.unblock(); <-done })
	awaitOutbound(t, block.entered, "accepted discussion")
	invalidateWhileBlocked(t, f.manager.Get(42), block)
	err := <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) {
		t.Fatalf("first publication = %v", err)
	}
	f.publisher.gitlab = &delayedDiscussionsGitLab{f.gitlab}
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("confirmed response was lost: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("created %d discussions", got)
	}
}

func TestDiscussionAcceptedResponseSurvivesCallerCancellation(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "accepted-caller-cancelled")
	block := newBlockedOutbound(t)
	f.publisher.gitlab = &acceptedDiscussionGitLab{reviewGitLab: f.gitlab, blockedOutbound: block}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.publisher.Publish(ctx, wf.ID) }()
	t.Cleanup(func() { cancel(); block.unblock(); <-done })
	awaitOutbound(t, block.entered, "accepted discussion")
	cancel()
	awaitOutbound(t, block.cancelled, "caller cancellation")
	block.unblock()
	err := <-done
	done <- err
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled publication = %v", err)
	}
	restartFixturePublisher(t, f, &delayedDiscussionsGitLab{f.gitlab})
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("confirmed response was lost: %v", err)
	}
	if got := countPublicationEffects(f).discussions; got != 1 {
		t.Fatalf("created %d discussions", got)
	}
}
