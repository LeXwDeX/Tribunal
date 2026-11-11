package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store/migrations"
)

// TestRemoveCostMigrationDropsColumnsAndPreservesData upgrades a database
// written before migration 0030. The rebuild must drop cost_micros from both
// tables while preserving rows, foreign keys and indexes, and must strip the
// legacy price object from stored model parameters.
func TestRemoveCostMigrationDropsColumnsAndPreservesData(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-cost.db")

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
	stamp := "2026-10-22T10:00:00.000000000Z"
	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			t.Fatalf("migrationVersion(%s): %v", name, err)
		}
		if version >= 30 {
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
			VALUES (?, ?, ?)`, version, name, stamp); err != nil {
			t.Fatalf("record %s: %v", name, err)
		}
	}
	if _, err := legacy.Exec(`INSERT INTO projects
		(id, path_with_namespace, web_url, default_branch, token_enc,
		 webhook_secret_enc, created_at, updated_at)
		VALUES (42, 'group/cost', 'https://gitlab.example.com/group/cost',
		 'main', x'00', x'01', ?, ?)`, stamp, stamp); err != nil {
		t.Fatalf("insert project: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO workflows
		(id, kind, project_id, trigger, status, graph_rev, tokens_in, tokens_out,
		 cost_micros, usage_incomplete, created_at, started_at, finished_at)
		VALUES ('wf-cost', 'review', 42, 'mr_open', 'completed', 9, 17, 5, 3, 1, ?, ?, ?)`,
		stamp, stamp, stamp); err != nil {
		t.Fatalf("insert workflow: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO workflow_nodes
		(workflow_id, id, kind, status, required, tokens_in, tokens_out,
		 cost_micros, usage_incomplete, added_rev)
		VALUES ('wf-cost', 'a', 'plan', 'completed', 1, 17, 5, 3, 1, 1)`); err != nil {
		t.Fatalf("insert node: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO findings
		(id, workflow_id, origin_node, origin_index, severity, category, title, body, votes)
		VALUES ('f-1', 'wf-cost', 'a', 0, 'P1', 'bug', 'title', 'body', 1)`); err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO llm_models
		(id, display_name, protocol, base_url, model, params_json, revision,
		 created_by, created_at, updated_by, updated_at)
		VALUES ('user:priced', 'priced', 'openai-chat', 'https://example.test/v1', 'm',
		 '{"max_tokens":4096,"price":{"input_per_mtok":1.5,"output_per_mtok":2.5},"rpm":60}', 1,
		 1, ?, 1, ?)`, stamp, stamp); err != nil {
		t.Fatalf("insert priced model: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO llm_models
		(id, display_name, protocol, base_url, model, params_json, revision,
		 created_by, created_at, updated_by, updated_at)
		VALUES ('user:corrupt', 'corrupt', 'openai-chat', 'https://example.test/v1', 'm',
		 'not-json', 1, 1, ?, 1, ?)`, stamp, stamp); err != nil {
		t.Fatalf("insert corrupt model: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	st, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("Open after 0030: %v", err)
	}
	defer st.Close()

	for _, table := range []string{"workflows", "workflow_nodes"} {
		for _, column := range tableColumns(t, st, table) {
			if column == "cost_micros" {
				t.Fatalf("%s still has cost_micros", table)
			}
		}
	}

	wf, err := st.GetWorkflow(ctx, "wf-cost")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.TokensIn != 17 || wf.TokensOut != 5 || !wf.UsageIncomplete || wf.GraphRev != 9 {
		t.Fatalf("workflow data changed: %+v", wf)
	}
	nodes, err := st.GetWorkflowNodes(ctx, "wf-cost")
	if err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].TokensIn != 17 || !nodes[0].UsageIncomplete {
		t.Fatalf("node data changed: %+v", nodes)
	}
	if got := rowCount(t, st, `SELECT COUNT(*) FROM findings WHERE workflow_id = 'wf-cost'`); got != 1 {
		t.Fatalf("findings after rebuild = %d, want 1 (DROP TABLE must not cascade)", got)
	}

	assertWorkflowIndexes(t, st)

	var params string
	if err := st.db.QueryRow(`SELECT params_json FROM llm_models WHERE id = 'user:priced'`).Scan(&params); err != nil {
		t.Fatalf("read priced model params: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(params), &decoded); err != nil {
		t.Fatalf("decode priced params %q: %v", params, err)
	}
	if _, present := decoded["price"]; present {
		t.Fatalf("stored price survived migration: %s", params)
	}
	if decoded["max_tokens"] != float64(4096) || decoded["rpm"] != float64(60) {
		t.Fatalf("other params changed: %s", params)
	}
	var corrupt string
	if err := st.db.QueryRow(`SELECT params_json FROM llm_models WHERE id = 'user:corrupt'`).Scan(&corrupt); err != nil {
		t.Fatalf("read corrupt model params: %v", err)
	}
	if corrupt != "not-json" {
		t.Fatalf("corrupt params changed: %q", corrupt)
	}
}

