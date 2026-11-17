package app

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
)

// statusCallRecorder stores the observable order of status writes. The receipt
// (durable note id) and the legacy delete are recorded from different layers,
// so the ordering assertion needs one shared log.
type statusCallRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *statusCallRecorder) record(event string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
}

func (r *statusCallRecorder) index(event string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i, got := range r.events {
		if got == event {
			return i
		}
	}
	return -1
}

// recordingStatusGitLab logs note mutations before delegating to the fake.
type recordingStatusGitLab struct {
	*reviewGitLab
	rec *statusCallRecorder
}

func (g *recordingStatusGitLab) CreateMRNote(ctx context.Context, token string, projectID, mrIID int64, body string) (*gitlab.Note, error) {
	note, err := g.reviewGitLab.CreateMRNote(ctx, token, projectID, mrIID, body)
	if note != nil {
		g.rec.record("create:" + strconv.FormatInt(note.ID, 10))
	}
	return note, err
}

func (g *recordingStatusGitLab) UpdateMRNote(ctx context.Context, token string, projectID, mrIID, noteID int64, body string) error {
	g.rec.record("update:" + strconv.FormatInt(noteID, 10))
	return g.reviewGitLab.UpdateMRNote(ctx, token, projectID, mrIID, noteID, body)
}

func (g *recordingStatusGitLab) DeleteMRNote(ctx context.Context, token string, projectID, mrIID, noteID int64) error {
	g.rec.record("delete:" + strconv.FormatInt(noteID, 10))
	return g.reviewGitLab.DeleteMRNote(ctx, token, projectID, mrIID, noteID)
}

// recordingStatusStore logs the note-id receipt write.
type recordingStatusStore struct {
	*sqlite.Store
	rec *statusCallRecorder
}

func (s *recordingStatusStore) RecordReviewStatusNoteID(ctx context.Context, projectID, mrIID, version, noteID int64, bodyHash string) error {
	s.rec.record("receipt:" + strconv.FormatInt(noteID, 10))
	return s.Store.RecordReviewStatusNoteID(ctx, projectID, mrIID, version, noteID, bodyHash)
}

// seedConvertedLegacyRow publishes the final body on a queued row and leaves a
// pre-upgrade legacy status note behind: the visit that converges the row must
// both take over the merged note and remove the legacy note.
func seedConvertedLegacyRow(t *testing.T, f *reviewFixture) (int64, int64) {
	t.Helper()
	ctx := context.Background()
	run, err := f.db.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: f.head,
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	body := review.SummaryMarkerPrefix + " v1 fp=published -->\n\n## 评审结论\n\nPASS\n"
	if _, err := f.db.SetReviewStatusFinalBody(ctx, 42, 7, run.Generation, f.head, body); err != nil {
		t.Fatalf("SetReviewStatusFinalBody: %v", err)
	}
	const legacyID = 701
	f.gitlab.mu.Lock()
	f.gitlab.notes = append(f.gitlab.notes, gitlab.Note{
		ID: legacyID, AuthorID: f.gitlab.currentUser.ID,
		Body: legacyStatusMarkerPrefix + " v1 -->\n\n**AI 评审：评审运行中**\n",
	})
	f.gitlab.mu.Unlock()
	return run.Generation, legacyID
}

func mergedStatusNote(t *testing.T, notes []gitlab.Note) gitlab.Note {
	t.Helper()
	for _, note := range notes {
		if strings.HasPrefix(note.Body, reviewStatusMarkerPrefix) {
			return note
		}
	}
	t.Fatalf("merged status note is missing: %+v", notes)
	return gitlab.Note{}
}

// TestStatusVisitRecordsMergedNoteIDBeforeLegacyDelete pins the ordering that
// closes the permalink window: the durable note id must name the merged note
// before the legacy note is deleted (#497).
func TestStatusVisitRecordsMergedNoteIDBeforeLegacyDelete(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	rec := &statusCallRecorder{}
	f.service.status = newStatusReporter(&recordingStatusGitLab{reviewGitLab: f.gitlab, rec: rec}, nil)
	f.service.store = &recordingStatusStore{Store: f.db, rec: rec}
	_, legacyID := seedConvertedLegacyRow(t, f)

	drainReviewJobs(t, f, "review.status")

	notes := f.gitlab.noteSnapshot()
	merged := mergedStatusNote(t, notes)
	if len(notes) != 1 || merged.ID == legacyID {
		t.Fatalf("notes = %+v, want the merged review note only", notes)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.NoteID != merged.ID {
		t.Fatalf("status note id = %d, merged note = %d", row.NoteID, merged.ID)
	}
	if row.AppliedVersion != row.Version {
		t.Fatalf("converged visit was not applied: %+v", row)
	}
	receipt := rec.index("receipt:" + strconv.FormatInt(merged.ID, 10))
	del := rec.index("delete:" + strconv.FormatInt(legacyID, 10))
	if receipt < 0 || del < 0 || receipt > del {
		t.Fatalf("status writes = %v, want the merged note id receipt before the legacy delete", rec.events)
	}
	written := rec.index("create:" + strconv.FormatInt(merged.ID, 10))
	if written >= 0 && written > receipt {
		t.Fatalf("status writes = %v, want the merged note write before its receipt", rec.events)
	}
}

// TestLegacyCleanupFailureKeepsNoteAndPendingVisit proves a failed legacy
// delete leaves the legacy note in place and the visit unapplied, so a later
// visit retries both the note write and the cleanup (#497).
func TestLegacyCleanupFailureKeepsNoteAndPendingVisit(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.service.status = testStatusReporter(f.gitlab)
	_, legacyID := seedConvertedLegacyRow(t, f)
	f.gitlab.noteDeleteErr = errors.New("delete unavailable")

	job, err := f.db.Claim(ctx, "legacy-cleanup-test", []string{"review.status"}, time.Minute)
	if err != nil || job == nil {
		t.Fatalf("claim review.status = %+v, %v", job, err)
	}
	if err := f.service.Handle(ctx, job); err == nil {
		t.Fatal("legacy cleanup failure settled the status visit")
	}

	notes := f.gitlab.noteSnapshot()
	merged := mergedStatusNote(t, notes)
	if noteByID(notes, legacyID) == nil {
		t.Fatalf("legacy note was removed despite the failed delete: %+v", notes)
	}
	row, err := f.db.GetReviewStatus(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if row.AppliedVersion >= row.Version {
		t.Fatalf("failed cleanup advanced the applied version: %+v", row)
	}
	if row.NoteID != merged.ID {
		t.Fatalf("status note id = %d, merged note = %d; the receipt must not point at the legacy note", row.NoteID, merged.ID)
	}
}
