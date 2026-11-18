package sqlite

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func recordAggregateCall(t *testing.T, st *Store, workflowID, nodeID string, turn int, input, output int64, latency time.Duration, err error) {
	t.Helper()
	record := llm.CallRecord{
		Provider: "p",
		Model:    "m",
		Meta:     llm.CallMeta{WorkflowID: workflowID, NodeID: nodeID, Turn: turn},
		Duration: latency,
		Err:      err,
	}
	if err == nil {
		record.Response = &llm.Response{Usage: llm.Usage{InputTokens: input, OutputTokens: output}, StopReason: llm.StopEndTurn}
	} else if input != 0 || output != 0 {
		record.Response = &llm.Response{Usage: llm.Usage{InputTokens: input, OutputTokens: output}}
	}
	if err := st.RecordLLMCall(context.Background(), record); err != nil {
		t.Fatalf("RecordLLMCall: %v", err)
	}
}

func deleteLLMCallDetails(t *testing.T, st *Store) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(), `DELETE FROM llm_calls`); err != nil {
		t.Fatalf("delete llm_calls: %v", err)
	}
}

// TestModelStatsSurviveDetailRetention is the retention regression: after the
// per-call audit rows are deleted, per-model counts, tokens, error rate and the
// latency percentile still come from the long-lived aggregates (#459).
func TestModelStatsSurviveDetailRetention(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	wfID := "wf-aggregate"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())

	recordAggregateCall(t, st, wfID, "a", 1, 10, 4, time.Second, nil)
	recordAggregateCall(t, st, wfID, "a", 2, 0, 0, 3*time.Second, &llm.Error{Kind: llm.ErrTransient})

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.ModelDetailsPurged {
		t.Fatal("details are still present, but the response claims they were purged")
	}
	assertAggregateModel(t, stats.ByModel)

	deleteLLMCallDetails(t, st)
	stats, err = st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats after retention: %v", err)
	}
	if !stats.ModelDetailsPurged {
		t.Fatal("purged details were not marked")
	}
	assertAggregateModel(t, stats.ByModel)

	// The unfiltered request takes the same aggregate path; latency and error
	// kinds must still be populated without project/date conditions.
	unfiltered, err := st.Stats(ctx, store.StatsFilter{})
	if err != nil {
		t.Fatalf("unfiltered Stats: %v", err)
	}
	if !unfiltered.ModelDetailsPurged {
		t.Fatal("unfiltered response did not mark purged details")
	}
	assertAggregateModel(t, unfiltered.ByModel)
}

// TestModelStatsCountsCrossMidnightCallOnce pins the boundary between detail
// rows and daily aggregates to workflow creation days: a workflow created
// before midnight whose surviving call happened after it must be served from
// the aggregate only, not from both sources (#520).
func TestModelStatsCountsCrossMidnightCallOnce(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	clock.Advance(11*time.Hour + 50*time.Minute) // 2026-10-22T23:50Z
	wfID := "wf-cross-midnight"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())
	recordAggregateCall(t, st, wfID, "a", 1, 10, 4, time.Second, nil)
	clock.Advance(20 * time.Minute) // 2026-10-23T00:10Z
	recordAggregateCall(t, st, wfID, "a", 2, 20, 8, 2*time.Second, nil)

	// Before retention the detail rows are complete: the aggregate-owned day
	// must not claim purged details.
	before, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats before retention: %v", err)
	}
	if before.ModelDetailsPurged {
		t.Fatal("complete detail rows were reported as purged")
	}
	if len(before.ByModel) != 1 || before.ByModel[0].Calls != 2 {
		t.Fatalf("model stats before retention = %+v, want 2 calls", before.ByModel)
	}

	// Retention keeps only the call after midnight, so MIN(created_at) falls
	// on the next UTC day while both calls belong to the earlier day's
	// aggregate row.
	retainedFrom := time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)
	if _, err := st.DeleteLLMCallsBefore(ctx, retainedFrom); err != nil {
		t.Fatalf("DeleteLLMCallsBefore: %v", err)
	}

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.ByModel) != 1 {
		t.Fatalf("by model = %+v", stats.ByModel)
	}
	model := stats.ByModel[0]
	if model.Calls != 2 || model.InputTokens != 30 || model.OutputTokens != 12 {
		t.Fatalf("model stats = %+v, want 2 calls and 30/12 tokens", model)
	}
	if model.AvgLatencyMS == nil || *model.AvgLatencyMS != 1500 {
		t.Fatalf("avg latency = %v, want 1500ms", model.AvgLatencyMS)
	}
}

