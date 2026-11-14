package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// completeReview writes a review workflow with a verdict and a completed
// status. The spec has one code node so the projected row is complete.
func completeReview(t *testing.T, st *Store, id string, projectID int64, verdict string, status string, at time.Time) {
	t.Helper()
	rev := mustCreateStatsWorkflow(t, st, id, statsMeta(projectID, "review", 9),
		statsTrendSpec("plan"), at)
	start := at.Add(time.Minute)
	rev = mustAppendEvent(t, st, id, rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, start))
	rev = mustAppendEvent(t, st, id, rev, queuedEvent(t, "a", 1, start))
	rev = mustAppendEvent(t, st, id, rev, startedEvent(t, "a", 1, "", "", start.Add(time.Second)))
	rev = mustAppendEvent(t, st, id, rev, completedEvent(t, "a", 1, start.Add(2*time.Second)))
	payload := dag.WorkflowTerminalPayload{}
	eventType := dag.EventWorkflowCompleted
	if status == "failed" {
		eventType = dag.EventWorkflowFailed
	}
	if _, err := st.Append(context.Background(), id, rev, testEvent(t, eventType, payload, start.Add(3*time.Second))); err != nil {
		t.Fatalf("complete %s: %v", id, err)
	}
	if verdict != "" {
		if err := st.SetWorkflowVerdict(context.Background(), id, verdict); err != nil {
			t.Fatalf("set verdict %s: %v", id, err)
		}
	}
}

func TestReviewStatsUseTheReviewDenominator(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t, newFakeClock())
	mustProject(t, st, 42)
	mustProject(t, st, 43)
	day1 := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	day2 := day1.Add(24 * time.Hour)

	completeReview(t, st, "wf-incomplete", 42, "INCOMPLETE", "completed", day1)
	completeReview(t, st, "wf-blocking", 42, "BLOCKING", "completed", day1.Add(time.Hour))
	completeReview(t, st, "wf-failed", 43, "", "failed", day2)

	// An onboarding run must stay out of the review numerator and denominator
	// but remain in the mixed denominator.
	rev := mustCreateStatsWorkflow(t, st, "wf-onboarding", statsMeta(42, "onboarding", 0),
		statsTrendSpec("synthesize"), day1.Add(2*time.Hour))
	start := day1.Add(2*time.Hour + time.Minute)
	rev = mustAppendEvent(t, st, "wf-onboarding", rev, testEvent(t, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{}, start))
	rev = mustAppendEvent(t, st, "wf-onboarding", rev, queuedEvent(t, "a", 1, start))
	rev = mustAppendEvent(t, st, "wf-onboarding", rev, startedEvent(t, "a", 1, "openai", "m", start.Add(time.Second)))
	rev = mustAppendEvent(t, st, "wf-onboarding", rev, completedEvent(t, "a", 1, start.Add(2*time.Second)))
	if _, err := st.Append(ctx, "wf-onboarding", rev,
		testEvent(t, dag.EventWorkflowCompleted, dag.WorkflowTerminalPayload{}, start.Add(3*time.Second))); err != nil {
		t.Fatalf("complete onboarding: %v", err)
	}

	stats, err := st.ReviewStats(ctx, store.ReviewStatsFilter{From: day1, To: day2.Add(24 * time.Hour)})
	if err != nil {
		t.Fatalf("ReviewStats: %v", err)
	}
	if stats.Runs != 3 || stats.Completed != 2 || stats.Incomplete != 1 || stats.Blocking != 1 || stats.Failed != 1 {
		t.Fatalf("review totals = %+v", stats)
	}
	if stats.BlockingRatio == nil || *stats.BlockingRatio != 1.0/3.0 {
		t.Fatalf("blocking_ratio = %v", stats.BlockingRatio)
	}
	if stats.IncompleteRatio == nil || *stats.IncompleteRatio != 1.0/3.0 {
		t.Fatalf("incomplete_ratio = %v", stats.IncompleteRatio)
	}
	if stats.AllRuns != 4 || stats.AllBlocking != 1 || stats.AllBlockingRatio == nil || *stats.AllBlockingRatio != 0.25 {
		t.Fatalf("mixed denominator = %+v", stats)
	}
	if len(stats.ByProject) != 2 {
		t.Fatalf("by_project = %+v", stats.ByProject)
	}
	projects := map[int64]store.ReviewProjectStats{}
	for _, project := range stats.ByProject {
		projects[project.ProjectID] = project
	}
	if projects[42].Runs != 2 || projects[42].Incomplete != 1 || projects[42].Blocking != 1 {
		t.Fatalf("project 42 = %+v", projects[42])
	}
	if projects[43].Runs != 1 || projects[43].Incomplete != 0 {
		t.Fatalf("project 43 = %+v", projects[43])
	}
	days := map[string]store.ReviewDailyStats{}
	for _, entry := range stats.Trend {
		days[entry.Day] = entry
	}
	if days["2026-10-20"].Runs != 2 || days["2026-10-20"].Incomplete != 1 || days["2026-10-20"].Blocking != 1 {
		t.Fatalf("day 1 = %+v", days["2026-10-20"])
	}
	if days["2026-10-21"].Runs != 1 {
		t.Fatalf("day 2 = %+v", days["2026-10-21"])
	}

	filtered, err := st.ReviewStats(ctx, store.ReviewStatsFilter{
		From: day1, To: day2, ProjectID: ptrInt64(43),
	})
	if err != nil {
		t.Fatalf("ReviewStats project filter: %v", err)
	}
	if filtered.Runs != 0 {
		t.Fatalf("project 43 day 1 runs = %d, want 0", filtered.Runs)
	}
	filtered, err = st.ReviewStats(ctx, store.ReviewStatsFilter{
		From: day2, To: day2.Add(24 * time.Hour), ProjectID: ptrInt64(43),
	})
	if err != nil {
		t.Fatalf("ReviewStats project filter day 2: %v", err)
	}
	if filtered.Runs != 1 || filtered.Failed != 1 || filtered.Blocking != 0 {
		t.Fatalf("project 43 day 2 = %+v", filtered)
	}
}
