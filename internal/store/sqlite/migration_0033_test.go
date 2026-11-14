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

// TestBackgroundMaintenanceMigrationBackfillsExistingData rehearses the 0033
// upgrade on a database written before it (#459): applied status rows get a
// verification timestamp, model aggregates are backfilled from the audit rows,
// and both singleton state rows exist.
func TestBackgroundMaintenanceMigrationBackfillsExistingData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-background.db")

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
		if version >= 33 {
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
	if _, err := legacy.Exec(`INSERT INTO workflows
		(id, kind, project_id, mr_iid, head_sha, profile, trigger, status, verdict, created_at, finished_at)
		VALUES ('wf-legacy', 'review', 7, 3, 'head', 'default', 'mr_open', 'completed', 'PASS', ?, ?)`,
		stamp, stamp); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO llm_calls
		(id, workflow_id, node_id, turn, provider, model, input_tokens, output_tokens,
		 cache_read, cache_write, latency_ms, error_kind, created_at)
		VALUES
		('call-ok', 'wf-legacy', 'a', 1, 'p', 'm', 10, 4, 0, 0, 1000, NULL, ?),
		('call-err', 'wf-legacy', 'a', 2, 'p', 'm', 0, 0, 0, 0, 3000, 'transient', ?)`,
		stamp, stamp); err != nil {
		t.Fatalf("insert llm calls: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO review_status
		(project_id, mr_iid, generation, head_sha, attempt, profile, state, detail,
		 note_id, body_hash, version, applied_version, last_error, updated_at)
		VALUES (7, 3, 1, 'head', 0, 'default', 'PASS', '', 11, 'hash', 2, 2, '', ?)`,
		stamp); err != nil {
		t.Fatalf("insert review status: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open with migration 0033: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	status, err := st.GetReviewStatus(ctx, 7, 3)
	if err != nil {
		t.Fatalf("GetReviewStatus: %v", err)
	}
	if status.LastVerifiedAt.IsZero() {
		t.Fatalf("applied status was not backfilled: %+v", status)
	}

	var (
		dailyCalls   int64
		dailyErrors  int64
		dailyInput   int64
		dailyOutput  int64
		dailyLatency int64
		dailySamples int64
	)
	if err := st.db.QueryRowContext(ctx, `SELECT calls, errors, input_tokens, output_tokens,
		latency_ms_sum, latency_samples FROM llm_call_daily
		WHERE day = '2026-11-14' AND project_id = 7 AND provider = 'p' AND model = 'm'`).Scan(
		&dailyCalls, &dailyErrors, &dailyInput, &dailyOutput, &dailyLatency, &dailySamples); err != nil {
		t.Fatalf("backfilled daily aggregate: %v", err)
	}
	if dailyCalls != 2 || dailyErrors != 1 || dailyInput != 10 ||
		dailyOutput != 4 || dailyLatency != 4000 || dailySamples != 2 {
		t.Fatalf("backfilled daily aggregate = calls %d errors %d in %d out %d latency %d samples %d",
			dailyCalls, dailyErrors, dailyInput, dailyOutput, dailyLatency, dailySamples)
	}
	var samples int64
	if err := st.db.QueryRowContext(ctx, `SELECT samples FROM llm_call_latency_daily
		WHERE day = '2026-11-14' AND project_id = 7 AND provider = 'p' AND model = 'm' AND latency_ms = 1000`).Scan(&samples); err != nil || samples != 1 {
		t.Fatalf("latency backfill samples = %d, %v", samples, err)
	}
	var errorCalls int64
	if err := st.db.QueryRowContext(ctx, `SELECT calls FROM llm_call_error_daily
		WHERE day = '2026-11-14' AND project_id = 7 AND provider = 'p' AND model = 'm' AND error_kind = 'transient'`).Scan(&errorCalls); err != nil || errorCalls != 1 {
		t.Fatalf("error backfill calls = %d, %v", errorCalls, err)
	}
	var collected sql.NullString
	if err := st.db.QueryRowContext(ctx, `SELECT last_collected_at FROM feedback_collection_state WHERE id = 1`).Scan(&collected); err != nil {
		t.Fatalf("feedback collection singleton: %v", err)
	}
	var overLimit int
	if err := st.db.QueryRowContext(ctx, `SELECT over_limit FROM workspace_capacity WHERE id = 1`).Scan(&overLimit); err != nil {
		t.Fatalf("workspace capacity singleton: %v", err)
	}
}
