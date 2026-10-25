package sqlite

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

func qualityWorkflow(t *testing.T, st *Store, workflowID string, projectID, mrIID int64, at time.Time, proposer, challenger string) {
	t.Helper()
	rev := mustCreateWorkflow(t, st, workflowID, reviewMeta(projectID, mrIID, "head-"+workflowID), at)
	rev = mustAppendEvent(t, st, workflowID, rev, queuedEvent(t, "a", 1, at))
	rev = mustAppendEvent(t, st, workflowID, rev, startedEvent(t, "a", 1, proposer, "m", at))
	rev = mustAppendEvent(t, st, workflowID, rev, queuedEvent(t, "b", 1, at))
	_ = mustAppendEvent(t, st, workflowID, rev, startedEvent(t, "b", 1, challenger, "m", at))
}

func qualityFinding(id, originProvider, challengeNode, challengeStatus string) store.Finding {
	return store.Finding{
		ID:              id,
		OriginNode:      "a",
		OriginIndex:     0,
		OriginProvider:  originProvider,
		Severity:        "P1",
		Category:        "logic",
		Title:           id,
		Body:            "body-" + id,
		ChallengeStatus: challengeStatus,
		ChallengeNode:   challengeNode,
	}
}

func TestStatsQualityByProvider(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	mustProject(t, st, 43)

	day1 := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	to := day1.Add(24 * time.Hour)

	qualityWorkflow(t, st, "wf-a", 42, 1, day1.Add(time.Hour), "claude", "gpt")
	if err := st.ReplaceFindings(ctx, "wf-a", []store.Finding{
		qualityFinding("q1", "claude", "b", "refuted"),
		qualityFinding("q2", "claude", "b", "refuted"),
		qualityFinding("q3", "claude", "b", "refuted"),
		qualityFinding("q4", "claude", "b", "confirmed"),
		qualityFinding("q5", "", "b", "refuted"),
		qualityFinding("q6", "meta", "b", "confirmed"),
	}); err != nil {
		t.Fatalf("ReplaceFindings wf-a: %v", err)
	}

	qualityWorkflow(t, st, "wf-b", 42, 2, day1.Add(2*time.Hour), "gpt", "qwen")
	if err := st.ReplaceFindings(ctx, "wf-b", []store.Finding{
		qualityFinding("q7", "gpt", "b", "refuted"),
		qualityFinding("q8", "", "b", "refuted"),
		qualityFinding("q9", "meta", "", "confirmed"),
		qualityFinding("q10", "", "", "refuted"),
	}); err != nil {
		t.Fatalf("ReplaceFindings wf-b: %v", err)
	}

	qualityWorkflow(t, st, "wf-other", 43, 1, day1.Add(3*time.Hour), "claude", "gpt")
	if err := st.ReplaceFindings(ctx, "wf-other", []store.Finding{qualityFinding("q-other", "claude", "b", "refuted")}); err != nil {
		t.Fatalf("ReplaceFindings wf-other: %v", err)
	}
	qualityWorkflow(t, st, "wf-to", 42, 3, to, "claude", "gpt")
	if err := st.ReplaceFindings(ctx, "wf-to", []store.Finding{qualityFinding("q-to", "claude", "b", "refuted")}); err != nil {
		t.Fatalf("ReplaceFindings wf-to: %v", err)
	}
	qualityWorkflow(t, st, "wf-before", 42, 4, day1.Add(-time.Nanosecond), "claude", "gpt")
	if err := st.ReplaceFindings(ctx, "wf-before", []store.Finding{qualityFinding("q-before", "claude", "b", "refuted")}); err != nil {
		t.Fatalf("ReplaceFindings wf-before: %v", err)
	}

	// Annotations are written after the Stats window. No quality count may
	// depend on annotated_at.
	clock.Advance(to.Add(48 * time.Hour).Sub(clock.Now()))
	for id, annotation := range map[string]string{
		"q1":       store.FindingAnnotationValid,
		"q2":       store.FindingAnnotationInvalid,
		"q3":       store.FindingAnnotationUncertain,
		"q5":       store.FindingAnnotationValid,
		"q7":       store.FindingAnnotationValid,
		"q8":       store.FindingAnnotationUncertain,
		"q9":       store.FindingAnnotationValid,
		"q-other":  store.FindingAnnotationValid,
		"q-to":     store.FindingAnnotationValid,
		"q-before": store.FindingAnnotationValid,
	} {
		workflowID := "wf-a"
		if id == "q7" || id == "q8" || id == "q9" {
			workflowID = "wf-b"
		}
		if id == "q-other" {
			workflowID = "wf-other"
		}
		if id == "q-to" {
			workflowID = "wf-to"
		}
		if id == "q-before" {
			workflowID = "wf-before"
		}
		if _, err := st.SetFindingAnnotation(ctx, workflowID, id, annotation, "alice"); err != nil {
			t.Fatalf("SetFindingAnnotation %s: %v", id, err)
		}
	}

	stats, err := st.Stats(ctx, store.StatsFilter{From: day1, To: to, ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := []store.ProviderQualityStats{
		{
			Provider: "claude", Proposed: 4, Reviewed: 2, Valid: 1, Invalid: 1,
			Accuracy: ptrFloat64(0.5), Overturned: 3, OverturnedRatio: ptrFloat64(0.75),
		},
		{
			Provider: "gpt", Proposed: 1, Reviewed: 1, Valid: 1,
			Accuracy: ptrFloat64(1), Overturned: 1, OverturnedRatio: ptrFloat64(1),
			Refuted: 4, RefutedReviewed: 3, FalseRefuted: 2, FalseKillRate: ptrFloat64(2.0 / 3.0),
		},
		{
			Provider: "meta", Proposed: 2, Reviewed: 1, Valid: 1,
			Accuracy: ptrFloat64(1), OverturnedRatio: ptrFloat64(0),
		},
		{
			Provider: "qwen", Refuted: 2, RefutedReviewed: 1, FalseRefuted: 1,
			FalseKillRate: ptrFloat64(1),
		},
	}
	if !reflect.DeepEqual(stats.QualityByProvider, want) {
		t.Fatalf("quality_by_provider = %+v, want %+v", stats.QualityByProvider, want)
	}

	empty, err := st.Stats(ctx, store.StatsFilter{
		From: day1.Add(48 * time.Hour), To: day1.Add(72 * time.Hour), ProjectID: ptrInt64(42),
	})
	if err != nil {
		t.Fatalf("Stats empty window: %v", err)
	}
	if empty.QualityByProvider == nil || len(empty.QualityByProvider) != 0 {
		t.Fatalf("empty quality_by_provider = %#v, want an empty array", empty.QualityByProvider)
	}
}

func TestStatsQualityMergesBothRolesForOneProvider(t *testing.T) {
	ctx := context.Background()
	clock := newFakeClock()
	st := openTestStore(t, clock)
	mustProject(t, st, 42)
	at := time.Date(2026, 10, 20, 1, 0, 0, 0, time.UTC)
	qualityWorkflow(t, st, "wf-same", 42, 1, at, "gpt", "gpt")
	if err := st.ReplaceFindings(ctx, "wf-same", []store.Finding{
		qualityFinding("s1", "gpt", "b", "refuted"),
		qualityFinding("s2", "gpt", "b", "refuted"),
		qualityFinding("s3", "gpt", "b", "confirmed"),
	}); err != nil {
		t.Fatalf("ReplaceFindings: %v", err)
	}
	for id, annotation := range map[string]string{
		"s1": store.FindingAnnotationValid,
		"s2": store.FindingAnnotationInvalid,
		"s3": store.FindingAnnotationValid,
	} {
		if _, err := st.SetFindingAnnotation(ctx, "wf-same", id, annotation, "alice"); err != nil {
			t.Fatalf("SetFindingAnnotation %s: %v", id, err)
		}
	}

	stats, err := st.Stats(ctx, store.StatsFilter{From: at.Add(-time.Hour), To: at.Add(time.Hour), ProjectID: ptrInt64(42)})
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	want := []store.ProviderQualityStats{{
		Provider: "gpt", Proposed: 3, Reviewed: 3, Valid: 2, Invalid: 1,
		Accuracy: ptrFloat64(2.0 / 3.0), Overturned: 2, OverturnedRatio: ptrFloat64(2.0 / 3.0),
		Refuted: 2, RefutedReviewed: 2, FalseRefuted: 1, FalseKillRate: ptrFloat64(0.5),
	}}
	if !reflect.DeepEqual(stats.QualityByProvider, want) {
		t.Fatalf("quality_by_provider = %+v, want %+v", stats.QualityByProvider, want)
	}
}
