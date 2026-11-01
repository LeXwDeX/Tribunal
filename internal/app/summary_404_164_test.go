package app

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

type deletedSummary164GitLab struct {
	*reviewGitLab
	updates          []int64
	scans, creates   int
	updateErr        error
	identityErr      error
	scanErr          error
	createErr        error
	onMissing        func()
	onScan           func()
	uncertainCreate  bool
	deleteOnPosition bool
	deletedBody      string
}

func (g *deletedSummary164GitLab) CurrentUser(ctx context.Context, token string) (*gitlab.User, error) {
	if g.identityErr != nil {
		return nil, g.identityErr
	}
	return g.reviewGitLab.CurrentUser(ctx, token)
}

func (g *deletedSummary164GitLab) ListMRNotes(ctx context.Context, token string, projectID, iid int64) ([]gitlab.Note, error) {
	g.scans++
	if g.scanErr != nil {
		return nil, g.scanErr
	}
	notes, err := g.reviewGitLab.ListMRNotes(ctx, token, projectID, iid)
	if g.onScan != nil {
		g.onScan()
	}
	return notes, err
}

func (g *deletedSummary164GitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	g.updates = append(g.updates, noteID)
	err := g.updateErr
	if err == nil {
		err = g.reviewGitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
	}
	if gitlab.IsNotFound(err) && g.onMissing != nil {
		g.onMissing()
	}
	return err
}

func (g *deletedSummary164GitLab) CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error) {
	g.creates++
	if g.createErr != nil {
		return nil, g.createErr
	}
	note, err := g.reviewGitLab.CreateMRNote(ctx, token, projectID, iid, body)
	if err == nil && g.uncertainCreate && g.creates == 1 {
		return nil, errSummaryTransport
	}
	return note, err
}

func (g *deletedSummary164GitLab) CreateDiscussion(ctx context.Context, token string, projectID, iid int64, d gitlab.NewDiscussion) (*gitlab.Discussion, error) {
	if !g.deleteOnPosition {
		return g.reviewGitLab.CreateDiscussion(ctx, token, projectID, iid, d)
	}
	g.reviewGitLab.mu.Lock()
	for i, note := range g.reviewGitLab.notes {
		if strings.HasPrefix(note.Body, review.SummaryMarkerPrefix) {
			g.deletedBody = note.Body
			g.reviewGitLab.notes = append(g.reviewGitLab.notes[:i], g.reviewGitLab.notes[i+1:]...)
			break
		}
	}
	g.reviewGitLab.mu.Unlock()
	return nil, &gitlab.Error{Op: "create_discussion", Status: 400}
}

