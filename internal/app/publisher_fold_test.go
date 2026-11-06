package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func foldFinding(category, title, body string) review.FindingOut {
	return review.FindingOut{
		Path: "main.go", Line: 2, Side: "new", Severity: review.SeverityP2, Category: category,
		Title: title, Body: body, Evidence: "bad := 1", Confidence: review.ConfidenceHigh,
	}
}

// duplicateFindings mirrors the real acceptance case: one problem reported
// under three different categories with paraphrased Chinese text.
func duplicateFindings() []review.FindingOut {
	return []review.FindingOut{
		foldFinding("correctness", "split_evenly 未校验 people，非法人数抛出不透明异常",
			"新增函数没有校验 people，total_cents // people 直接抛 ZeroDivisionError，传入负数时抛 IndexError，调用方无法统一处理。"),
		foldFinding("boundary_handling", "split_evenly 完全未校验 people，0 或负值直接抛无关异常",
			"这个新函数对人数没有任何前置校验：people=0 时 total_cents // people 抛 ZeroDivisionError，people<0 时抛 IndexError，调用方拿到无关异常。"),
		foldFinding("error-handling", "split_evenly 未校验 people 导致底层异常",
			"当调用方传入 people = 0 时 total_cents // people 直接抛 ZeroDivisionError，people < 0 时抛 IndexError，调用方拿到与业务无关的底层异常。"),
	}
}

func publishWithFindings(t *testing.T, f *reviewFixture, findings []review.FindingOut) (*capturedDiscussionRequest, store.Workflow) {
	t.Helper()
	f.runner.findings = findings
	captured := &capturedDiscussionRequest{}
	useInlineGitLab(t, f, func(w http.ResponseWriter, r *http.Request) {
		ensureDiscussionPath(t, r)
		captured.capture(r)
		requests, _, _, _ := captured.snapshot()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		fmt.Fprintf(w, `{"id":"fold-disc-%d","notes":[]}`, requests)
	})
	job := f.seedReview(t, "job-fold")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	return captured, *workflow
}

func TestPublisherFoldsNearDuplicatesIntoOneDiscussion(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	captured, workflow := publishWithFindings(t, f, duplicateFindings())

	requests, body, _, decodeErr := captured.snapshot()
	if decodeErr != nil || requests != 1 {
		t.Fatalf("inline discussions = %d (decode %v), want exactly 1", requests, decodeErr)
	}
	if !strings.Contains(body, "相关描述") {
		t.Fatalf("discussion lacks the related descriptions:\n%s", body)
	}
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 3 {
		t.Fatalf("stored findings = %d, want the 3 original groups", len(findings))
	}
	keys := map[string]int{}
	for _, finding := range findings {
		if finding.Decision != string(review.ActionPublish) || strings.Contains(finding.DecisionReason, "presentation") {
			t.Fatalf("folding leaked into stored finding: decision=%q reason=%q", finding.Decision, finding.DecisionReason)
		}
		if finding.PublishKey != "" {
			keys[finding.PublishKey]++
		}
	}
	if len(keys) != 1 {
		t.Fatalf("only the displayed group may own a publish key, got %v", keys)
	}
	f.gitlab.mu.Lock()
	summary := f.gitlab.notes[0].Body
	f.gitlab.mu.Unlock()
	if !strings.Contains(summary, "P2** × 1") && !strings.Contains(summary, "P2 × 1") {
		t.Fatalf("summary counts must reflect one posted problem:\n%s", summary)
	}

	// Republishing the same workflow must not create another discussion.
	if err := f.publisher.Publish(context.Background(), workflow.ID); err != nil {
		t.Fatalf("republish: %v", err)
	}
	if requests, _, _, _ := captured.snapshot(); requests != 1 {
		t.Fatalf("republish created extra discussions: %d", requests)
	}
	findings, _ = f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
	for _, finding := range findings {
		if finding.Decision != string(review.ActionPublish) {
			t.Fatalf("republish changed stored decision: %q", finding.Decision)
		}
	}
}

func TestPublisherFoldedGroupsSharingPublishKeyPostOneDiscussion(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	same := func(category string) review.FindingOut {
		return foldFinding(category, "split_evenly 未校验 people 导致异常",
			"split_evenly 未校验 people，total_cents // people 抛出 ZeroDivisionError，调用方拿到无关异常。")
	}
	captured, workflow := publishWithFindings(t, f, []review.FindingOut{same("correctness"), same("error-handling")})
	if requests, body, _, _ := captured.snapshot(); requests != 1 || strings.Contains(body, "发布标识冲突") {
		t.Fatalf("same-key duplicates: discussions = %d, want 1", requests)
	}
	f.gitlab.mu.Lock()
	summary := f.gitlab.notes[0].Body
	f.gitlab.mu.Unlock()
	if strings.Contains(summary, "发布标识冲突") {
		t.Fatalf("misleading publish-key conflict warning:\n%s", summary)
	}
	findings, _ := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
	keyed := 0
	for _, finding := range findings {
		if finding.PublishKey != "" {
			keyed++
		}
	}
	if len(findings) != 2 || keyed != 1 {
		t.Fatalf("stored rows=%d with keys=%d, want 2 rows and a single publish key", len(findings), keyed)
	}
}

func TestPublisherFoldsEvenWhenAnotherPathIsIgnored(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = append(f.gitlab.diffs, gitlab.FileDiff{NewPath: "go.sum", Diff: "@@ -1 +1 @@\n-a\n+b\n"})
	f.defaults.Ignore = []string{"**/*.sum"}
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	captured, workflow := publishWithFindings(t, f, duplicateFindings())
	// An unrelated ignored file (#357) only withholds its own path, so the
	// folded problem is still posted as one inline discussion listing the
	// related descriptions. This is the end-to-end regression for #357.
	requests, body, _, decodeErr := captured.snapshot()
	if decodeErr != nil || requests != 1 || !strings.Contains(body, "相关描述") {
		t.Fatalf("inline discussions = %d (decode %v), want exactly 1 with related descriptions:\n%s", requests, decodeErr, body)
	}
	f.gitlab.mu.Lock()
	summary := f.gitlab.notes[0].Body
	f.gitlab.mu.Unlock()
	if !strings.Contains(summary, "P2** × 1") && !strings.Contains(summary, "P2 × 1") {
		t.Fatalf("summary counts must reflect one posted problem:\n%s", summary)
	}
	findings, _ := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
	for _, finding := range findings {
		if finding.Decision != string(review.ActionPublish) {
			t.Fatalf("stored decision changed: %q", finding.Decision)
		}
	}
}
