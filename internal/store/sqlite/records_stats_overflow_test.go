package sqlite

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func insertStatsOverflowCall(t *testing.T, st *Store, id, workflowID string, input, output int64, latencyMS int64, at time.Time) {
	t.Helper()
	_, err := st.db.ExecContext(context.Background(), `INSERT INTO llm_calls
		(id, workflow_id, node_id, turn, provider, model, input_tokens, output_tokens,
		 cache_read, cache_write, latency_ms, created_at)
		VALUES (?, ?, 'a', 1, 'p', 'm', ?, ?, 0, 0, ?, ?)`,
		id, workflowID, input, output, latencyMS, formatTime(at))
	if err != nil {
		t.Fatalf("insert llm call %s: %v", id, err)
	}
}

func setStatsOverflowNode(t *testing.T, st *Store, workflowID, nodeID string, cost int64, at time.Time) {
	t.Helper()
	result, err := st.db.ExecContext(context.Background(), `UPDATE workflow_nodes
		SET provider = 'p', model = 'm', cost_micros = ?, started_at = ?, finished_at = ?
		WHERE workflow_id = ? AND id = ?`,
		cost, formatTime(at), formatTime(at), workflowID, nodeID)
	if err != nil {
		t.Fatalf("set workflow node %s: %v", nodeID, err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		t.Fatalf("set workflow node %s: rows=%d err=%v", nodeID, changed, err)
	}
}

func setStatsOverflowWorkflowCost(t *testing.T, st *Store, workflowID string, cost int64) {
	t.Helper()
	if _, err := st.db.ExecContext(context.Background(),
		`UPDATE workflows SET cost_micros = ? WHERE id = ?`, cost, workflowID); err != nil {
		t.Fatalf("set workflow cost %s: %v", workflowID, err)
	}
}

func TestSaturatingAddInt64Bounds(t *testing.T) {
	cases := []struct{ a, b, want int64 }{
		{1, 2, 3},
		{math.MaxInt64 - 1, 1, math.MaxInt64},
		{math.MaxInt64, 1, math.MaxInt64},
		{math.MinInt64 + 1, -1, math.MinInt64},
		{math.MinInt64, -1, math.MinInt64},
	}
	for _, tc := range cases {
		if got := saturatingAddInt64(tc.a, tc.b); got != tc.want {
			t.Fatalf("saturatingAddInt64(%d, %d) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestStatsSaturatesExtremeTokenAndCostAggregates(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	at := clock.Now().UTC()
	wfID := "wf-overflow"
	mustCreateWorkflow(t, st, wfID, reviewMeta(42, 9, "head-overflow"), at)
	otherID := "wf-overflow-other"
	mustCreateWorkflow(t, st, otherID, reviewMeta(42, 10, "head-overflow-other"), at)

	insertStatsOverflowCall(t, st, "call-huge", wfID, math.MaxInt64, math.MaxInt64, 100, at)
	insertStatsOverflowCall(t, st, "call-one", wfID, 1, 1, 300, at)
	setStatsOverflowNode(t, st, wfID, "a", math.MaxInt64, at)
	setStatsOverflowNode(t, st, wfID, "b", 1, at)
	setStatsOverflowWorkflowCost(t, st, wfID, math.MaxInt64)
	setStatsOverflowWorkflowCost(t, st, otherID, 1)

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.CostMicros != math.MaxInt64 {
		t.Fatalf("top cost = %d, want saturated MaxInt64", stats.CostMicros)
	}
	if len(stats.ByProject) != 1 || stats.ByProject[0].CostMicros != math.MaxInt64 {
		t.Fatalf("by project = %+v, want saturated cost", stats.ByProject)
	}
	if len(stats.ByProvider) != 1 || stats.ByProvider[0].CostMicros != math.MaxInt64 {
		t.Fatalf("by provider = %+v, want saturated cost", stats.ByProvider)
	}
	if len(stats.ByBlock) != 2 ||
		stats.ByBlock[0].Block != "code-review" || stats.ByBlock[0].CostMicros != math.MaxInt64 ||
		stats.ByBlock[1].Block != "synthesize" || stats.ByBlock[1].CostMicros != 1 {
		t.Fatalf("by block = %+v, want per-block costs without overflow", stats.ByBlock)
	}
	if len(stats.ByModel) != 1 {
		t.Fatalf("by model = %+v", stats.ByModel)
	}
	model := stats.ByModel[0]
	if model.InputTokens != math.MaxInt64 || model.OutputTokens != math.MaxInt64 ||
		model.CostMicros != math.MaxInt64 {
		t.Fatalf("model stats = %+v, want saturated usage and cost", model)
	}
	if model.Calls != 2 || model.AvgLatencyMS == nil || *model.AvgLatencyMS != 200 ||
		model.P50LatencyMS == nil || *model.P50LatencyMS != 100 {
		t.Fatalf("model stats = %+v, want normal call/latency aggregates", model)
	}
}

func TestStatsKeepsLegacyNegativeUsageSums(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 44)
	at := clock.Now().UTC()
	wfID := "wf-negative"
	mustCreateWorkflow(t, st, wfID, reviewMeta(44, 9, "head-negative"), at)
	otherID := "wf-negative-other"
	mustCreateWorkflow(t, st, otherID, reviewMeta(44, 10, "head-negative-other"), at)

	insertStatsOverflowCall(t, st, "call-negative", wfID, -5, -5, 100, at)
	insertStatsOverflowCall(t, st, "call-positive", wfID, 3, 3, 300, at)
	setStatsOverflowNode(t, st, wfID, "a", -7, at)
	setStatsOverflowNode(t, st, wfID, "b", 2, at)
	setStatsOverflowWorkflowCost(t, st, wfID, -7)
	setStatsOverflowWorkflowCost(t, st, otherID, 2)

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(44)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.CostMicros != -5 {
		t.Fatalf("top cost = %d, want -5", stats.CostMicros)
	}
	if len(stats.ByProject) != 1 || stats.ByProject[0].CostMicros != -5 {
		t.Fatalf("by project = %+v, want cost -5", stats.ByProject)
	}
	if len(stats.ByModel) != 1 {
		t.Fatalf("by model = %+v", stats.ByModel)
	}
	model := stats.ByModel[0]
	if model.InputTokens != -2 || model.OutputTokens != -2 || model.CostMicros != -5 {
		t.Fatalf("model stats = %+v, want summed negatives", model)
	}
}
