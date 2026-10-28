package app

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

var errSummaryTransport = errors.New("synthetic transport error")

type uncertainSummaryGitLab struct {
	*reviewGitLab
	mode         string
	posts, scans int
	identityErr  error
	identity     *gitlab.User
	missingUser  bool
}

func (g *uncertainSummaryGitLab) CurrentUser(ctx context.Context, token string) (*gitlab.User, error) {
	if g.identityErr != nil || g.identity != nil || g.missingUser {
		return g.identity, g.identityErr
	}
	return g.reviewGitLab.CurrentUser(ctx, token)
}

func (g *uncertainSummaryGitLab) ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error) {
	g.scans++
	if g.mode == "scan-fails" && g.scans > 1 {
		return nil, errSummaryTransport
	}
	return g.reviewGitLab.ListMRNotes(ctx, token, projectID, iid)
}

func (g *uncertainSummaryGitLab) CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error) {
	g.posts++
	if g.mode == "400" || g.mode == "404" {
		status := 400
		if g.mode == "404" {
			status = 404
		}
		return nil, &gitlab.Error{Op: "create_note", Status: status}
	}
	if g.posts == 1 && g.mode == "not-saved" {
		return nil, errSummaryTransport
	}
	note, err := g.reviewGitLab.CreateMRNote(ctx, token, projectID, iid, body)
	if err != nil || g.posts != 1 {
		return note, err
	}
	switch g.mode {
	case "saved", "scan-fails":
		return nil, errSummaryTransport
	case "nil":
		return nil, nil
	case "invalid":
		return &gitlab.Note{}, nil
	}
	return note, nil
}

func TestSummaryUncertainCreate(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		posts   int
		wantErr bool
	}{
		{"saved", 1, false}, {"scan-fails", 1, true}, {"not-saved", 2, false},
		{"nil", 1, true}, {"invalid", 1, true},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			api := &uncertainSummaryGitLab{reviewGitLab: f.gitlab, mode: tc.mode}
			f.publisher.gitlab, f.publisher.retries = api, 2
			err := f.publisher.publishStandaloneSummaryLocked(context.Background(), 42, 7, "token", "hash", review.SummaryMarkerPrefix+"body")
			if (err != nil) != tc.wantErr {
				t.Fatalf("error = %v, want error %v", err, tc.wantErr)
			}
			if api.posts != tc.posts || countPublicationEffects(f).summaries != 1 {
				t.Fatalf("posts=%d summaries=%d", api.posts, countPublicationEffects(f).summaries)
			}
			if _, err := f.db.GetPublication(context.Background(), 42, 7, summaryPublishKind, summaryPublicationKey); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("standalone summary stored publication: %v", err)
			}
		})
	}
}

func TestSummaryIdentityAndRestart(t *testing.T) {
	for _, tc := range []struct {
		name        string
		identity    *gitlab.User
		identityErr error
		missingUser bool
		wantErr     bool
	}{
		{"lookup error", nil, errSummaryTransport, false, true},
		{"missing identity", nil, nil, true, true},
		{"invalid identity", &gitlab.User{}, nil, false, true},
		{"other author", &gitlab.User{ID: 9}, nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			f.gitlab.notes = []gitlab.Note{{ID: 80, AuthorID: 10, Body: review.SummaryMarkerPrefix + "other"}}
			api := &uncertainSummaryGitLab{reviewGitLab: f.gitlab, identity: tc.identity, identityErr: tc.identityErr, missingUser: tc.missingUser}
			f.publisher.gitlab = api
			err := f.publisher.publishStandaloneSummaryLocked(context.Background(), 42, 7, "token", "new", review.SummaryMarkerPrefix+"new")
			if (err != nil) != tc.wantErr || f.gitlab.notes[0].Body != review.SummaryMarkerPrefix+"other" {
				t.Fatalf("err=%v notes=%+v", err, f.gitlab.notes)
			}
			if tc.wantErr && api.posts != 0 {
				t.Fatalf("posts=%d", api.posts)
			}
			if !tc.wantErr {
				// A fresh Publisher finds the bot's marker even for a new fingerprint.
				p := &Publisher{store: f.db, gitlab: api, retries: 2}
				if err := p.publishStandaloneSummaryLocked(context.Background(), 42, 7, "token", "another", review.SummaryMarkerPrefix+"updated"); err != nil {
					t.Fatal(err)
				}
				if api.posts != 1 || f.gitlab.notes[1].Body != review.SummaryMarkerPrefix+"updated" {
					t.Fatalf("posts=%d notes=%+v", api.posts, f.gitlab.notes)
				}
			}
		})
	}
}