func tableColumns(t *testing.T, st *Store, table string) []string {
	t.Helper()
	rows, err := st.db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("table_info(%s): %v", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var (
			cid        int
			name       string
			ctype      string
			notNull    int
			defaultV   sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &defaultV, &primaryKey); err != nil {
			t.Fatalf("scan table_info(%s): %v", table, err)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate table_info(%s): %v", table, err)
	}
	return columns
}

func assertWorkflowIndexes(t *testing.T, st *Store) {
	t.Helper()
	rows, err := st.db.Query(`PRAGMA index_list(workflows)`)
	if err != nil {
		t.Fatalf("index_list: %v", err)
	}
	indexes := map[string]bool{}
	unique := map[string]bool{}
	for rows.Next() {
		var (
			seq     int
			name    string
			isUniq  int
			origin  string
			partial int
		)
		if err := rows.Scan(&seq, &name, &isUniq, &origin, &partial); err != nil {
			rows.Close()
			t.Fatalf("scan index_list: %v", err)
		}
		indexes[name] = true
		unique[name] = isUniq != 0
	}
	rows.Close()
	for _, name := range []string{"workflows_mr", "workflows_status", "workflows_active_review"} {
		if !indexes[name] {
			t.Fatalf("index %s missing: %v", name, indexes)
		}
	}
	if !unique["workflows_active_review"] {
		t.Fatalf("workflows_active_review is not unique")
	}
	fkRows, err := st.db.Query(`PRAGMA foreign_key_list(workflow_nodes)`)
	if err != nil {
		t.Fatalf("foreign_key_list: %v", err)
	}
	defer fkRows.Close()
	found := false
	for fkRows.Next() {
		var (
			id       int
			seq      int
			table    string
			fromCol  string
			toCol    string
			onUpdate string
			onDelete string
			match    string
		)
		if err := fkRows.Scan(&id, &seq, &table, &fromCol, &toCol, &onUpdate, &onDelete, &match); err != nil {
			t.Fatalf("scan foreign_key_list: %v", err)
		}
		if table == "workflows" && fromCol == "workflow_id" && toCol == "id" && onDelete == "CASCADE" {
			found = true
		}
	}
	if err := fkRows.Err(); err != nil {
		t.Fatalf("iterate foreign_key_list: %v", err)
	}
	if !found {
		t.Fatal("workflow_nodes -> workflows ON DELETE CASCADE foreign key missing")
	}
}

// TestRemoveCostMigrationFreshDatabase verifies a new database never creates
// the removed column and still reports clean foreign keys.
func TestRemoveCostMigrationFreshDatabase(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, filepath.Join(t.TempDir(), "fresh.db"), WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer st.Close()
	for _, table := range []string{"workflows", "workflow_nodes"} {
		for _, column := range tableColumns(t, st, table) {
			if column == "cost_micros" {
				t.Fatalf("%s still has cost_micros", table)
			}
		}
	}
	assertWorkflowIndexes(t, st)
	var violations int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil {
		t.Fatalf("foreign_key_check: %v", err)
	}
	if violations != 0 {
		t.Fatalf("foreign key violations = %d", violations)
	}
}
