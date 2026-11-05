package app

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/pkg/dag"
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
			want := `msg="recovery cancelling workflow" workflow_id=` + id + " cause=" + tc.cause
			if !strings.Contains(logs.String(), want) {
				t.Fatalf("log missing %q:\n%s", want, logs.String())
			}
			snapshot, err := f.service.engine.Snapshot(ctx, id)
			if err != nil || snapshot.CancelReason != tc.cause {
				t.Fatalf("cancel reason = %q, %v; want %q", snapshot.CancelReason, err, tc.cause)
			}
		})
	}
}

func TestRecoveryLogsControlNotCurrentCause(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	job := f.seedReview(t, "job-cause-control")
	id := interruptReview(t, f, job)
	f.seedCancel(t, "cancel-after-interrupt")
	var logs bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logs, nil))
	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflow(context.Background(), id)
	if err != nil || wf.Status != "cancelled" {
		t.Fatalf("recovery = %+v, %v; want cancelled", wf, err)
	}
	want := `msg="recovery cancelling workflow" workflow_id=` + id + " cause=control_not_current"
	if !strings.Contains(logs.String(), want) {
		t.Fatalf("log missing %q:\n%s", want, logs.String())
	}
}

// A process kill leaves a running node; recovery must restart it and run the
// model nodes even though the workflow has a token budget.
func TestRecoveryContinuesBudgetedWorkflowAfterInterruption(t *testing.T) {
	f := newReviewFixtureWithProfile(t, reviewTestHead, func(p *config.ProfileConfig) {
		p.Budget.MaxTokens = 1_000_000
	})
	job := f.seedReview(t, "job-budgeted-recovery")
	id := interruptReview(t, f, job)
	before := f.runner.count()
	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if f.runner.count() <= before {
		t.Fatalf("recovered budgeted workflow never reached the model: calls %d -> %d", before, f.runner.count())
	}
	wf, err := f.engine.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != dag.WorkflowCompleted || !wf.UsageIncomplete || wf.UsageUnreported || wf.UsageInterruptions == 0 {
		t.Fatalf("status=%s incomplete=%t unreported=%t interruptions=%d, want completed/true/false/>0",
			wf.Status, wf.UsageIncomplete, wf.UsageUnreported, wf.UsageInterruptions)
	}
	assertReviewPublished(t, f, id)
}

// One recovery round restarts every running node. With more concurrently
// running model nodes than the interruption cap, a budgeted workflow must
// still continue: the cap bounds restarts per node, not per round.
func TestRecoveryContinuesBudgetedWorkflowWithParallelNodes(t *testing.T) {
	const parallel = 5
	f := newReviewFixtureWithProfile(t, reviewTestHead, func(p *config.ProfileConfig) {
		p.Budget.MaxTokens = 1_000_000
		p.Propose = nil
		for i := 0; i < parallel; i++ {
			p.Propose = append(p.Propose, config.ProposeConfig{Block: "code-review", Provider: "fake"})
		}
	})
	job := f.seedReview(t, "job-budgeted-parallel-recovery")
	id := interruptReviewRunning(t, f, job, parallel)
	if err := f.service.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	wf, err := f.engine.Snapshot(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != dag.WorkflowCompleted || wf.UsageUnreported || wf.UsageInterruptions != 1 {
		t.Fatalf("status=%s unreported=%t interruptions=%d error=%q, want completed/false/1",
			wf.Status, wf.UsageUnreported, wf.UsageInterruptions, wf.Error)
	}
	assertReviewPublished(t, f, id)
}