func publishedSummary164(t *testing.T, failure bool) (*reviewFixture, *store.Workflow, *store.Publication, gitlab.Note) {
	t.Helper()
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	if failure {
		f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
			return nil, agent.ErrNoSubmit
		}
	}
	job := f.seedReview(t, "summary-164")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("run review: %v", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := f.db.GetPublication(ctx, 42, 7, summaryPublishKind, summaryPublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	id, err := strconv.ParseInt(pub.GitlabID, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	for _, note := range f.gitlab.notes {
		if note.ID == id {
			return f, wf, pub, note
		}
	}
	t.Fatalf("stored summary note %d not found", id)
	return nil, nil, nil, gitlab.Note{}
}

func deleteSummary164(t *testing.T, f *reviewFixture, id int64) {
	t.Helper()
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	for i, note := range f.gitlab.notes {
		if note.ID == id {
			f.gitlab.notes = append(f.gitlab.notes[:i], f.gitlab.notes[i+1:]...)
			return
		}
	}
	t.Fatalf("note %d not found for deletion", id)
}

func summaryPublication164(t *testing.T, f *reviewFixture) *store.Publication {
	t.Helper()
	pub, err := f.db.GetPublication(context.Background(), 42, 7, summaryPublishKind, summaryPublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	return pub
}

func TestSummaryDeleted164Recovers(t *testing.T) {
	for _, tc := range []struct {
		name      string
		failure   bool
		marker    string
		uncertain bool
	}{
		{name: "missing normal note"},
		{name: "missing failure note", failure: true},
		{name: "existing bot marker", marker: "old"},
		{name: "existing identical bot marker", marker: "identical"},
		{name: "create persisted but response lost", uncertain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			f, wf, before, deleted := publishedSummary164(t, tc.failure)
			deleteSummary164(t, f, deleted.ID)
			foreign := gitlab.Note{ID: 301, AuthorID: 10, Body: review.SummaryMarkerPrefix + "foreign"}
			f.gitlab.notes = append(f.gitlab.notes, foreign)
			if tc.marker != "" {
				body := review.SummaryMarkerPrefix + "old"
				if tc.marker == "identical" {
					body = deleted.Body
				}
				f.gitlab.notes = append(f.gitlab.notes, gitlab.Note{ID: 302, AuthorID: 9, Body: body})
			}
			api := &deletedSummary164GitLab{reviewGitLab: f.gitlab, uncertainCreate: tc.uncertain}
			f.publisher.gitlab, f.publisher.retries = api, 2
			if err := f.publisher.Republish(ctx, wf.ID); err != nil {
				t.Fatalf("republish after deletion: %v", err)
			}
			pub := summaryPublication164(t, f)
			if pub.ID != before.ID || pub.GitlabID == before.GitlabID || pub.WorkflowID != wf.ID || pub.Fingerprint != before.Fingerprint {
				t.Fatalf("summary binding: before=%+v after=%+v", before, pub)
			}
			if f.gitlab.notes[0] != foreign {
				t.Fatalf("foreign note was changed: %+v", f.gitlab.notes[0])
			}
			var botNotes []gitlab.Note
			for _, note := range f.gitlab.notes {
				if note.AuthorID == 9 && strings.HasPrefix(note.Body, review.SummaryMarkerPrefix) {
					botNotes = append(botNotes, note)
				}
			}
			if len(botNotes) != 1 || pub.GitlabID != strconv.FormatInt(botNotes[0].ID, 10) || pub.BodyHash != bodyHash(botNotes[0].Body) {
				t.Fatalf("summary binding=%+v bot notes=%+v", pub, botNotes)
			}
			if (!tc.failure && botNotes[0].Body != deleted.Body) || (tc.failure && !strings.Contains(botNotes[0].Body, "review execution failed")) {
				t.Fatalf("recovered summary has the wrong body: %q", botNotes[0].Body)
			}
			wantCreates := 1
			if tc.marker != "" {
				wantCreates = 0
				if botNotes[0].ID != 302 {
					t.Fatalf("did not reuse bot marker: %+v", botNotes)
				}
			}
			wantScans := 1
			if tc.uncertain {
				wantScans = 2
			}
			if api.creates != wantCreates || api.scans != wantScans || len(api.updates) == 0 || api.updates[0] != deleted.ID {
				t.Fatalf("creates=%d scans=%d updates=%v", api.creates, api.scans, api.updates)
			}
			wantUpdates := 1
			if tc.marker == "old" {
				wantUpdates = 2
			}
			if len(api.updates) != wantUpdates {
				t.Fatalf("updates=%v want %d", api.updates, wantUpdates)
			}
			if err := f.publisher.Republish(ctx, wf.ID); err != nil {
				t.Fatalf("repeat republish: %v", err)
			}
			if api.creates != wantCreates || api.scans != wantScans || summaryPublication164(t, f).GitlabID != pub.GitlabID {
				t.Fatalf("repeat created or scanned again: creates=%d scans=%d", api.creates, api.scans)
			}
			if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationPublished {
				t.Fatalf("publication state = %q", got)
			}
		})
	}
}

func TestSummaryDeleted164RecoveryErrors(t *testing.T) {
	for _, tc := range []struct {
		name        string
		updateErr   error
		identityErr error
		scanErr     error
		createErr   error
		wantScans   int
		wantCreates int
	}{
		{name: "bad request", updateErr: &gitlab.Error{Op: "update_note", Status: 400}},
		{name: "forbidden", updateErr: &gitlab.Error{Op: "update_note", Status: 403}},
		{name: "server failure", updateErr: &gitlab.Error{Op: "update_note", Status: 503}},
		{name: "transport failure", updateErr: errors.New("transport " + publicationSecret)},
		{name: "identity failure", identityErr: errSummaryTransport},
		{name: "scan failure", scanErr: errSummaryTransport, wantScans: 1},
		{name: "create 404", createErr: &gitlab.Error{Op: "create_note", Status: 404}, wantScans: 1, wantCreates: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, wf, before, deleted := publishedSummary164(t, false)
			deleteSummary164(t, f, deleted.ID)
			api := &deletedSummary164GitLab{reviewGitLab: f.gitlab, updateErr: tc.updateErr, identityErr: tc.identityErr, scanErr: tc.scanErr, createErr: tc.createErr}
			f.publisher.gitlab, f.publisher.retries = api, 1
			var logs bytes.Buffer
			f.publisher.logger = slog.New(slog.NewTextHandler(&logs, nil))
			wantErr := tc.updateErr
			for _, candidate := range []error{tc.identityErr, tc.scanErr, tc.createErr} {
				if candidate != nil {
					wantErr = candidate
				}
			}
			if err := f.publisher.Republish(context.Background(), wf.ID); !errors.Is(err, wantErr) {
				t.Fatalf("wrong recovery error for %s", tc.name)
			}
			after := summaryPublication164(t, f)
			if *after != *before || api.scans != tc.wantScans || api.creates != tc.wantCreates || len(api.updates) != 1 {
				t.Fatalf("binding changed or unexpected calls: before=%+v after=%+v scans=%d creates=%d updates=%v", before, after, api.scans, api.creates, api.updates)
			}
			if wf := workflowPublication(t, f, wf.ID); wf.PublicationState != store.WorkflowPublicationPending || wf.PublicationError != publicationRetryReason {
				t.Fatalf("publication state = %q/%q", wf.PublicationState, wf.PublicationError)
			}
			assertNoSecret(t, logs.String())
		})
	}
}

func TestSummaryDeleted164StaleDuringRecovery(t *testing.T) {
	for _, tc := range []struct {
		name   string
		marker string
		atScan bool
	}{
		{name: "before create"},
		{name: "before marker update", marker: "old"},
		{name: "during scan before create", atScan: true},
		{name: "during scan before marker update", marker: "old", atScan: true},
		{name: "during scan before identical marker adoption", marker: "identical", atScan: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, wf, before, deleted := publishedSummary164(t, false)
			deleteSummary164(t, f, deleted.ID)
			if tc.marker != "" {
				body := review.SummaryMarkerPrefix + "old"
				if tc.marker == "identical" {
					body = deleted.Body
				}
				f.gitlab.notes = append(f.gitlab.notes, gitlab.Note{ID: 302, AuthorID: 9, Body: body})
			}
			api := &deletedSummary164GitLab{reviewGitLab: f.gitlab}
			if tc.atScan {
				api.onScan = mrSerialBumpControl(t, f)
			} else {
				api.onMissing = mrSerialBumpControl(t, f)
			}
			f.publisher.gitlab, f.publisher.retries = api, 1
			err := f.publisher.Republish(context.Background(), wf.ID)
			if !errors.Is(err, ErrStalePublication) {
				t.Fatalf("error=%v, want stale publication", err)
			}
			if after := summaryPublication164(t, f); *after != *before || api.creates != 0 || len(api.updates) != 1 || api.updates[0] != deleted.ID {
				t.Fatalf("stale recovery changed publication: before=%+v after=%+v creates=%d updates=%v", before, after, api.creates, api.updates)
			}
			if got := workflowPublication(t, f, wf.ID).PublicationState; got != store.WorkflowPublicationSkipped {
				t.Fatalf("publication state = %q, want skipped", got)
			}
		})
	}
}

