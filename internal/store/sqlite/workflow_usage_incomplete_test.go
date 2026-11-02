package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"io/fs"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/migrations"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// TestWorkflowUsageIncompletePersistsAcrossReadModelsAndRestart verifies that
// the DAG projection is stored and returned by every workflow and node read
// model, survives a process restart, and stays sticky at workflow level.
func TestWorkflowUsageIncompletePersistsAcrossReadModelsAndRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "usage-incomplete.db")
	clock := newFakeClock()
	open := func() *Store {
		st, err := Open(ctx, path, WithClock(clock.Now))
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		return st
	}

	st := open()
	mustProject(t, st, 42)
	wfID := "wf-usage-incomplete"
	rev := mustCreateWorkflow(t, st, wfID, reviewMeta(42, 7, "head-1"), clock.Now())
	rev = mustAppendEvent(t, st, wfID, rev,
		testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "a", 1, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "a", 1, "p", "m", clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, testEvent(t, dag.EventNodeCompleted, dag.NodeCompletedPayload{
		NodeID:          "a",
		Attempt:         1,
		Output:          json.RawMessage(`{"ok":true}`),
		Usage:           dag.Usage{InputTokens: 2, OutputTokens: 3, CostMicros: 5},
		UsageIncomplete: true,
	}, clock.Now()))

	check := func(label string, st *Store) {
		t.Helper()
		wf, err := st.GetWorkflow(ctx, wfID)
		if err != nil {
			t.Fatalf("%s GetWorkflow: %v", label, err)
		}
		if !wf.UsageIncomplete {
			t.Fatalf("%s workflow usage_incomplete = false, want true", label)
		}
		if wf.TokensIn != 2 || wf.TokensOut != 3 || wf.CostMicros != 5 {
			t.Fatalf("%s workflow usage = %+v", label, wf)
		}
		nodes, err := st.GetWorkflowNodes(ctx, wfID)
		if err != nil {
			t.Fatalf("%s GetWorkflowNodes: %v", label, err)
		}
		if len(nodes) != 2 {
			t.Fatalf("%s nodes = %d, want 2", label, len(nodes))
		}
		if !nodes[0].UsageIncomplete || nodes[1].UsageIncomplete {
			t.Fatalf("%s node flags = %+v", label, nodes)
		}
		projectID := int64(42)
		list, _, err := st.ListWorkflows(ctx, store.WorkflowFilter{ProjectID: &projectID}, "", 10)
		if err != nil {
			t.Fatalf("%s ListWorkflows: %v", label, err)
		}
		if len(list) != 1 || !list[0].UsageIncomplete {
			t.Fatalf("%s list workflows = %+v", label, list)
		}
		active, err := st.FindActiveReview(ctx, 42, 7)
		if err != nil || !active.UsageIncomplete {
			t.Fatalf("%s FindActiveReview = %+v, err=%v", label, active, err)
		}
		batch, err := st.ListWorkflowNodesByWorkflow(ctx, []string{wfID})
		if err != nil {
			t.Fatalf("%s ListWorkflowNodesByWorkflow: %v", label, err)
		}
		if len(batch[wfID]) != 2 || !batch[wfID][0].UsageIncomplete || batch[wfID][1].UsageIncomplete {
			t.Fatalf("%s batch nodes = %+v", label, batch)
		}
	}
	check("before restart", st)

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	st = open()
	defer st.Close()
	check("after restart", st)

	// A later complete result must not clear the workflow-level marker.
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "b", 1, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "b", 1, "p", "m", clock.Now()))
	mustAppendEvent(t, st, wfID, rev, completedEvent(t, "b", 1, 7, clock.Now()))
	wf, err := st.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow after second node: %v", err)
	}
	if !wf.UsageIncomplete || wf.TokensIn != 4 || wf.TokensOut != 6 {
		t.Fatalf("workflow usage after second node = %+v", wf)
	}
}

