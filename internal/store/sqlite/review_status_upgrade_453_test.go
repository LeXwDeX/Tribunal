package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

// openLegacyStore builds a database at migration 0031, before final_body_ready
// existed, using the same files the runner would have applied. The status rows
// it inserts are the shapes a pre-#452 deployment leaves behind.
func openLegacyStore(t *testing.T, path string) *sql.DB {
	t.Helper()
	legacy, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatalf("open legacy db: %v", err)
	}
	if _, err := legacy.Exec(`CREATE TABLE schema_migrations (
		version INTEGER PRIMARY KEY,
		name TEXT NOT NULL,
		applied_at TEXT NOT NULL
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read migrations: %v", err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			t.Fatalf("migration version %s: %v", name, err)
		}
		if version > 31 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at)
			VALUES (?, ?, '2026-11-13T10:00:00.000000000Z')`, version, name); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	return legacy
}

func TestFinalBodyReadyMigrationKeepsHistoricalRows(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	legacy := openLegacyStore(t, path)
	head := reviewHead("a")
	stamp := "2026-11-13T10:00:00.000000000Z"
	insert := `INSERT INTO review_status
		(project_id, mr_iid, generation, head_sha, attempt, profile, state, detail,
		 final_body, note_id, body_hash, version, applied_version, last_error, updated_at)
		VALUES (?, ?, 3, ?, 0, 'default', ?, '', ?, ?, '', 2, 2, '', ?)`
	if _, err := legacy.Exec(insert, 42, 7, head, "PASS", "", nil, stamp); err != nil {
		t.Fatalf("insert status-only row: %v", err)
	}
	if _, err := legacy.Exec(insert, 42, 8, head, "PASS", "published summary", 901, stamp); err != nil {
		t.Fatalf("insert published row: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open upgraded db: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	historical := mustReviewStatus(t, s, 42, 7)
	if historical.FinalBodyReady {
		t.Fatalf("historical row was marked ready: %+v", historical)
	}
	if historical.FinalBody != "" {
		t.Fatalf("historical final body = %q, want empty", historical.FinalBody)
	}
	published := mustReviewStatus(t, s, 42, 8)
	if !published.FinalBodyReady || published.FinalBody != "published summary" {
		t.Fatalf("published row = %+v, want a ready body", published)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != migrationCount(t) {
		t.Fatalf("schema_migrations = %d, want %d", got, migrationCount(t))
	}
}

func TestEnqueueCommandCancelWithoutReviewKeepsReceipt(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)

	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true,
		Payload: json.RawMessage(`{"trigger":"command","note_id":777}`),
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest cancel: %v", err)
	}
	if result.Skipped || result.JobID == "" {
		t.Fatalf("command cancel = %+v, want the authenticated receipt path", result)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.State != store.ReviewStatusQueued || status.Generation != result.Generation {
		t.Fatalf("command cancel status = %+v", status)
	}
}

func TestWebhookCancelAfterTerminalStatusWithoutBodySkips(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(ctx, "worker", []string{reviewRunKind}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim run: %+v, %v", claimed, err)
	}
	if err := s.Complete(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: run.Generation, HeadSHA: head, State: store.ReviewStatusFailed,
	}); err != nil {
		t.Fatal(err)
	}

	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true, Payload: json.RawMessage(`{"trigger":"webhook"}`),
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest cancel: %v", err)
	}
	if !result.Skipped {
		t.Fatalf("cancel after a terminal status = %+v, want skipped", result)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.State != store.ReviewStatusFailed || status.Generation != run.Generation {
		t.Fatalf("cancel changed the terminal status: %+v", status)
	}
}

func TestWebhookCancelWithPendingWorkflowCancels(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(run.JobID, run.Generation, 42, 7, head)); err != nil {
		t.Fatalf("CreateReviewWorkflow: %v", err)
	}
	// The run job is gone, but the pending workflow still owns the MR.
	claimed, err := s.Claim(ctx, "worker", []string{reviewRunKind}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim run: %+v, %v", claimed, err)
	}
	if err := s.Complete(ctx, *claimed); err != nil {
		t.Fatal(err)
	}

	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true, Payload: json.RawMessage(`{"trigger":"webhook"}`),
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest cancel: %v", err)
	}
	if result.Skipped || result.JobID == "" {
		t.Fatalf("cancel with a pending workflow = %+v, want an accepted cancel", result)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !control.Cancelled || control.Generation != result.Generation {
		t.Fatalf("control = %+v", control)
	}
}

func TestSetReviewStatusFinalBodyMarksRowReady(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	written, err := s.SetReviewStatusFinalBody(ctx, 42, 7, run.Generation, head, "rendered summary")
	if err != nil || !written {
		t.Fatalf("SetReviewStatusFinalBody = %v, %v", written, err)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.FinalBody != "rendered summary" || !status.FinalBodyReady {
		t.Fatalf("status = %+v, want a ready final body", status)
	}
}

func TestSetReviewStatusFinalBodyRejectsSupersededGeneration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	first := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: first})
	if err != nil {
		t.Fatal(err)
	}
	second := reviewHead("b")
	successor, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: second})
	if err != nil {
		t.Fatal(err)
	}
	if successor.Generation <= run.Generation {
		t.Fatalf("successor generation did not advance: %d", successor.Generation)
	}

	written, err := s.SetReviewStatusFinalBody(ctx, 42, 7, run.Generation, first, "old generation summary")
	if written || !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("stale final body = %v, %v; want ErrSuperseded", written, err)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.FinalBody != "" || status.Generation != successor.Generation {
		t.Fatalf("superseded body reached the successor row: %+v", status)
	}

	written, err = s.SetReviewStatusFinalBody(ctx, 42, 7, successor.Generation, first, "old head summary")
	if written || !errors.Is(err, store.ErrSuperseded) {
		t.Fatalf("stale head final body = %v, %v; want ErrSuperseded", written, err)
	}
	if status := mustReviewStatus(t, s, 42, 7); status.FinalBody != "" {
		t.Fatalf("superseded head wrote a body: %+v", status)
	}
}

