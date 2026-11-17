package review

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 64_000 is MaxSuggestionRunes; the literal keeps this guard usable against
// builds that temporarily disable the contract limit.
const perfSuggestionRunes = 64_000

// TestRenderSummaryStaysBoundedUnderContractLimits guards the #493 review
// finding: 200 findings with maximum-size suggestions must render in linear
// time and without building the whole oversized note first. The thresholds are
// deliberately loose to stay stable on CI.
func TestRenderSummaryStaysBoundedUnderContractLimits(t *testing.T) {
	suggestion := strings.Repeat("@", perfSuggestionRunes)
	groups := make([]Group, 0, MaxFindings)
	for i := 0; i < MaxFindings; i++ {
		group := renderGroup(fmt.Sprintf("perf-%d", i), ActionPublish, SeverityP2, false, fmt.Sprintf("条目 %d", i), "正文。")
		group.Suggestion = suggestion
		groups = append(groups, group)
	}
	input := SummaryInput{
		Verdict: VerdictBlocking, Summary: "发现问题", Groups: groups, Fingerprint: "abc",
	}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	out, err := RenderSummary(input)
	elapsed := time.Since(start)
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	allocated := after.TotalAlloc - before.TotalAlloc
	if elapsed > 2*time.Second || allocated > 1_500*1024*1024 {
		t.Fatalf("RenderSummary took %v and allocated %d bytes, want under 2s and 1.5GiB", elapsed, allocated)
	}
	if n := len(out); n > MaxPublishedNoteBytes {
		t.Fatalf("summary note bytes = %d, want at most %d", n, MaxPublishedNoteBytes)
	}
	if !strings.HasSuffix(out, platformLimitNotice) {
		t.Fatal("over-limit summary lacks the platform notice")
	}
}

// TestRenderFindingCodeFenceIsLinear guards the quadratic fence search that a
// hostile evidence value could trigger.
func TestRenderFindingCodeFenceIsLinear(t *testing.T) {
	const backticks = 80_000
	group := mkGroup("fence-linear", 20, "logic", SeverityP2, "超长围栏", "正文。")
	group.Decision.Action = ActionMark
	group.Evidence = strings.Repeat("`", backticks)

	start := time.Now()
	out, err := RenderFinding(group, FindingRenderOptions{})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("RenderFinding with %d backticks took %v, want under 100ms", backticks, elapsed)
	}
	if !strings.Contains(out, strings.Repeat("`", backticks)) {
		t.Fatal("evidence content was clipped")
	}
	if strings.Count(out, "```")%2 != 0 {
		t.Fatal("fence search produced an unbalanced fence")
	}
}

// TestRenderSummaryOmitsTailEntriesOverBudget proves the incremental byte
// budget: entries are rendered one at a time, and once the next entry does not
// fit it is omitted whole (never cut mid-block) and the platform notice ends
// the note.
func TestRenderSummaryOmitsTailEntriesOverBudget(t *testing.T) {
	markers := make([]string, 0, 3)
	groups := make([]Group, 0, 3)
	for i := 1; i <= 3; i++ {
		marker := fmt.Sprintf("条目结束标记%d", i)
		markers = append(markers, marker)
		body := strings.Repeat("完整正文行。\n\n", 20_000) + "```\n示例代码\n```\n\n" + marker
		group := renderGroup(fmt.Sprintf("budget-%d", i), ActionPublish, SeverityP2, false, fmt.Sprintf("条目 %d", i), body)
		group.PositionOK = false
		group.Suggestion = ""
		groups = append(groups, group)
	}
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "发现问题", Groups: groups, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out); n > MaxPublishedNoteBytes {
		t.Fatalf("summary note bytes = %d, want at most %d", n, MaxPublishedNoteBytes)
	}
	if !strings.HasSuffix(out, platformLimitNotice) {
		t.Fatal("over-budget summary lacks the platform notice")
	}
	kept := strings.TrimSuffix(out, platformLimitNotice)
	if !strings.Contains(kept, markers[0]) {
		t.Fatal("the first complete entry was dropped")
	}
	if !strings.Contains(kept, markers[1]) {
		t.Fatal("the second complete entry was dropped")
	}
	if strings.Contains(out, markers[2]) {
		t.Fatal("the tail entry was cut instead of omitted whole")
	}
	if strings.Count(kept, "```")%2 != 0 {
		t.Fatal("the budget cut inside a code block")
	}
}
