package sqlite

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

func statsTrendSpec(kinds ...string) dag.GraphSpec {
	spec := dag.GraphSpec{Nodes: make([]dag.NodeSpec, 0, len(kinds))}
	for index, kind := range kinds {
		id := string(rune('a' + index))
		spec.Nodes = append(spec.Nodes, dag.NodeSpec{ID: id, Kind: kind, Required: true})
	}
	return spec
}

func mustCreateStatsWorkflow(t *testing.T, st *Store, id string, meta map[string]string, spec dag.GraphSpec, at time.Time) int64 {
	t.Helper()
	rev, err := st.Append(context.Background(), id, 0, testEvent(t, dag.EventWorkflowCreated, dag.WorkflowCreatedPayload{
		WorkflowID: id,
		Spec:       spec,
		Meta:       meta,
	}, at))
	if err != nil {
		t.Fatalf("Append created %s: %v", id, err)
	}
	return rev
}

func statsMeta(projectID int64, kind string, mrIID int64) map[string]string {
	meta := map[string]string{
		"project_id":  strconv.FormatInt(projectID, 10),
		"kind":        kind,
		"trigger":     "mr_open",
		"head_sha":    "head",
		"base_sha":    "base",
		"fingerprint": "fp",
		"profile":     "default",
	}
	if mrIID > 0 {
		meta["mr_iid"] = strconv.FormatInt(mrIID, 10)
	}
	if kind == "onboarding" {
		meta["trigger"] = "register"
	}
	return meta
}

func ptrFloat64(value float64) *float64 { return &value }

