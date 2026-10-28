package app

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestReviewContextUsesTargetTipNotSharedWorkspaceBase(t *testing.T) {
	for _, tc := range []struct {
		name, workspaceBase, docBase, want string
	}{
		{"locked-current-but-wrong-target", strings.Repeat("1", 40), strings.Repeat("1", 40), ""},
		{"stale-but-matches-target", strings.Repeat("1", 40), strings.Repeat("2", 40), "CONTEXT_CANARY"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			f.service.reviewDefaults.Ignore = nil
			f.publisher.reviewDefaults.Ignore = nil
			ctx := context.Background()
			if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: tc.docBase}); err != nil {
				t.Fatal(err)
			}
			contexts := newContextService(t, f)
			if _, err := contexts.SaveContext(ctx, 42, "agents", "CONTEXT_CANARY", nil, nil); err != nil {
				t.Fatal(err)
			}
			if err := contexts.LockContext(ctx, 42, "agents", true, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := f.db.SaveWorkspace(ctx, store.Workspace{ProjectID: 42, State: "ready", BaseSHA: tc.workspaceBase}); err != nil {
				t.Fatal(err)
			}
			before, err := f.db.GetCurrentContextDoc(ctx, 42, "agents")
			if err != nil || before.Stale != (tc.docBase != tc.workspaceBase) {
				t.Fatalf("unexpected fixture: %+v, %v", before, err)
			}
			job := f.seedReview(t, "job-target-context")
			if err := f.service.RunReview(ctx, job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil {
				t.Fatal(err)
			}
			record, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			var d review.Dossier
			if err := json.Unmarshal(record.Data, &d); err != nil {
				t.Fatal(err)
			}
			if d.ContextDocs != tc.want {
				t.Fatalf("context = %q, want %q", d.ContextDocs, tc.want)
			}
			wantBase := ""
			if tc.want != "" {
				wantBase = tc.docBase
			}
			if d.WorkspaceProvenance == nil || d.WorkspaceProvenance.ContextBaseSHA != wantBase {
				t.Fatalf("context provenance = %+v, want base %q", d.WorkspaceProvenance, wantBase)
			}
			after, err := f.db.GetCurrentContextDoc(ctx, 42, "agents")
			if err != nil || !after.Locked || after.Content != before.Content || after.BaseSHA != before.BaseSHA {
				t.Fatalf("manual context changed: %+v, %v", after, err)
			}
		})
	}
}
