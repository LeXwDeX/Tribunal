package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func TestReservedDossierOwnerAndProjectDeletion(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	head := reviewHead("a")
	job, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	id, err := s.reserveReviewDossier(ctx, reviewWorkflowRequest(job.JobID, 1, 42, 7, head), s.now())
	if err != nil {
		t.Fatal(err)
	}
	var owner int64
	if err := s.db.QueryRow(`SELECT project_id FROM workflow_dossiers WHERE workflow_id = ?`, id).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != 42 {
		t.Fatalf("reserved owner = %d", owner)
	}
	insertCleanupDossier(t, s, "unknown-orphan", nil, store.WorkflowPublicationPending)
	insertCleanupDossier(t, s, "other-orphan", nil, store.WorkflowPublicationPending)
	if _, err := s.db.Exec(`UPDATE workflow_dossiers SET project_id = 43 WHERE workflow_id = 'other-orphan'`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(ctx, 42); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWorkflowDossier(ctx, id); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deleted orphan: %v", err)
	}
	for _, keep := range []string{"unknown-orphan", "other-orphan"} {
		if n := rowCount(t, s, `SELECT count(*) FROM workflow_dossiers WHERE workflow_id = ?`, keep); n != 1 {
			t.Fatalf("lost %s", keep)
		}
	}
}

func TestCancelledPendingScannedBeforePublishableBacklog(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	for i := 0; i < 51; i++ {
		id := fmt.Sprintf("completed-%02d", i)
		insertCleanupWorkflow(t, s, id, 42, "completed")
		insertCleanupDossier(t, s, id, nil, store.WorkflowPublicationPending)
	}
	insertCleanupWorkflow(t, s, "zzz-cancelled", 42, "cancelled")
	insertCleanupDossier(t, s, "zzz-cancelled", nil, store.WorkflowPublicationPending)
	items, err := s.ListPendingPublications(ctx, 50)
	if err != nil || len(items) != 50 || items[0].WorkflowID != "zzz-cancelled" {
		t.Fatalf("scan does not prioritize local cancellation: %+v, %v", items, err)
	}
}

func TestDossierOwnerCannotBeClearedOrTransferred(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	d := store.WorkflowDossier{WorkflowID: "owner", ProjectID: 42, Version: 1, Data: json.RawMessage(`{"snapshot":1}`)}
	if _, err := s.SaveWorkflowDossier(ctx, d); err != nil {
		t.Fatal(err)
	}
	d.ProjectID = 0
	got, err := s.SaveWorkflowDossier(ctx, d)
	if err != nil || got.ProjectID != 42 {
		t.Fatalf("owner after omitted update = %+v, %v", got, err)
	}
	d.ProjectID = 43
	if _, err := s.SaveWorkflowDossier(ctx, d); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("transfer: %v", err)
	}
	insertCleanupWorkflow(t, s, "linked", 42, "completed")
	d.WorkflowID, d.ProjectID = "linked", 43
	if _, err := s.SaveWorkflowDossier(ctx, d); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("contradict workflow: %v", err)
	}
	d.ProjectID = 0
	got, err = s.SaveWorkflowDossier(ctx, d)
	if err != nil || got.ProjectID != 42 {
		t.Fatalf("linked owner = %+v, %v", got, err)
	}
}