func assertStatDuration(t *testing.T, label string, got *float64, want float64) {
	t.Helper()
	if got == nil || *got != want {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func assertDailyTrend(t *testing.T, got store.DailyStats, day string, runs, cost, samples int64, avg, p95 *float64) {
	t.Helper()
	if got.Day != day || got.Runs != runs || got.CostMicros != cost || got.DurationSamples != samples {
		t.Fatalf("daily stats %s = %+v, want runs=%d cost=%d samples=%d", day, got, runs, cost, samples)
	}
	for label, pair := range map[string][2]*float64{"avg": {got.AvgDurationMS, avg}, "p95": {got.P95DurationMS, p95}} {
		switch {
		case pair[1] == nil && pair[0] != nil:
			t.Fatalf("%s.%s = %v, want nil", day, label, *pair[0])
		case pair[1] != nil && (pair[0] == nil || *pair[0] != *pair[1]):
			t.Fatalf("%s.%s = %v, want %v", day, label, pair[0], *pair[1])
		}
	}
}

func trendIndex(t *testing.T, trend []store.DailyStats) map[string]store.DailyStats {
	t.Helper()
	days := make(map[string]store.DailyStats, len(trend))
	for _, entry := range trend {
		if _, exists := days[entry.Day]; exists {
			t.Fatalf("duplicate day %s in %+v", entry.Day, trend)
		}
		days[entry.Day] = entry
	}
	return days
}

func TestStatsTrendsGroupsNodesAndWorkflowDurations(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustProject(t, st, 43)

	day1 := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	to := day1.Add(48 * time.Hour)

	// Project 42, day 1: one completed review. The code node "b" has no
	// provider.
	rev := mustCreateStatsWorkflow(t, st, "wf-42-d1", statsMeta(42, "review", 9),
		statsTrendSpec("code-review", "verify"), day1)
	start := day1.Add(time.Hour)
	rev = mustAppendEvent(t, st, "wf-42-d1", rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, start))
	rev = mustAppendEvent(t, st, "wf-42-d1", rev, queuedEvent(t, "a", 1, start))
	rev = mustAppendEvent(t, st, "wf-42-d1", rev, startedEvent(t, "a", 1, "anthropic", "m", start.Add(time.Second)))
	rev = mustAppendEvent(t, st, "wf-42-d1", rev, completedEvent(t, "a", 1, 5, start.Add(2*time.Second)))
	rev = mustAppendEvent(t, st, "wf-42-d1", rev, queuedEvent(t, "b", 1, start.Add(2*time.Second)))
	rev = mustAppendEvent(t, st, "wf-42-d1", rev, startedEvent(t, "b", 1, "", "", start.Add(3*time.Second)))
	rev = mustAppendEvent(t, st, "wf-42-d1", rev, completedEvent(t, "b", 1, 0, start.Add(4*time.Second)))
	if _, err := st.Append(ctx, "wf-42-d1", rev,
		testEvent(t, dag.EventWorkflowCompleted, dag.WorkflowTerminalPayload{}, start.Add(9*time.Second))); err != nil {
		t.Fatalf("complete wf-42-d1: %v", err)
	}
	// Two turns on one persisted node must not inflate its count or cost.
	for turn := 0; turn < 2; turn++ {
		if err := st.RecordLLMCall(ctx, llm.CallRecord{
			Provider: "anthropic", Model: "m",
			Meta:     llm.CallMeta{WorkflowID: "wf-42-d1", NodeID: "a", Turn: turn},
			Duration: time.Second,
		}); err != nil {
			t.Fatalf("RecordLLMCall turn %d: %v", turn, err)
		}
	}

	// Project 43, day 2: one review still running. Node "b" started but has
	// no finish, so it counts but contributes no duration sample. Node "c" is
	// queued but never started, so it must not appear in block stats.
	wf2Start := day2.Add(12*time.Hour + time.Second)
	rev = mustCreateStatsWorkflow(t, st, "wf-43-d2", statsMeta(43, "review", 10),
		statsTrendSpec("review.standards", "review.intent", "review.gate"), day2.Add(12*time.Hour))
	rev = mustAppendEvent(t, st, "wf-43-d2", rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, wf2Start))
	rev = mustAppendEvent(t, st, "wf-43-d2", rev, queuedEvent(t, "a", 1, wf2Start))
	rev = mustAppendEvent(t, st, "wf-43-d2", rev, startedEvent(t, "a", 1, "openai", "m", wf2Start.Add(time.Second)))
	rev = mustAppendEvent(t, st, "wf-43-d2", rev, completedEvent(t, "a", 1, 7, wf2Start.Add(3*time.Second)))
	rev = mustAppendEvent(t, st, "wf-43-d2", rev, queuedEvent(t, "b", 1, wf2Start.Add(3*time.Second)))
	rev = mustAppendEvent(t, st, "wf-43-d2", rev, startedEvent(t, "b", 1, "anthropic", "m", wf2Start.Add(4*time.Second)))
	_ = mustAppendEvent(t, st, "wf-43-d2", rev, queuedEvent(t, "c", 1, wf2Start.Add(4*time.Second)))

	// Project 42, day 2: onboarding must count as a run and its node must
	// contribute provider/block metrics.
	wf3Start := day2.Add(10*time.Hour + time.Second)
	rev = mustCreateStatsWorkflow(t, st, "wf-42-onboard", statsMeta(42, "onboarding", 0),
		statsTrendSpec("synthesize"), day2.Add(10*time.Hour))
	rev = mustAppendEvent(t, st, "wf-42-onboard", rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, wf3Start))
	rev = mustAppendEvent(t, st, "wf-42-onboard", rev, queuedEvent(t, "a", 1, wf3Start))
	rev = mustAppendEvent(t, st, "wf-42-onboard", rev, startedEvent(t, "a", 1, "openai", "m", wf3Start.Add(time.Second)))
	rev = mustAppendEvent(t, st, "wf-42-onboard", rev, completedEvent(t, "a", 1, 11, wf3Start.Add(4*time.Second)))
	if _, err := st.Append(ctx, "wf-42-onboard", rev,
		testEvent(t, dag.EventWorkflowCompleted, dag.WorkflowTerminalPayload{}, wf3Start.Add(4*time.Second))); err != nil {
		t.Fatalf("complete wf-42-onboard: %v", err)
	}

	// This review starts exactly at the exclusive upper bound. Neither the
	// workflow nor its node may leak into any aggregate.
	rev = mustCreateStatsWorkflow(t, st, "wf-excluded", statsMeta(42, "review", 11),
		statsTrendSpec("code-review"), to)
	rev = mustAppendEvent(t, st, "wf-excluded", rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, to))
	rev = mustAppendEvent(t, st, "wf-excluded", rev, queuedEvent(t, "a", 1, to))
	rev = mustAppendEvent(t, st, "wf-excluded", rev, startedEvent(t, "a", 1, "openai", "m", to.Add(time.Second)))
	_ = mustAppendEvent(t, st, "wf-excluded", rev, completedEvent(t, "a", 1, 100, to.Add(2*time.Second)))

	stats, err := st.Stats(ctx, store.StatsFilter{From: day1, To: to})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Runs != 3 || stats.CostMicros != 23 || stats.DurationSamples != 2 {
		t.Fatalf("top totals = runs %d cost %d samples %d", stats.Runs, stats.CostMicros, stats.DurationSamples)
	}
	assertStatDuration(t, "avg_duration_ms", stats.AvgDurationMS, 6500)
	assertStatDuration(t, "p95_duration_ms", stats.P95DurationMS, 9000)

	if len(stats.Trend) != 2 || stats.Trend[0].Day != "2026-10-20" || stats.Trend[1].Day != "2026-10-21" {
		t.Fatalf("trend order = %+v", stats.Trend)
	}
	days := trendIndex(t, stats.Trend)
	assertDailyTrend(t, days["2026-10-20"], "2026-10-20", 1, 5, 1, ptrFloat64(9000), ptrFloat64(9000))
	assertDailyTrend(t, days["2026-10-21"], "2026-10-21", 2, 18, 1, ptrFloat64(4000), ptrFloat64(4000))

	if len(stats.ByProject) != 2 || stats.ByProject[0].ProjectID != 42 || stats.ByProject[1].ProjectID != 43 {
		t.Fatalf("by_project order = %+v", stats.ByProject)
	}
	p42, p43 := stats.ByProject[0], stats.ByProject[1]
	if p42.Runs != 2 || p42.CostMicros != 16 || p42.DurationSamples != 2 || p43.Runs != 1 ||
		p43.CostMicros != 7 || p43.DurationSamples != 0 || p43.AvgDurationMS != nil || p43.P95DurationMS != nil {
		t.Fatalf("project stats = %+v / %+v", p42, p43)
	}
	assertStatDuration(t, "project 42 avg", p42.AvgDurationMS, 6500)
	assertStatDuration(t, "project 42 p95", p42.P95DurationMS, 9000)
	if len(p42.Trend) != 2 || len(p43.Trend) != 1 {
		t.Fatalf("project trends = %+v / %+v", p42.Trend, p43.Trend)
	}
	assertDailyTrend(t, p42.Trend[0], "2026-10-20", 1, 5, 1, ptrFloat64(9000), ptrFloat64(9000))
	assertDailyTrend(t, p42.Trend[1], "2026-10-21", 1, 11, 1, ptrFloat64(4000), ptrFloat64(4000))
	assertDailyTrend(t, p43.Trend[0], "2026-10-21", 1, 7, 0, nil, nil)

	if len(stats.ByProvider) != 2 || stats.ByProvider[0].Provider != "anthropic" ||
		stats.ByProvider[1].Provider != "openai" {
		t.Fatalf("by_provider order = %+v", stats.ByProvider)
	}
	anthropic, openai := stats.ByProvider[0], stats.ByProvider[1]
	if anthropic.Runs != 2 || anthropic.CostMicros != 5 || anthropic.DurationSamples != 1 {
		t.Fatalf("anthropic stats = %+v", anthropic)
	}
	assertStatDuration(t, "anthropic avg", anthropic.AvgDurationMS, 1000)
	assertStatDuration(t, "anthropic p95", anthropic.P95DurationMS, 1000)
	anthropicDays := trendIndex(t, anthropic.Trend)
	assertDailyTrend(t, anthropicDays["2026-10-20"], "2026-10-20", 1, 5, 1, ptrFloat64(1000), ptrFloat64(1000))
	assertDailyTrend(t, anthropicDays["2026-10-21"], "2026-10-21", 1, 0, 0, nil, nil)
	if openai.Runs != 2 || openai.CostMicros != 18 || openai.DurationSamples != 2 {
		t.Fatalf("openai stats = %+v", openai)
	}
	assertStatDuration(t, "openai avg", openai.AvgDurationMS, 2500)
	assertStatDuration(t, "openai p95", openai.P95DurationMS, 3000)
	if len(openai.Trend) != 1 {
		t.Fatalf("openai trend = %+v", openai.Trend)
	}
	assertDailyTrend(t, openai.Trend[0], "2026-10-21", 2, 18, 2, ptrFloat64(2500), ptrFloat64(3000))

	wantBlocks := []string{"code-review", "review.intent", "review.standards", "synthesize", "verify"}
	if len(stats.ByBlock) != len(wantBlocks) {
		t.Fatalf("by_block = %+v, want %v", stats.ByBlock, wantBlocks)
	}
	for index, block := range stats.ByBlock {
		if block.Block != wantBlocks[index] {
			t.Fatalf("by_block[%d] = %q, want %q", index, block.Block, wantBlocks[index])
		}
	}
	blocks := make(map[string]store.BlockStats, len(stats.ByBlock))
	for _, block := range stats.ByBlock {
		blocks[block.Block] = block
	}
	if blocks["code-review"].Runs != 1 || blocks["code-review"].CostMicros != 5 ||
		blocks["code-review"].DurationSamples != 1 || blocks["code-review"].Trend[0].Day != "2026-10-20" {
		t.Fatalf("code-review block = %+v", blocks["code-review"])
	}
	assertStatDuration(t, "code-review avg", blocks["code-review"].AvgDurationMS, 1000)
	if blocks["verify"].Runs != 1 || blocks["verify"].DurationSamples != 1 ||
		blocks["review.intent"].Runs != 1 || blocks["review.intent"].DurationSamples != 0 ||
		blocks["review.intent"].AvgDurationMS != nil || blocks["review.intent"].P95DurationMS != nil {
		t.Fatalf("block counts = %+v", blocks)
	}
	assertStatDuration(t, "review.standards avg", blocks["review.standards"].AvgDurationMS, 2000)
	assertStatDuration(t, "synthesize avg", blocks["synthesize"].AvgDurationMS, 3000)

	// ProjectID filters the node scan as well as the workflow scan.
	projectStats, err := st.Stats(ctx, store.StatsFilter{From: day1, To: to, ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats project 42: %v", err)
	}
	if projectStats.Runs != 2 || projectStats.CostMicros != 16 || projectStats.DurationSamples != 2 {
		t.Fatalf("project-filtered totals = %+v", projectStats)
	}
	if len(projectStats.ByProject) != 1 || projectStats.ByProject[0].ProjectID != 42 {
		t.Fatalf("project-filtered projects = %+v", projectStats.ByProject)
	}
	if len(projectStats.ByProvider) != 2 || len(projectStats.ByBlock) != 3 {
		t.Fatalf("project-filtered groups = providers %+v blocks %+v", projectStats.ByProvider, projectStats.ByBlock)
	}
	for _, provider := range projectStats.ByProvider {
		if provider.Provider == "openai" && (provider.Runs != 1 || provider.CostMicros != 11) {
			t.Fatalf("project-filtered openai = %+v", provider)
		}
	}
	for _, block := range projectStats.ByBlock {
		if block.Block == "review.standards" || block.Block == "review.intent" {
			t.Fatalf("project-filtered block leaked project 43: %+v", projectStats.ByBlock)
		}
	}

	// From is inclusive and To is exclusive.
	firstDay, err := st.Stats(ctx, store.StatsFilter{From: day1, To: day2})
	if err != nil {
		t.Fatalf("Stats first day: %v", err)
	}
	if firstDay.Runs != 1 || firstDay.CostMicros != 5 || len(firstDay.ByBlock) != 2 ||
		len(firstDay.ByProvider) != 1 || firstDay.ByProvider[0].Provider != "anthropic" {
		t.Fatalf("first-day stats = %+v", firstDay)
	}
}

