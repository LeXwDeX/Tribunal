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

// #487 removed publish-key collision hints. Findings at one position share a
// discussion; when GitLab rejects the inline post, every affected finding stays
// in the summary without a misleading conflict hint.
func TestInlinePositionFallbackAfterRejectedDiscussion(t *testing.T) {
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
	job := f.seedReview(t, "position-fallback-rejected")
	if err := f.service.Handle(ctx, job); err != nil {
		t.Fatal(err)
	}
	if posts == 0 {
		t.Fatal("no inline position was posted, so the fallback path did not run")
	}
	f.gitlab.mu.Lock()
	defer f.gitlab.mu.Unlock()
	if len(f.gitlab.notes) != 1 {
		t.Fatalf("notes = %d", len(f.gitlab.notes))
	}
	body := f.gitlab.notes[0].Body
	for _, misleading := range []string{"已保留在摘要中，未创建行内讨论", "发布标识冲突", "历史讨论内容"} {
		if strings.Contains(body, misleading) {
			t.Fatalf("misleading hint %q after a rejected inline post:\n%s", misleading, body)
		}
	}
	if !strings.Contains(body, "bad assignment") || !strings.Contains(body, "的写法") {
		t.Fatalf("rejected findings are missing from the summary:\n%s", body)
	}
}
