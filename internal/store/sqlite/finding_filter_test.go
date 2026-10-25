package sqlite

import (
	"context"
	"reflect"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func TestListFindingsFiltersByEffectiveSeverity(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustCreateWorkflow(t, st, "wf-severity", reviewMeta(42, 5, "head"), clock.Now())

	findings := []store.Finding{
		{ID: "raw-p1-final-p2", OriginNode: "a", OriginIndex: 0, Severity: "P1", FinalSeverity: "P2", Category: "logic", Title: "t", Body: "b", Decision: "publish"},
		{ID: "raw-p1-final-null", OriginNode: "a", OriginIndex: 1, Severity: "P1", Category: "logic", Title: "t", Body: "b", Decision: "drop"},
		{ID: "raw-p2-final-empty", OriginNode: "a", OriginIndex: 2, Severity: "P2", Category: "logic", Title: "t", Body: "b", Decision: "publish"},
		{ID: "raw-p2-final-p1", OriginNode: "a", OriginIndex: 3, Severity: "P2", FinalSeverity: "P1", Category: "logic", Title: "t", Body: "b", Decision: "publish"},
		{ID: "raw-info-final-empty", OriginNode: "a", OriginIndex: 4, Severity: "info", Category: "logic", Title: "t", Body: "b", Decision: "mark"},
	}
	if err := st.ReplaceFindings(ctx, "wf-severity", findings); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	if _, err := st.db.ExecContext(ctx, `UPDATE findings SET final_severity = '' WHERE id = ?`, "raw-p2-final-empty"); err != nil {
		t.Fatalf("set empty final severity: %v", err)
	}

	cases := []struct {
		name   string
		filter store.FindingFilter
		want   []string
	}{
		{
			name:   "final severity wins over raw severity",
			filter: store.FindingFilter{WorkflowID: "wf-severity", Severity: "P2"},
			want:   []string{"raw-p1-final-p2", "raw-p2-final-empty"},
		},
		{
			name:   "null and empty final severity fall back to raw severity",
			filter: store.FindingFilter{WorkflowID: "wf-severity", Severity: "P1"},
			want:   []string{"raw-p1-final-null", "raw-p2-final-p1"},
		},
		{
			name:   "unrelated severity stays empty",
			filter: store.FindingFilter{WorkflowID: "wf-severity", Severity: "P0"},
			want:   []string{},
		},
		{
			name:   "decision filter still applies",
			filter: store.FindingFilter{WorkflowID: "wf-severity", Severity: "P2", Decision: "publish"},
			want:   []string{"raw-p1-final-p2", "raw-p2-final-empty"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := st.ListFindings(ctx, tc.filter)
			if err != nil {
				t.Fatalf("ListFindings: %v", err)
			}
			ids := make([]string, 0, len(got))
			for _, finding := range got {
				ids = append(ids, finding.ID)
			}
			if !reflect.DeepEqual(ids, tc.want) {
				t.Fatalf("ids = %v, want %v", ids, tc.want)
			}
		})
	}
}