// TestModelStatsKeepsRetentionBoundaryDayCalls covers the inverse gap: the
// aggregate rows of the oldest retained call's UTC day must stay in the
// aggregate window, because that day's earlier detail rows were deleted by
// retention and are otherwise counted nowhere (#520).
func TestModelStatsKeepsRetentionBoundaryDayCalls(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	// wf-lost is created on the boundary day and its only call is deleted by
	// retention, so it has no detail row left to be counted from.
	clock.Advance(20 * time.Hour) // 2026-10-23T08:00Z
	wfLost := "wf-lost"
	mustCreateWorkflow(t, st, wfLost, reviewMeta(42, 9, "head"), clock.Now())
	recordAggregateCall(t, st, wfLost, "a", 1, 10, 4, time.Second, nil)

	// wf-survivor sets MIN(created_at) with its call at 12:00 after one of its
	// two calls is deleted as well.
	clock.Advance(3 * time.Hour) // 2026-10-23T11:00Z
	wfSurvivor := "wf-survivor"
	mustCreateWorkflow(t, st, wfSurvivor, reviewMeta(42, 10, "head"), clock.Now())
	recordAggregateCall(t, st, wfSurvivor, "a", 1, 20, 8, time.Second, nil)
	clock.Advance(time.Hour) // 2026-10-23T12:00Z
	recordAggregateCall(t, st, wfSurvivor, "a", 2, 5, 2, 2*time.Second, nil)

	if _, err := st.DeleteLLMCallsBefore(ctx, time.Date(2026, 10, 23, 11, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("DeleteLLMCallsBefore: %v", err)
	}

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.ByModel) != 1 {
		t.Fatalf("by model = %+v", stats.ByModel)
	}
	model := stats.ByModel[0]
	if model.Calls != 3 || model.InputTokens != 35 || model.OutputTokens != 14 {
		t.Fatalf("model stats = %+v, want 3 calls and 35/14 tokens", model)
	}
}

// TestModelStatsServesWorkflowsAfterDetailBoundaryFromDetails guards the other
// side of the partition: workflows created after the boundary day are counted
// exactly once from their retained detail rows (#520).
func TestModelStatsServesWorkflowsAfterDetailBoundaryFromDetails(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)

	// Old workflow whose surviving call sets MIN(created_at) to 2026-10-23.
	wfOld := "wf-old"
	mustCreateWorkflow(t, st, wfOld, reviewMeta(42, 9, "head"), clock.Now())
	recordAggregateCall(t, st, wfOld, "a", 1, 10, 4, time.Second, nil)
	clock.Advance(24 * time.Hour) // 2026-10-23T12:00Z
	recordAggregateCall(t, st, wfOld, "a", 2, 20, 8, 2*time.Second, nil)

	// A workflow created the day after the boundary is fully retained.
	clock.Advance(21 * time.Hour) // 2026-10-24T09:00Z
	wfNew := "wf-new"
	mustCreateWorkflow(t, st, wfNew, reviewMeta(42, 10, "head"), clock.Now())
	recordAggregateCall(t, st, wfNew, "a", 1, 5, 2, 3*time.Second, nil)

	if _, err := st.DeleteLLMCallsBefore(ctx, time.Date(2026, 10, 23, 0, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("DeleteLLMCallsBefore: %v", err)
	}

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.ByModel) != 1 {
		t.Fatalf("by model = %+v", stats.ByModel)
	}
	model := stats.ByModel[0]
	if model.Calls != 3 || model.InputTokens != 35 || model.OutputTokens != 14 {
		t.Fatalf("model stats = %+v, want 3 calls and 35/14 tokens", model)
	}
}

func assertAggregateModel(t *testing.T, models []store.ModelStats) {
	t.Helper()
	if len(models) != 1 {
		t.Fatalf("by model = %+v", models)
	}
	model := models[0]
	if model.Provider != "p" || model.Model != "m" || model.Calls != 2 || model.Errors != 1 ||
		model.InputTokens != 10 || model.OutputTokens != 4 ||
		model.AvgLatencyMS == nil || *model.AvgLatencyMS != 2000 ||
		model.P50LatencyMS == nil || *model.P50LatencyMS != 1000 ||
		model.ErrorRate == nil || *model.ErrorRate != 0.5 {
		t.Fatalf("model stats = %+v", model)
	}
	if model.ErrorsByKind[string(llm.ErrTransient)] != 1 {
		t.Fatalf("errors by kind = %+v, want one transient", model.ErrorsByKind)
	}
}

