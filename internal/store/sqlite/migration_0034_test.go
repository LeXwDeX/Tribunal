package sqlite

import (
	"context"
	"database/sql"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

// TestWebhookDeliveryMRMigrationBackfillsJobs rehearses the 0034 upgrade on a
// database written before it (#466): enqueued deliveries recover the MR from
// their job payload, while ignored/rejected rows without a job keep 0.
func TestWebhookDeliveryMRMigrationBackfillsJobs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-deliveries.db")

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
		if !entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	stamp := "2026-11-14T10:00:00.000000000Z"
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			t.Fatalf("migrationVersion(%s): %v", name, err)
		}
		if version >= 34 {
			continue
		}
		body, err := migrations.FS.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if _, err := legacy.Exec(string(body)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
		if _, err := legacy.Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			version, name, stamp); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}

	if _, err := legacy.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc, webhook_secret_enc, created_at, updated_at)
		VALUES (7, 'group/legacy', 'https://gitlab.example.com/group/legacy', 'main', x'00', x'01', ?, ?)`,
		stamp, stamp); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO jobs
		(id, kind, key, payload, state, attempts, max_attempts, run_after, created_at, updated_at)
		VALUES ('job-7', 'review.candidate', 'review:7:9', '{"project_id":7,"mr_iid":9}', 'queued', 0, 3, ?, ?, ?)`,
		stamp, stamp, stamp); err != nil {
		t.Fatalf("insert job: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO webhook_deliveries
		(project_id, uuid, event, outcome, reason, job_id, received_at)
		VALUES
		(7, 'enqueued', 'Merge Request Hook', 'enqueued', NULL, 'job-7', ?),
		(7, 'ignored', 'Note Hook', 'ignored', 'unsupported_action', NULL, ?)`,
		stamp, stamp); err != nil {
		t.Fatalf("insert deliveries: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy: %v", err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open upgraded store: %v", err)
	}
	defer st.Close()

	var enqueued, ignored int64
	if err := st.db.QueryRowContext(ctx, `SELECT mr_iid FROM webhook_deliveries WHERE uuid = 'enqueued'`).Scan(&enqueued); err != nil {
		t.Fatal(err)
	}
	if enqueued != 9 {
		t.Fatalf("enqueued mr_iid = %d, want the job payload MR 9", enqueued)
	}
	if err := st.db.QueryRowContext(ctx, `SELECT mr_iid FROM webhook_deliveries WHERE uuid = 'ignored'`).Scan(&ignored); err != nil {
		t.Fatal(err)
	}
	if ignored != 0 {
		t.Fatalf("ignored mr_iid = %d, want 0 without a job", ignored)
	}
}