func TestSetReviewStatusPreservesFinalBodyWithinGeneration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: run.Generation, HeadSHA: head, State: store.ReviewStatusPass,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetReviewStatusFinalBody(ctx, 42, 7, run.Generation, head, "published summary"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: run.Generation, Attempt: 1, HeadSHA: head,
		State: store.ReviewStatusStale, Detail: "review is no longer current",
	}); err != nil {
		t.Fatal(err)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.State != store.ReviewStatusStale || status.FinalBody != "published summary" || !status.FinalBodyReady {
		t.Fatalf("stale status cleared the published body: %+v", status)
	}
}

func TestEnqueueWebhookCancelWithoutReviewCreatesNoRows(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)

	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true, Payload: json.RawMessage(`{"trigger":"webhook"}`),
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest cancel: %v", err)
	}
	if !result.Skipped || result.JobID != "" {
		t.Fatalf("cancel on an unreviewed MR = %+v, want skipped", result)
	}
	if _, err := s.GetReviewControl(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel created a control: %v", err)
	}
	if _, err := s.GetReviewStatus(ctx, 42, 7); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel created a status: %v", err)
	}
	if job, err := s.Claim(ctx, "worker", []string{reviewCancelKind}, time.Minute); err != nil || job != nil {
		t.Fatalf("cancel job = %+v, %v; want none", job, err)
	}
}

func TestRecordReviewDeliveryCancelWithoutReviewSkips(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)

	result, err := s.RecordReviewDelivery(ctx, store.Delivery{
		UUID: "close-unreviewed", ProjectID: 42, Event: "Merge Request Hook", Outcome: store.DeliveryEnqueued,
	}, &store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true, Payload: json.RawMessage(`{"trigger":"webhook"}`),
	})
	if err != nil {
		t.Fatalf("RecordReviewDelivery: %v", err)
	}
	if !result.Skipped {
		t.Fatalf("delivery result = %+v, want skipped", result)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_controls WHERE project_id = 42`); got != 0 {
		t.Fatalf("control rows = %d, want none", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM review_status WHERE project_id = 42`); got != 0 {
		t.Fatalf("status rows = %d, want none", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM webhook_deliveries WHERE project_id = 42`); got != 0 {
		t.Fatalf("delivery rows = %d, want the skipped delivery rolled back", got)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM jobs WHERE kind = 'review.cancel'`); got != 0 {
		t.Fatalf("cancel jobs = %d, want none", got)
	}
}

func TestWebhookCancelAfterPublishedReviewKeepsResult(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := s.Claim(ctx, "worker", []string{reviewRunKind}, time.Minute)
	if err != nil || claimed == nil {
		t.Fatalf("claim run: %+v, %v", claimed, err)
	}
	if err := s.Complete(ctx, *claimed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetReviewStatus(ctx, store.ReviewStatusGoal{
		ProjectID: 42, MRIID: 7, Generation: run.Generation, HeadSHA: head, State: store.ReviewStatusPass,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetReviewStatusFinalBody(ctx, 42, 7, run.Generation, head, "published summary"); err != nil {
		t.Fatal(err)
	}
	before := mustReviewStatus(t, s, 42, 7)

	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true, Payload: json.RawMessage(`{"trigger":"webhook"}`),
	})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest cancel: %v", err)
	}
	if !result.Skipped {
		t.Fatalf("cancel after a published review = %+v, want skipped", result)
	}
	after := mustReviewStatus(t, s, 42, 7)
	if after.Generation != before.Generation || after.State != before.State ||
		after.FinalBody != before.FinalBody || after.FinalBodyReady != before.FinalBodyReady {
		t.Fatalf("cancel changed a published status: before %+v after %+v", before, after)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if control.Generation != run.Generation || control.Cancelled {
		t.Fatalf("cancel changed the published control: %+v", control)
	}
	if job, err := s.Claim(ctx, "worker", []string{reviewCancelKind}, time.Minute); err != nil || job != nil {
		t.Fatalf("cancel job = %+v, %v; want none", job, err)
	}
}

func TestEnqueueCancelWhileReviewQueuedAdvancesGeneration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}

	result, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, Cancel: true})
	if err != nil {
		t.Fatalf("EnqueueReviewLatest cancel: %v", err)
	}
	if result.Skipped || result.JobID == "" || result.Generation <= run.Generation {
		t.Fatalf("queued cancel = %+v, want an accepted successor generation", result)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatal(err)
	}
	if !control.Cancelled || control.Generation != result.Generation {
		t.Fatalf("control = %+v", control)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if status.Generation != result.Generation || status.State != store.ReviewStatusQueued {
		t.Fatalf("status after queued cancel = %+v", status)
	}
	if job, err := s.Claim(ctx, "worker", []string{reviewCancelKind}, time.Minute); err != nil || job == nil {
		t.Fatalf("cancel job = %+v, %v; want one", job, err)
	}
}

func TestNewReviewStatusRowIsReadyForMergedNote(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	run, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	status := mustReviewStatus(t, s, 42, 7)
	if !status.FinalBodyReady {
		t.Fatalf("new row is not ready for the merged note: %+v", status)
	}
	if status.Generation != run.Generation {
		t.Fatalf("new row generation = %d, want %d", status.Generation, run.Generation)
	}
}