func TestSummaryCreateStopsOnPermanentStatusAndCancellation(t *testing.T) {
	for _, mode := range []string{"400", "404"} {
		t.Run(mode, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			api := &uncertainSummaryGitLab{reviewGitLab: f.gitlab, mode: mode}
			f.publisher.gitlab, f.publisher.retries = api, 3
			err := f.publisher.publishStandaloneSummaryLocked(context.Background(), 42, 7, "token", "hash", review.SummaryMarkerPrefix+"body")
			if err == nil || api.posts != 1 || api.scans != 1 {
				t.Fatalf("err=%v posts=%d scans=%d", err, api.posts, api.scans)
			}
		})
	}
	f := newReviewFixture(t, reviewTestHead)
	api := &uncertainSummaryGitLab{reviewGitLab: f.gitlab, mode: "not-saved"}
	f.publisher.gitlab, f.publisher.retries = api, 3
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := f.publisher.publishStandaloneSummaryLocked(ctx, 42, 7, "token", "hash", review.SummaryMarkerPrefix+"body")
	if !errors.Is(err, context.Canceled) || api.posts > 1 {
		t.Fatalf("err=%v posts=%d", err, api.posts)
	}
}

type pagedSummaryGitLab struct {
	*reviewGitLab
	client *gitlab.Client
}

func (g *pagedSummaryGitLab) ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error) {
	return g.client.ListMRNotes(ctx, token, projectID, iid)
}

func TestSummaryMarkerOnLaterGitLabPage(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.notes = []gitlab.Note{{ID: 77, AuthorID: 9, Body: review.SummaryMarkerPrefix + "old"}}
	pages := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pages++
		if r.URL.Path != "/api/v4/projects/42/merge_requests/7/notes" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"id":76,"body":"ordinary","author":{"id":9}}]`)
			return
		}
		fmt.Fprintf(w, `[{"id":77,"body":%q,"author":{"id":9}}]`, review.SummaryMarkerPrefix+"old")
	}))
	defer server.Close()
	client, err := gitlab.New(gitlab.Config{BaseURL: server.URL, RequestTimeout: 2 * time.Second, MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	f.publisher.gitlab = &pagedSummaryGitLab{reviewGitLab: f.gitlab, client: client}
	if err := f.publisher.publishStandaloneSummaryLocked(context.Background(), 42, 7, "token", "hash", review.SummaryMarkerPrefix+"new"); err != nil {
		t.Fatal(err)
	}
	if pages != 2 || f.gitlab.summaryCreates != 0 || f.gitlab.notes[0].Body != review.SummaryMarkerPrefix+"new" {
		t.Fatalf("pages=%d creates=%d notes=%+v", pages, f.gitlab.summaryCreates, f.gitlab.notes)
	}
}

type failSummaryStore struct {
	ReviewStore
	fail bool
}

func (s *failSummaryStore) UpsertPublication(ctx context.Context, p store.Publication) (*store.Publication, error) {
	if s.fail && p.Kind == summaryPublishKind {
		s.fail = false
		return nil, errSummaryTransport
	}
	return s.ReviewStore.UpsertPublication(ctx, p)
}

func TestSummaryUncertainCreatePublicationPaths(t *testing.T) {
	for _, failure := range []bool{false, true} {
		name := "normal"
		if failure {
			name = "failure"
		}
		t.Run(name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			if failure {
				f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
			}
			api := &uncertainSummaryGitLab{reviewGitLab: f.gitlab, mode: "saved"}
			f.publisher.gitlab, f.publisher.retries = api, 2
			job := f.seedReview(t, "job-summary-uncertain")
			if err := f.service.RunReview(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			if api.posts != 1 || countPublicationEffects(f).summaries != 1 {
				t.Fatalf("posts=%d summaries=%d", api.posts, countPublicationEffects(f).summaries)
			}
			pub, err := f.db.GetPublication(context.Background(), 42, 7, summaryPublishKind, summaryPublicationKey)
			if err != nil || pub.WorkflowID != wf.ID || !strings.EqualFold(pub.Kind, summaryPublishKind) {
				t.Fatalf("publication=%+v err=%v", pub, err)
			}
		})
	}
}

func TestSummaryPublicationStoreFailureRecovers(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	wf := reviewWithFailedNote(t, f, "job-summary-store-failure")
	api := &uncertainSummaryGitLab{reviewGitLab: f.gitlab}
	f.publisher.gitlab = api
	f.publisher.store = &failSummaryStore{ReviewStore: f.db, fail: true}
	if err := f.publisher.Publish(context.Background(), wf.ID); err == nil {
		t.Fatal("expected store error")
	}
	if err := f.publisher.Publish(context.Background(), wf.ID); err != nil {
		t.Fatal(err)
	}
	if api.posts != 1 || countPublicationEffects(f).summaries != 1 {
		t.Fatalf("posts=%d summaries=%d", api.posts, countPublicationEffects(f).summaries)
	}
}