// TestWorkflowUsageIncompleteLegacyEventReadsFalse verifies that an event
// written before the field existed still decodes and projects to false. False
// only means no marker was detected; it does not prove complete usage.
func TestWorkflowUsageIncompleteLegacyEventReadsFalse(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	wfID := "wf-legacy-event"
	rev := mustCreateWorkflow(t, st, wfID, reviewMeta(42, 8, "head-2"), clock.Now())
	rev = mustAppendEvent(t, st, wfID, rev,
		testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, queuedEvent(t, "a", 1, clock.Now()))
	rev = mustAppendEvent(t, st, wfID, rev, startedEvent(t, "a", 1, "p", "m", clock.Now()))
	mustAppendEvent(t, st, wfID, rev, dag.Event{
		Type: dag.EventNodeCompleted,
		Payload: json.RawMessage(
			`{"node_id":"a","attempt":1,"usage":{"input_tokens":2,"output_tokens":3,"cost_micros":5}}`),
		At: clock.Now(),
	})

	wf, err := st.GetWorkflow(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.UsageIncomplete {
		t.Fatalf("legacy event workflow usage_incomplete = true, want false")
	}
	nodes, err := st.GetWorkflowNodes(ctx, wfID)
	if err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	if nodes[0].UsageIncomplete {
		t.Fatalf("legacy event node usage_incomplete = true, want false")
	}
}

// TestWorkflowUsageIncompleteMigrationLeavesLegacyRowsUnmarked applies every
// migration before 0024, inserts rows that predate the columns, and verifies
// that 0024 adds default-false flags without changing the old values.
func TestWorkflowUsageIncompleteMigrationLeavesLegacyRowsUnmarked(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "legacy-usage.db")

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
		if version >= 24 {
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
		VALUES (42, 'group/legacy', 'https://gitlab.example.com/group/legacy',
		 'main', x'00', x'01', ?, ?)`, stamp, stamp); err != nil {
		t.Fatalf("insert legacy project: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO workflows
		(id, kind, project_id, trigger, status, graph_rev, tokens_in, tokens_out,
		 cost_micros, created_at)
		VALUES ('wf-legacy-usage', 'review', 42, 'mr_open', 'completed', 9, 17, 5, 3, ?)`,
		stamp); err != nil {
		t.Fatalf("insert legacy workflow: %v", err)
	}
	if _, err := legacy.Exec(`INSERT INTO workflow_nodes
		(workflow_id, id, kind, status, required, tokens_in, tokens_out,
		 cost_micros, added_rev)
		VALUES ('wf-legacy-usage', 'a', 'plan', 'completed', 1, 17, 5, 3, 1)`); err != nil {
		t.Fatalf("insert legacy node: %v", err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatalf("close legacy db: %v", err)
	}

	st, err := Open(ctx, path, WithClock(newFakeClock().Now))
	if err != nil {
		t.Fatalf("Open after 0024: %v", err)
	}
	defer st.Close()
	wf, err := st.GetWorkflow(ctx, "wf-legacy-usage")
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if wf.UsageIncomplete || wf.TokensIn != 17 || wf.TokensOut != 5 || wf.CostMicros != 3 {
		t.Fatalf("legacy workflow = %+v", wf)
	}
	nodes, err := st.GetWorkflowNodes(ctx, "wf-legacy-usage")
	if err != nil {
		t.Fatalf("GetWorkflowNodes: %v", err)
	}
	if len(nodes) != 1 || nodes[0].UsageIncomplete || nodes[0].TokensIn != 17 {
		t.Fatalf("legacy nodes = %+v", nodes)
	}
	projectID := int64(42)
	list, _, err := st.ListWorkflows(ctx, store.WorkflowFilter{ProjectID: &projectID}, "", 10)
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	if len(list) != 1 || list[0].UsageIncomplete {
		t.Fatalf("legacy list workflows = %+v", list)
	}
	batch, err := st.ListWorkflowNodesByWorkflow(ctx, []string{"wf-legacy-usage"})
	if err != nil {
		t.Fatalf("ListWorkflowNodesByWorkflow: %v", err)
	}
	if len(batch["wf-legacy-usage"]) != 1 || batch["wf-legacy-usage"][0].UsageIncomplete {
		t.Fatalf("legacy batch nodes = %+v", batch)
	}
	if got := rowCount(t, st, `SELECT COUNT(*) FROM schema_migrations WHERE version = 24`); got != 1 {
		t.Fatalf("migration 0024 recorded %d times, want 1", got)
	}
}

// TestWorkflowUsageIncompleteConcurrentWritesAndReads checks that independent
// appends and read-model queries keep the flags consistent under concurrency.
func TestWorkflowUsageIncompleteConcurrentWritesAndReads(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	const workflowCount = 4
	ids := make([]string, 0, workflowCount)
	revs := make([]int64, 0, workflowCount)
	for i := 0; i < workflowCount; i++ {
		id := "wf-concurrent-" + string(rune('a'+i))
		ids = append(ids, id)
		rev := mustCreateWorkflow(t, st, id, reviewMeta(42, int64(100+i), "head"), clock.Now())
		rev = mustAppendEvent(t, st, id, rev,
			testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, clock.Now()))
		rev = mustAppendEvent(t, st, id, rev, queuedEvent(t, "a", 1, clock.Now()))
		rev = mustAppendEvent(t, st, id, rev, startedEvent(t, "a", 1, "p", "m", clock.Now()))
		revs = append(revs, rev)
	}

	var wg sync.WaitGroup
	errs := make(chan error, workflowCount+workflowCount*4)
	for i, id := range ids {
		i, id := i, id
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := st.Append(ctx, id, revs[i], testEvent(t, dag.EventNodeCompleted, dag.NodeCompletedPayload{
				NodeID: "a", Attempt: 1,
				Usage:           dag.Usage{InputTokens: 1},
				UsageIncomplete: true,
			}, clock.Now()))
			if err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()

	for _, id := range ids {
		id := id
		for reader := 0; reader < 4; reader++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for n := 0; n < 10; n++ {
					wf, err := st.GetWorkflow(ctx, id)
					if err != nil {
						errs <- err
						return
					}
					if !wf.UsageIncomplete {
						errs <- errUsageIncompleteRead(id)
						return
					}
					nodes, err := st.GetWorkflowNodes(ctx, id)
					if err != nil || len(nodes) == 0 || !nodes[0].UsageIncomplete {
						if err == nil {
							err = errUsageIncompleteRead(id)
						}
						errs <- err
						return
					}
				}
			}()
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent read: %v", err)
	}
}

type usageIncompleteReadError string

func (e usageIncompleteReadError) Error() string {
	return "workflow or node lost usage_incomplete: " + string(e)
}

func errUsageIncompleteRead(id string) error {
	return usageIncompleteReadError(id)
}