// TestDailyAggregateSaturatesTokenOverflow keeps the Go-side aggregate safe
// from SQLite integer overflow when two legitimate calls approach the limit.
func TestDailyAggregateSaturatesTokenOverflow(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	wfID := "wf-overflow-daily"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())
	recordAggregateCall(t, st, wfID, "a", 1, math.MaxInt64, math.MaxInt64, time.Second, nil)
	recordAggregateCall(t, st, wfID, "a", 2, 1, 1, time.Second, nil)
	deleteLLMCallDetails(t, st)

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.ByModel) != 1 || stats.ByModel[0].InputTokens != math.MaxInt64 ||
		stats.ByModel[0].OutputTokens != math.MaxInt64 {
		t.Fatalf("saturated model stats = %+v", stats.ByModel)
	}
}

// TestDailyAggregateKeepsNegativeTokenSums preserves the legacy sum semantics
// when details are gone.
func TestDailyAggregateKeepsNegativeTokenSums(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 44)
	wfID := "wf-negative-daily"
	mustCreateWorkflow(t, st, wfID, reviewMeta(44, 9, "head"), clock.Now())
	recordAggregateCall(t, st, wfID, "a", 1, -5, -5, time.Second, nil)
	recordAggregateCall(t, st, wfID, "a", 2, 3, 3, time.Second, nil)
	deleteLLMCallDetails(t, st)

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(44)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.ByModel) != 1 || stats.ByModel[0].InputTokens != -2 || stats.ByModel[0].OutputTokens != -2 {
		t.Fatalf("negative sums = %+v", stats.ByModel)
	}
}

// TestDailyAggregateRespectsProjectFilter proves aggregate rows still honor
// the stats project filter once details are gone.
func TestDailyAggregateRespectsProjectFilter(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustProject(t, st, 43)
	wfID := "wf-project-filter"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())
	recordAggregateCall(t, st, wfID, "a", 1, 1, 1, time.Second, nil)
	deleteLLMCallDetails(t, st)

	other, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(43)})
	if err != nil {
		t.Fatalf("Stats other project: %v", err)
	}
	if len(other.ByModel) != 0 || other.ModelDetailsPurged {
		t.Fatalf("other project model stats = %+v purged=%v", other.ByModel, other.ModelDetailsPurged)
	}
	own, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil || len(own.ByModel) != 1 || own.ByModel[0].Calls != 1 {
		t.Fatalf("own project model stats = %+v, %v", own.ByModel, err)
	}
}

// TestDailyAggregatePercentileMatchesNearestRank pins the histogram percentile
// to the existing nearest-rank definition.
func TestDailyAggregatePercentileMatchesNearestRank(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	wfID := "wf-percentile"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())
	for i, latency := range []time.Duration{100 * time.Millisecond, 300 * time.Millisecond, 200 * time.Millisecond} {
		recordAggregateCall(t, st, wfID, "a", i+1, 1, 1, latency, nil)
	}
	deleteLLMCallDetails(t, st)

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.ByModel) != 1 || stats.ByModel[0].P50LatencyMS == nil || *stats.ByModel[0].P50LatencyMS != 200 {
		t.Fatalf("p50 = %+v, want nearest-rank 200ms", stats.ByModel)
	}
}

// TestProjectDeletionRemovesModelAggregates keeps deleted projects out of the
// long-lived stats even after the detail rows are gone (#459).
func TestProjectDeletionRemovesModelAggregates(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	wfID := "wf-delete-project"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head"), clock.Now())
	recordAggregateCall(t, st, wfID, "a", 1, 1, 1, time.Second, nil)
	deleteLLMCallDetails(t, st)

	if err := st.DeleteProject(ctx, 42); err != nil {
		t.Fatalf("DeleteProject: %v", err)
	}
	stats, err := st.Stats(ctx, store.StatsFilter{})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if len(stats.ByModel) != 0 || stats.ModelDetailsPurged {
		t.Fatalf("deleted project still reported: models=%+v purged=%v", stats.ByModel, stats.ModelDetailsPurged)
	}
}