func TestStatsTrendsEmptyCollectionsAreKnown(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)

	stats, err := st.Stats(ctx, store.StatsFilter{ProjectID: ptrInt64(999)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if stats.Trend == nil || len(stats.Trend) != 0 ||
		stats.ByProvider == nil || len(stats.ByProvider) != 0 ||
		stats.ByBlock == nil || len(stats.ByBlock) != 0 {
		t.Fatalf("empty stats = %+v", stats)
	}
	if stats.DurationSamples != 0 || stats.AvgDurationMS != nil || stats.P95DurationMS != nil {
		t.Fatalf("empty durations = %+v", stats)
	}

	unfiltered, err := st.Stats(ctx, store.StatsFilter{})
	if err != nil {
		t.Fatalf("Stats without filters: %v", err)
	}
	if unfiltered.Runs != 0 || unfiltered.Trend == nil || unfiltered.ByProvider == nil ||
		unfiltered.ByBlock == nil {
		t.Fatalf("unfiltered stats = %+v", unfiltered)
	}
}

func TestStatsGroupsWorkflowsByCreatedDayNotCompletionDay(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)

	day1 := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)
	created := day1.Add(23*time.Hour + 50*time.Minute)
	started := day1.Add(23*time.Hour + 51*time.Minute)
	finished := day2.Add(10 * time.Minute)

	rev := mustCreateStatsWorkflow(t, st, "wf-cross-day", statsMeta(42, "review", 9),
		statsTrendSpec("code-review"), created)
	rev = mustAppendEvent(t, st, "wf-cross-day", rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, started))
	rev = mustAppendEvent(t, st, "wf-cross-day", rev, queuedEvent(t, "a", 1, started))
	rev = mustAppendEvent(t, st, "wf-cross-day", rev, startedEvent(t, "a", 1, "anthropic", "m", day1.Add(23*time.Hour+52*time.Minute)))
	rev = mustAppendEvent(t, st, "wf-cross-day", rev, completedEvent(t, "a", 1, 5, day2.Add(9*time.Minute)))
	if _, err := st.Append(ctx, "wf-cross-day", rev,
		testEvent(t, dag.EventWorkflowCompleted, dag.WorkflowTerminalPayload{}, finished)); err != nil {
		t.Fatalf("complete wf-cross-day: %v", err)
	}

	stats, err := st.Stats(ctx, store.StatsFilter{From: day1, To: day2})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	// The workflow was created on day 1 and finished on day 2. It belongs to
	// day 1; a completion-time bucket would leave the trend empty.
	if len(stats.Trend) != 1 || stats.Trend[0].Day != "2026-10-20" || stats.Trend[0].Runs != 1 {
		t.Fatalf("workflow trend = %+v", stats.Trend)
	}
	if len(stats.ByProject) != 1 || len(stats.ByProject[0].Trend) != 1 ||
		stats.ByProject[0].Trend[0].Day != "2026-10-20" {
		t.Fatalf("project trend = %+v", stats.ByProject)
	}
	if len(stats.ByProvider) != 1 || len(stats.ByProvider[0].Trend) != 1 ||
		stats.ByProvider[0].Trend[0].Day != "2026-10-20" {
		t.Fatalf("provider trend = %+v", stats.ByProvider)
	}
	if len(stats.ByBlock) != 1 || len(stats.ByBlock[0].Trend) != 1 ||
		stats.ByBlock[0].Trend[0].Day != "2026-10-20" {
		t.Fatalf("block trend = %+v", stats.ByBlock)
	}
}