type summary164HeadStore struct {
	ReviewStore
	stale bool
}

func (s *summary164HeadStore) GetReviewControl(ctx context.Context, projectID, mrIID int64) (*store.ReviewControl, error) {
	control, err := s.ReviewStore.GetReviewControl(ctx, projectID, mrIID)
	if err == nil && s.stale {
		copy := *control
		copy.HeadSHA = strings.Repeat("f", 40)
		return &copy, nil
	}
	return control, err
}

func TestSummaryDeleted164HeadChangesDuringScan(t *testing.T) {
	for _, marker := range []string{"missing", "different", "identical"} {
		t.Run(marker, func(t *testing.T) {
			f, wf, before, deleted := publishedSummary164(t, false)
			deleteSummary164(t, f, deleted.ID)
			if marker != "missing" {
				body := review.SummaryMarkerPrefix + "old"
				if marker == "identical" {
					body = deleted.Body
				}
				f.gitlab.notes = append(f.gitlab.notes, gitlab.Note{ID: 302, AuthorID: 9, Body: body})
			}
			st := &summary164HeadStore{ReviewStore: f.db}
			api := &deletedSummary164GitLab{reviewGitLab: f.gitlab, onScan: func() { st.stale = true }}
			f.publisher.gitlab, f.publisher.store, f.publisher.retries = api, st, 1
			err := f.publisher.Republish(context.Background(), wf.ID)
			if !errors.Is(err, ErrStalePublication) {
				t.Fatalf("error=%v, want stale publication", err)
			}
			if after := summaryPublication164(t, f); *after != *before || api.creates != 0 || len(api.updates) != 1 {
				t.Fatalf("head change did not stop recovery: before=%+v after=%+v creates=%d updates=%v", before, after, api.creates, api.updates)
			}
		})
	}
}

