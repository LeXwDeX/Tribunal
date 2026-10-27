package sqlite

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// TestListWorkflowMRTitlesReadsPersistedTitles checks that the MR title is
// persisted when the dossier is saved and that one batched read returns it for
// a page of workflows. Dossiers without a title are absent from the result.
func TestListWorkflowMRTitlesReadsPersistedTitles(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t, newFakeClock())

	dossiers := map[string]string{
		"wf-titled":  `{"mr":{"title":"  feat: 支持批量导入  "}}`,
		"wf-blank":   `{"mr":{"title":"   "}}`,
		"wf-no-mr":   `{"files":["a.go"]}`,
		"wf-mr-type": `{"mr":"not an object"}`,
	}
	for id, data := range dossiers {
		if _, err := s.SaveWorkflowDossier(ctx, store.WorkflowDossier{
			WorkflowID: id, Version: 1, Data: json.RawMessage(data),
		}); err != nil {
			t.Fatalf("SaveWorkflowDossier %s: %v", id, err)
		}
	}

	got, err := s.ListWorkflowMRTitles(ctx, []string{"wf-titled", "wf-blank", "wf-no-mr", "wf-mr-type", "wf-missing"})
	if err != nil {
		t.Fatalf("ListWorkflowMRTitles: %v", err)
	}
	want := map[string]string{"wf-titled": "feat: 支持批量导入"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("titles = %v, want %v", got, want)
	}

	// A later save of the same workflow refreshes the stored title.
	if _, err := s.SaveWorkflowDossier(ctx, store.WorkflowDossier{
		WorkflowID: "wf-titled", Version: 1, Data: json.RawMessage(`{"mr":{"title":"fix: 重命名"}}`),
	}); err != nil {
		t.Fatalf("resave: %v", err)
	}
	got, err = s.ListWorkflowMRTitles(ctx, []string{"wf-titled"})
	if err != nil || got["wf-titled"] != "fix: 重命名" {
		t.Fatalf("titles after resave = %v, %v", got, err)
	}

	empty, err := s.ListWorkflowMRTitles(ctx, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty input = %v, %v", empty, err)
	}
}