func TestOrphanDossiersSettleOnlyWhenSafe(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	for _, state := range []string{store.JobQueued, store.JobRunning, store.JobDone, store.JobFailed, store.JobCancelled, "missing"} {
		job := "job-" + state
		if state != "missing" {
			insertJobRow(t, s, job, state, s.now().Add(-48*time.Hour))
		}
		_, err := s.SaveWorkflowDossier(ctx, store.WorkflowDossier{WorkflowID: "wf-" + state, ProjectID: 42, JobID: job, Version: 1, Data: json.RawMessage(`{"exact":"bytes"}`)})
		if err != nil {
			t.Fatal(err)
		}
	}
	// A linked workflow is not an orphan, even if its job is terminal.
	insertCleanupWorkflow(t, s, "linked-terminal", 42, "completed")
	insertJobRow(t, s, "job-linked", store.JobDone, s.now())
	if _, err := s.SaveWorkflowDossier(ctx, store.WorkflowDossier{WorkflowID: "linked-terminal", JobID: "job-linked", Version: 1, Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	insertJobRow(t, s, "job-terminal-present", store.JobFailed, s.now())
	if _, err := s.SaveWorkflowDossier(ctx, store.WorkflowDossier{WorkflowID: "terminal-present", ProjectID: 42, JobID: "job-terminal-present", Version: 1, Data: json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	// Retention may remove a terminal job before the next cleanup pass.
	if _, err := s.DeleteTerminalJobsBefore(ctx, s.now().Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for pass := 0; pass < 2; pass++ {
		if err := s.SettleOrphanDossiers(ctx, 42); err != nil {
			t.Fatal(err)
		}
	}
	refs, err := s.ProtectedRunRefs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []string{store.JobQueued, store.JobRunning, store.JobDone, store.JobFailed, store.JobCancelled, "missing"} {
		d, err := s.GetWorkflowDossier(ctx, "wf-"+state)
		active := state == store.JobQueued || state == store.JobRunning
		want := store.WorkflowPublicationSkipped
		if active {
			want = store.WorkflowPublicationPending
		}
		if err != nil || d.PublicationState != want || string(d.Data) != `{"exact":"bytes"}` {
			t.Fatalf("%s: %+v, %v", state, d, err)
		}
		_, exists := refs["wf-"+state]
		if exists != active {
			t.Fatalf("%s protected = %v", state, exists)
		}
	}
	d, err := s.GetWorkflowDossier(ctx, "linked-terminal")
	if err != nil || d.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("linked = %+v, %v", d, err)
	}
	d, err = s.GetWorkflowDossier(ctx, "terminal-present")
	if err != nil || d.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("terminal = %+v, %v", d, err)
	}
}

func TestDossierProjectMigrationPreservesLegacyBytes(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	db, err := sql.Open("sqlite", withPragmas(path))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE schema_migrations (version INTEGER PRIMARY KEY, name TEXT NOT NULL, applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range names {
		version := migrationVersionForTest(t, name)
		if version >= 18 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := db.Exec(`INSERT INTO schema_migrations VALUES (?, ?, '2026-10-22T12:00:00Z')`, version, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO projects (id,path_with_namespace,web_url,default_branch,token_enc,webhook_secret_enc,created_at,updated_at) VALUES (42,'g/p','https://example.test/g/p','main',x'01',x'02','2026-10-22T12:00:00Z','2026-10-22T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO workflows (id,kind,project_id,trigger,status,created_at) VALUES ('legacy-linked','review',42,'api','completed','2026-10-22T12:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	compressed, err := compressDossier([]byte(`{"project":{"id":999},"old":true}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"legacy-linked", "legacy-orphan"} {
		if _, err := db.Exec(`INSERT INTO workflow_dossiers (workflow_id,version,hash,data,created_at,updated_at) VALUES (?,1,'hash',?,'2026-10-22T12:00:00Z','2026-10-22T12:00:00Z')`, id, compressed); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != migrationCount(t) {
		t.Fatalf("migration count = %d", got)
	}
	for id, want := range map[string]int64{"legacy-linked": 42, "legacy-orphan": 0} {
		var owner int64
		var actual []byte
		if err := s.db.QueryRow(`SELECT project_id,data FROM workflow_dossiers WHERE workflow_id = ?`, id).Scan(&owner, &actual); err != nil {
			t.Fatal(err)
		}
		if owner != want || !bytes.Equal(actual, compressed) {
			t.Fatalf("%s owner=%d compressed changed=%v", id, owner, !bytes.Equal(actual, compressed))
		}
	}
}

func TestAppendFailureReusesReservedDossier(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	head := reviewHead("a")
	job, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER reject_created BEFORE INSERT ON events BEGIN SELECT RAISE(ABORT, 'injected'); END`); err != nil {
		t.Fatal(err)
	}
	req := reviewWorkflowRequest(job.JobID, 1, 42, 7, head)
	if _, _, err := s.CreateReviewWorkflow(ctx, req); err == nil {
		t.Fatal("append unexpectedly succeeded")
	}
	id, err := s.dossierWorkflowIDByJob(ctx, job.JobID)
	if err != nil || id == "" {
		t.Fatalf("reservation = %q, %v", id, err)
	}
	if err := s.SettleOrphanDossiers(ctx, 42); err != nil {
		t.Fatal(err)
	}
	d, err := s.GetWorkflowDossier(ctx, id)
	if err != nil || d.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("active reservation = %+v, %v", d, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER reject_created`); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.CreateReviewWorkflow(ctx, req)
	if err != nil || got != id {
		t.Fatalf("retry id=%q want %q: %v", got, id, err)
	}
}

func TestCreateReviewWorkflowDoesNotReuseAnotherProjectsJob(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())
	mustProject(t, s, 42)
	mustProject(t, s, 43)
	head := reviewHead("a")
	first, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 42, MRIID: 7, HeadSHA: head})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(first.JobID, 1, 42, 7, head)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.EnqueueReviewLatest(ctx, store.ReviewCommand{ProjectID: 43, MRIID: 7, HeadSHA: head}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateReviewWorkflow(ctx, reviewWorkflowRequest(first.JobID, 1, 43, 7, head)); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("cross-project workflow reuse error = %v", err)
	}
}
