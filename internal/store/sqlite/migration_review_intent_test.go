package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

func TestReviewIntentMigrationDefaultsAndPersistence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy.db")
	head := reviewHead("a")

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
	for _, name := range []string{"0001_init.sql", "0003_review_state.sql"} {
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES
		(1, '0001_init.sql', '2026-10-22T12:00:00.000000000Z'),
		(3, '0003_review_state.sql', '2026-10-22T12:00:00.000000000Z')`); err != nil {
		t.Fatalf("record legacy migrations: %v", err)
	}
	stamp := "2026-10-22T10:00:00.000000000Z"
	if _, err := legacy.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc,
		 webhook_secret_enc, created_at, updated_at)
		VALUES (42, 'group/repo', 'https://gitlab.example.com/group/repo', 'main',
		 x'00', x'01', ?, ?)`, stamp, stamp); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO review_controls
		(project_id, mr_iid, generation, head_sha, cancelled, updated_at)
		VALUES (42, 7, 5, ?, 1, ?)`, head, stamp); err != nil {
		t.Fatalf("insert review control: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO jobs
		(id, kind, key, payload, state, attempts, max_attempts, run_after,
		 lease_owner, lease_until, last_error, created_at, updated_at)
		VALUES ('legacy-run', 'review.run', 'review:42:7', ?, 'queued', 0, 3, ?, NULL, NULL, NULL, ?, ?)`,
		`{"project_id":42,"mr_iid":7,"head_sha":"`+head+`","generation":5,"trigger":"mr_open"}`,
		stamp, stamp, stamp); err != nil {
		t.Fatalf("insert review job: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	s, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("open migrated db: %v", err)
	}
	control, err := s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl: %v", err)
	}
	if control.Generation != 5 || control.HeadSHA != head || !control.Cancelled ||
		control.CandidateSeq != 0 || control.BarrierSeq != 0 {
		t.Fatalf("migrated control = %+v", control)
	}
	legacyJob, err := s.GetJob(ctx, "legacy-run")
	if err != nil {
		t.Fatalf("legacy review job: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(legacyJob.Payload, &payload); err != nil {
		t.Fatalf("decode legacy review payload: %v", err)
	}
	if payload["generation"] != float64(5) || payload["head_sha"] != head {
		t.Fatalf("legacy review payload = %v", payload)
	}
	if got := rowCount(t, s, `SELECT COUNT(*) FROM schema_migrations`); got != 11 {
		t.Fatalf("schema_migrations = %d, want 11", got)
	}

	// Candidate allocation persists without reviving the cancelled control.
	if _, err := s.RecordReviewCandidateDelivery(ctx, candidateDelivery("migrated-candidate"), &store.ReviewCandidate{
		ProjectID: 42, MRIID: 7, HeadSHA: reviewHead("b"),
		Payload: candidatePayload(t, "mr_open"), MaxAttempts: 3,
	}); err != nil {
		t.Fatalf("RecordReviewCandidateDelivery: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close migrated db: %v", err)
	}

	s, err = Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("reopen migrated db: %v", err)
	}
	defer s.Close()
	control, err = s.GetReviewControl(ctx, 42, 7)
	if err != nil {
		t.Fatalf("GetReviewControl after reopen: %v", err)
	}
	if control.Generation != 5 || !control.Cancelled || control.CandidateSeq != 1 || control.BarrierSeq != 0 {
		t.Fatalf("control after reopen = %+v", control)
	}
}
