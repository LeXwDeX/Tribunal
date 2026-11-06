package app

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
)

// The conflict hint is worded from the final summary view. Two groups share a
// publish key and so cannot be posted; a third near-duplicate group is posted
// first and the two fold under it. When that inline post is rejected (400) the
// third group falls back to the summary, the fold is recomputed, and the hint
// must describe the new placement instead of the first one.
func TestPublishKeyConflictHintFollowsFallbackReRender(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{OldPath: "main.go", NewPath: "main.go", Diff: "@@ -0,0 +1,6 @@\n+a\n+bad := 1\n+c\n+d\n+bad := 1\n+f\n"}}
	run := f.runner.Run
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		result, err := run(ctx, spec)
		if err == nil {
			out := result.Output.(*review.CodeReviewOut)
			twin := out.Findings[0]
			twin.Category = "security"
			third := out.Findings[0]
			third.Line, third.Title = 5, out.Findings[0].Title+" 的写法"
			out.Findings = append(out.Findings, twin, third)
		}
		return result, err
	}
	var posts int
	useInlineGitLab(t, f, func(w http.ResponseWriter, r *http.Request) {
		posts++
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"message":"line_code invalid"}`)
	})
	job := f.seedReview(t, "conflict-hint-fallback")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if posts == 0 {
		t.Fatal("the third group was never posted inline, so the fallback path did not run")
	}
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	if len(f.gitlab.notes) != 1 {
		t.Fatalf("notes = %d", len(f.gitlab.notes))
	}
	body := f.gitlab.notes[0].Body
	if strings.Contains(body, "已保留在摘要中，未创建行内讨论") {
		t.Fatalf("misleading conflict hint:\n%s", body)
	}
	if !strings.Contains(body, "发布标识冲突") {
		t.Fatalf("conflict hint missing:\n%s", body)
	}
	// After the fallback the conflicted pair is itself listed in the summary
	// (the rejected third group folds under or beside them), so the hint must
	// not claim every conflicted item sits under a related description.
	if strings.Contains(body, "部分问题的发布标识冲突，未创建行内讨论，以相关描述形式保留在") {
		t.Fatalf("hint still describes the pre-fallback placement:\n%s", body)
	}
	if !strings.Contains(body, "单独列出") {
		t.Fatalf("hint does not say the listed item stands on its own:\n%s", body)
	}
}
