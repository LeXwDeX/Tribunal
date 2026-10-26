package sqlite

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestListWorkflowNodesByWorkflowMatchesPerWorkflowRead(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	revA := mustCreateWorkflow(t, st, "wf-a", reviewMeta(42, 1, "head-a"), clock.Now())
	finishWorkflow(t, st, "wf-a", revA, clock.Now())
	mustCreateWorkflow(t, st, "wf-b", reviewMeta(42, 2, "head-b"), clock.Now())
	mustCreateWorkflow(t, st, "wf-c", reviewMeta(42, 3, "head-c"), clock.Now())

	got, err := st.ListWorkflowNodesByWorkflow(ctx, []string{"wf-a", "wf-b", "wf-missing"})
	if err != nil {
		t.Fatalf("ListWorkflowNodesByWorkflow: %v", err)
	}
	if _, ok := got["wf-c"]; ok {
		t.Fatalf("unrequested workflow returned: %v", got["wf-c"])
	}
	if _, ok := got["wf-missing"]; ok {
		t.Fatalf("missing workflow returned: %v", got["wf-missing"])
	}
	for _, id := range []string{"wf-a", "wf-b"} {
		full, err := st.GetWorkflowNodes(ctx, id)
		if err != nil {
			t.Fatalf("GetWorkflowNodes %s: %v", id, err)
		}
		batch := got[id]
		if len(batch) != len(full) || len(full) == 0 {
			t.Fatalf("%s nodes: batch %d, full %d", id, len(batch), len(full))
		}
		for i := range full {
			b, f := batch[i], full[i]
			if b.WorkflowID != f.WorkflowID || b.ID != f.ID || b.Kind != f.Kind ||
				b.Status != f.Status || b.Provider != f.Provider || b.Attempt != f.Attempt ||
				!reflect.DeepEqual(b.DependsOn, f.DependsOn) ||
				!reflect.DeepEqual(b.StartedAt, f.StartedAt) || !reflect.DeepEqual(b.FinishedAt, f.FinishedAt) {
				t.Fatalf("%s node %d: batch %+v, full %+v", id, i, b, f)
			}
			if b.Output != nil {
				t.Fatalf("%s node %s: batch read loaded output %s", id, b.ID, b.Output)
			}
		}
	}

	empty, err := st.ListWorkflowNodesByWorkflow(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty input = %v, %v", empty, err)
	}
}

func TestCountFindingsByWorkflowUsesEffectiveSeverity(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-a", reviewMeta(42, 1, "head-a"), clock.Now())
	mustCreateWorkflow(t, st, "wf-b", reviewMeta(42, 2, "head-b"), clock.Now())
	mustCreateWorkflow(t, st, "wf-c", reviewMeta(42, 3, "head-c"), clock.Now())

	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{
		{ID: "a1", OriginNode: "n", OriginIndex: 0, Severity: "P1", FinalSeverity: "P0", Category: "c", Title: "t", Body: "b", Decision: "publish"},
		{ID: "a2", OriginNode: "n", OriginIndex: 1, Severity: "P1", Category: "c", Title: "t", Body: "b", Decision: "publish"},
		{ID: "a3", OriginNode: "n", OriginIndex: 2, Severity: "P2", Category: "c", Title: "t", Body: "b", Decision: "drop"},
		{ID: "a4", OriginNode: "n", OriginIndex: 3, Severity: "P2", Category: "c", Title: "t", Body: "b"},
		{ID: "a5", OriginNode: "n", OriginIndex: 4, Severity: "P1", Category: "c", Title: "t", Body: "b", Decision: "publish"},
	}); err != nil {
		t.Fatalf("ReplaceFindings wf-a: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE findings SET final_severity = '' WHERE id = ?`, "a2"); err != nil {
		t.Fatalf("set empty final severity: %v", err)
	}
	if err := st.ReplaceFindings(ctx, "wf-b", []store.Finding{
		{ID: "b1", OriginNode: "n", OriginIndex: 0, Severity: "info", Category: "c", Title: "t", Body: "b", Decision: "mark"},
	}); err != nil {
		t.Fatalf("ReplaceFindings wf-b: %v", err)
	}
	if err := st.ReplaceFindings(ctx, "wf-c", []store.Finding{
		{ID: "c1", OriginNode: "n", OriginIndex: 0, Severity: "P0", Category: "c", Title: "t", Body: "b", Decision: "publish"},
	}); err != nil {
		t.Fatalf("ReplaceFindings wf-c: %v", err)
	}

	got, err := st.CountFindingsByWorkflow(ctx, []string{"wf-a", "wf-b"})
	if err != nil {
		t.Fatalf("CountFindingsByWorkflow: %v", err)
	}
	sort.Slice(got, func(i, j int) bool {
		a, b := got[i], got[j]
		if a.WorkflowID != b.WorkflowID {
			return a.WorkflowID < b.WorkflowID
		}
		if a.Decision != b.Decision {
			return a.Decision < b.Decision
		}
		return a.Severity < b.Severity
	})
	want := []store.FindingCount{
		{WorkflowID: "wf-a", Decision: "", Severity: "P2", Count: 1},
		{WorkflowID: "wf-a", Decision: "drop", Severity: "P2", Count: 1},
		{WorkflowID: "wf-a", Decision: "publish", Severity: "P0", Count: 1},
		{WorkflowID: "wf-a", Decision: "publish", Severity: "P1", Count: 2},
		{WorkflowID: "wf-b", Decision: "mark", Severity: "info", Count: 1},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("counts = %+v, want %+v", got, want)
	}

	empty, err := st.CountFindingsByWorkflow(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty input = %v, %v", empty, err)
	}
}