func TestSummaryDeleted164Recheck404IsNotNote404(t *testing.T) {
	f, wf, before, _ := publishedSummary164(t, false)
	api := &deletedSummary164GitLab{reviewGitLab: f.gitlab}
	f.publisher.gitlab, f.publisher.retries = api, 1
	checks := 0
	recheck := func() error {
		checks++
		if checks == 2 {
			return &gitlab.Error{Op: "control_check", Status: 404}
		}
		return nil
	}
	_, err := f.publisher.upsertSummary(context.Background(), 42, 7, "token", before.Fingerprint, review.SummaryMarkerPrefix+"new", wf.ID, recheck)
	if !gitlab.IsNotFound(err) || api.scans != 0 || api.creates != 0 || len(api.updates) != 0 {
		t.Fatalf("recheck failure was treated as a missing note: error=%v scans=%d creates=%d updates=%v", err, api.scans, api.creates, api.updates)
	}
	if after := summaryPublication164(t, f); *after != *before {
		t.Fatalf("recheck failure changed binding: before=%+v after=%+v", before, after)
	}
}

func TestSummaryDeleted164StoreFailureRecoversWithoutDuplicate(t *testing.T) {
	f, wf, before, deleted := publishedSummary164(t, false)
	deleteSummary164(t, f, deleted.ID)
	api := &deletedSummary164GitLab{reviewGitLab: f.gitlab}
	f.publisher.gitlab, f.publisher.store, f.publisher.retries = api, &failSummaryStore{ReviewStore: f.db, fail: true}, 1
	if err := f.publisher.Republish(context.Background(), wf.ID); !errors.Is(err, errSummaryTransport) {
		t.Fatalf("first republish error = %v, want store error", err)
	}
	if after := summaryPublication164(t, f); *after != *before || api.creates != 1 {
		t.Fatalf("failed store changed binding: before=%+v after=%+v creates=%d", before, after, api.creates)
	}
	if err := f.publisher.Republish(context.Background(), wf.ID); err != nil {
		t.Fatalf("retry after store failure: %v", err)
	}
	pub := summaryPublication164(t, f)
	if api.creates != 1 || api.scans != 2 || len(f.gitlab.notes) != 1 || pub.GitlabID != strconv.FormatInt(f.gitlab.notes[0].ID, 10) || pub.BodyHash != bodyHash(f.gitlab.notes[0].Body) {
		t.Fatalf("retry did not reuse created note: binding=%+v notes=%+v creates=%d scans=%d", pub, f.gitlab.notes, api.creates, api.scans)
	}
}

type summary164RetryGitLab struct {
	*deletedSummary164GitLab
	mode      string
	missingID int64
	bump      func()
}

func (g *summary164RetryGitLab) UpdateMRNote(ctx context.Context, token string, projectID, iid, noteID int64, body string) error {
	if g.mode == "update" && noteID != g.missingID {
		g.updates = append(g.updates, noteID)
		g.bump()
		return errSummaryTransport
	}
	return g.deletedSummary164GitLab.UpdateMRNote(ctx, token, projectID, iid, noteID, body)
}

func (g *summary164RetryGitLab) CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error) {
	note, err := g.deletedSummary164GitLab.CreateMRNote(ctx, token, projectID, iid, body)
	if g.mode == "create" {
		g.bump()
	}
	return note, err
}

