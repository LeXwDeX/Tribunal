package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

func TestRecoveryCancelLogsStableCause(t *testing.T) {
	tests := []struct {
		name     string
		metadata string
		value    string
		cause    string
	}{
		{name: "missing profile hash", metadata: "profile_hash", cause: "profile_hash_changed"},
		{name: "foreign execution identity", metadata: "execution_identity", value: "sha256:other", cause: "execution_identity_changed"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			job := f.seedReview(t, "job-cause-"+tc.cause)
			id := interruptReview(t, f, job)
			ctx := context.Background()
			record, err := f.db.GetWorkflowDossier(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			var data map[string]any
			if err := json.Unmarshal(record.Data, &data); err != nil {
				t.Fatal(err)
			}
			metadata := data["metadata"].(map[string]any)
			if tc.value == "" {
				delete(metadata, tc.metadata)
			} else {
				metadata[tc.metadata] = tc.value
			}
			if record.Data, err = json.Marshal(data); err != nil {
				t.Fatal(err)
			}
			record.Hash = ""
			if _, err := f.db.SaveWorkflowDossier(ctx, *record); err != nil {
				t.Fatal(err)
			}
			var logs bytes.Buffer
			f.service.logger = slog.New(slog.NewTextHandler(&logs, nil))
			if err := f.service.Recover(ctx); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflow(ctx, id)
			if err != nil || wf.Status != "cancelled" {
				t.Fatalf("recovery = %+v, %v; want cancelled", wf, err)
			}
			want := `msg="recovery cancelled workflow" workflow_id=` + id + " cause=" + tc.cause
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("log missing %q:\n%s", want, logs.String())
			}
		})
	}
}