func TestSummaryDeleted164RetryRechecksGeneration(t *testing.T) {
	for _, mode := range []string{"create", "update"} {
		t.Run(mode, func(t *testing.T) {
			f, wf, before, deleted := publishedSummary164(t, false)
			deleteSummary164(t, f, deleted.ID)
			if mode == "update" {
				f.gitlab.notes = append(f.gitlab.notes, gitlab.Note{ID: 302, AuthorID: 9, Body: review.SummaryMarkerPrefix + "old"})
			}
			base := &deletedSummary164GitLab{reviewGitLab: f.gitlab, uncertainCreate: mode == "create"}
			api := &summary164RetryGitLab{deletedSummary164GitLab: base, mode: mode, missingID: deleted.ID, bump: mrSerialBumpControl(t, f)}
			f.publisher.gitlab, f.publisher.retries = api, 2
			err := f.publisher.Republish(context.Background(), wf.ID)
			if !errors.Is(err, ErrStalePublication) {
				t.Fatalf("retry error=%v, want stale publication", err)
			}
			wantCreates, wantUpdates := 1, 1
			if mode == "update" {
				wantCreates, wantUpdates = 0, 2
				if f.gitlab.notes[0].Body != review.SummaryMarkerPrefix+"old" {
					t.Fatalf("stale retry changed marker: %+v", f.gitlab.notes[0])
				}
			}
			if after := summaryPublication164(t, f); *after != *before || api.creates != wantCreates || len(api.updates) != wantUpdates {
				t.Fatalf("retry crossed generation: before=%+v after=%+v creates=%d updates=%v", before, after, api.creates, api.updates)
			}
		})
	}
}

func TestSummaryDeleted164VisibleAfter404DoesNotDuplicate(t *testing.T) {
	f, wf, before, _ := publishedSummary164(t, false)
	api := &deletedSummary164GitLab{reviewGitLab: f.gitlab, updateErr: &gitlab.Error{Op: "update_note", Status: 404}}
	f.publisher.gitlab, f.publisher.retries = api, 1
	if err := f.publisher.Republish(context.Background(), wf.ID); !gitlab.IsNotFound(err) {
		t.Fatalf("error=%v, want original 404", err)
	}
	if after := summaryPublication164(t, f); *after != *before || api.scans != 1 || api.creates != 0 || countPublicationEffects(f).summaries != 1 {
		t.Fatalf("visible note was replaced: before=%+v after=%+v scans=%d creates=%d", before, after, api.scans, api.creates)
	}
}

func TestSummaryDeleted164Visible404StillRechecksGeneration(t *testing.T) {
	f, wf, before, _ := publishedSummary164(t, false)
	api := &deletedSummary164GitLab{
		reviewGitLab: f.gitlab,
		updateErr:    &gitlab.Error{Op: "update_note", Status: 404},
		onScan:       mrSerialBumpControl(t, f),
	}
	f.publisher.gitlab, f.publisher.retries = api, 1
	err := f.publisher.Republish(context.Background(), wf.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("error=%v, want stale publication", err)
	}
	if after := summaryPublication164(t, f); *after != *before || api.creates != 0 {
		t.Fatalf("stale recovery changed binding: before=%+v after=%+v creates=%d", before, after, api.creates)
	}
}

func TestSummaryDeleted164InvalidMarkerNotAccepted(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	control := seedSummaryControl(t, f)
	f.gitlab.notes = []gitlab.Note{{ID: 0, AuthorID: 9, Body: review.SummaryMarkerPrefix + "old"}}
	api := &deletedSummary164GitLab{reviewGitLab: f.gitlab}
	f.publisher.gitlab, f.publisher.retries = api, 1
	err := f.publisher.publishStandaloneSummaryLocked(context.Background(), 42, 7, control.Generation, control.HeadSHA, "token", "hash", review.SummaryMarkerPrefix+"new")
	if err == nil || api.creates != 0 {
		t.Fatalf("invalid marker was accepted or duplicated: error=%v creates=%d", err, api.creates)
	}
}

func TestSummaryDeleted164DuringInlineFallback(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	api := &deletedSummary164GitLab{reviewGitLab: f.gitlab, deleteOnPosition: true}
	f.publisher.gitlab, f.publisher.retries = api, 1
	job := f.seedReview(t, "summary-164-inline-fallback")
	if err := f.service.RunReview(ctx, job); err != nil {
		t.Fatalf("publish inline fallback: %v", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	pub := summaryPublication164(t, f)
	if api.deletedBody == "" || len(f.gitlab.notes) != 1 || f.gitlab.notes[0].Body == api.deletedBody || pub.GitlabID != strconv.FormatInt(f.gitlab.notes[0].ID, 10) || pub.WorkflowID != wf.ID || pub.BodyHash != bodyHash(f.gitlab.notes[0].Body) {
		t.Fatalf("fallback binding=%+v notes=%+v", pub, f.gitlab.notes)
	}
	if api.scans != 2 || api.creates != 2 || len(api.updates) != 1 {
		t.Fatalf("fallback calls: scans=%d creates=%d updates=%v", api.scans, api.creates, api.updates)
	}
}
