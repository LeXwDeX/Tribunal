package review

import (
	"fmt"
	"strings"
	"testing"
)

const indexGapMessage = "block code-review: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index abcdef01, target 33333333)"

func indexGapSignals(nodes ...string) []UnresolvedSignal {
	signals := make([]UnresolvedSignal, 0, len(nodes))
	for _, node := range nodes {
		signals = append(signals, UnresolvedSignal{Node: node, Kind: UnresolvedEvidence, Message: indexGapMessage})
	}
	return signals
}

// distinctGaps returns n evidence gaps with different unmapped messages, so
// each one is its own cause.
func distinctGaps(n int) []UnresolvedSignal {
	signals := make([]UnresolvedSignal, 0, n)
	for i := 0; i < n; i++ {
		signals = append(signals, UnresolvedSignal{
			Node: fmt.Sprintf("rv.p%d", i), Kind: UnresolvedEvidence, Message: fmt.Sprintf("pipeline log %d unavailable", i),
		})
	}
	return signals
}

// Several nodes withheld from the code graph for the same reason are one cause:
// they take one slot, name the first node and say how many nodes share it.
func TestIncompleteSummaryCollapsesNodesSharingACauseIntoOneSlot(t *testing.T) {
	summary := incompleteSummary(indexGapSignals("explore", "cr.p1", "cr.p2", "rv.p1", "reasoner", "validate"))
	if !strings.HasPrefix(summary, unresolvedSummaryPrefix) {
		t.Fatalf("summary lost the constant prefix: %q", summary)
	}
	if got := strings.Count(summary, "blocked by workspace index identity"); got != 1 {
		t.Fatalf("summary lists the cause %d times, want once: %q", got, summary)
	}
	if !strings.Contains(summary, "explore 等 6 个节点: "+indexGapMessage) {
		t.Fatalf("summary does not name the first node and the node count: %q", summary)
	}
	if strings.Contains(summary, "另有") {
		t.Fatalf("summary reports an overflow for a single cause: %q", summary)
	}
}

// The repeats must not push the other gaps out behind the 8-item cap.
func TestIncompleteSummaryKeepsOtherGapsBehindCollapsedCause(t *testing.T) {
	signals := indexGapSignals("explore", "cr.p1", "cr.p2", "cr.p3", "rv.p1", "rv.p2", "reasoner", "validate", "cr.p4", "rv.p3")
	signals = append(signals, distinctGaps(5)...)
	summary := incompleteSummary(signals)
	for i := 0; i < 5; i++ {
		want := fmt.Sprintf("rv.p%d: pipeline log %d unavailable", i, i)
		if !strings.Contains(summary, want) {
			t.Fatalf("summary dropped %q behind the repeated cause: %q", want, summary)
		}
	}
	if strings.Contains(summary, "另有") {
		t.Fatalf("six causes fit the cap but the summary reports an overflow: %q", summary)
	}
}

// The cap counts causes, not signals: 1 collapsed cause + 9 others = 10 causes,
// 8 are listed and 2 are reported as the overflow.
func TestIncompleteSummaryCapCountsCauses(t *testing.T) {
	signals := indexGapSignals("explore", "cr.p1", "cr.p2", "cr.p3", "rv.p1")
	signals = append(signals, distinctGaps(9)...)
	summary := incompleteSummary(signals)
	if !strings.HasSuffix(summary, "；另有 2 项") {
		t.Fatalf("summary does not report the 2 causes beyond the cap: %q", summary)
	}
	// 8 listed items use 7 separators; the overflow note adds one more.
	if got := strings.Count(summary, "；"); got != 8 {
		t.Fatalf("summary has %d separators, want 8 (8 items and the overflow): %q", got, summary)
	}
}

// A different cause is not merged: the commits are part of the cause.
func TestIncompleteSummaryKeepsDifferentIndexIdentityCausesApart(t *testing.T) {
	signals := indexGapSignals("explore", "cr.p1")
	signals = append(signals, UnresolvedSignal{
		Node: "rv.p1", Kind: UnresolvedEvidence,
		Message: "block review: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index unknown, target unknown)",
	})
	summary := incompleteSummary(signals)
	if got := strings.Count(summary, "blocked by workspace index identity"); got != 2 {
		t.Fatalf("summary lists %d index identity causes, want 2: %q", got, summary)
	}
	if !strings.Contains(summary, "explore 等 2 个节点: ") || !strings.Contains(summary, "rv.p1: block review") {
		t.Fatalf("summary did not collapse only the shared cause: %q", summary)
	}
}

// Two signals on one node with the same cause stay a single plain node.
func TestIncompleteSummaryCollapseOnOneNodeKeepsThePlainNode(t *testing.T) {
	signals := []UnresolvedSignal{
		{Node: "explore", Kind: UnresolvedEvidence, Message: `block explore: MCP tools unavailable: tool "mcp__a" blocked by workspace index identity`},
		{Node: "explore", Kind: UnresolvedEvidence, Message: `block explore: MCP tools unavailable: tool "mcp__b" blocked by workspace index identity`},
	}
	summary := incompleteSummary(signals)
	if strings.Contains(summary, "等") {
		t.Fatalf("one node was reported as several: %q", summary)
	}
	if !strings.Contains(summary, `具体缺口：explore: block explore: MCP tools unavailable: tool "mcp__a"`) || strings.Contains(summary, "mcp__b") {
		t.Fatalf("summary does not keep exactly the first signal of the cause: %q", summary)
	}
}

// Unmapped messages say nothing about a shared cause, so they are never merged.
func TestIncompleteSummaryDoesNotMergeUnmappedMessages(t *testing.T) {
	summary := incompleteSummary(distinctGaps(4))
	for i := 0; i < 4; i++ {
		if !strings.Contains(summary, fmt.Sprintf("pipeline log %d unavailable", i)) {
			t.Fatalf("summary dropped gap %d: %q", i, summary)
		}
	}
	if strings.Contains(summary, "等") {
		t.Fatalf("summary merged unrelated gaps: %q", summary)
	}
}

// The collapsed form is part of the published contract: the renderer must read
// it back, list the cause once and keep every other cause.
func TestLocalizeIncompleteSummaryReadsCollapsedItems(t *testing.T) {
	signals := indexGapSignals("explore", "cr.p1", "cr.p2", "rv.p1")
	signals = append(signals,
		UnresolvedSignal{Node: "cr.p1", Kind: UnresolvedEvidence, Message: "lane cr.p1 (code-review) did not complete: skipped"},
		UnresolvedSignal{Node: "validate", Kind: UnresolvedBlocking, Message: "validate lane reported FAIL without a P0/P1 finding in the review scope"},
	)
	text, ok := localizeIncompleteSummary(incompleteSummary(signals), 0, 0)
	if !ok {
		t.Fatal("collapsed summary was not recognized")
	}
	if got := strings.Count(text, indexMismatchLabel); got != 1 {
		t.Fatalf("published summary lists the cause %d times, want once:\n%s", got, text)
	}
	for _, want := range []string{"代码审查通道未完成（已跳过）", "验证通道判定失败"} {
		if !strings.Contains(text, want) {
			t.Fatalf("published summary lacks %q:\n%s", want, text)
		}
	}
	for _, banned := range []string{"等 4 个节点", "explore", "另有"} {
		if strings.Contains(text, banned) {
			t.Fatalf("published summary leaks %q:\n%s", banned, text)
		}
	}
}

// The node count suffix must not hide the node from the fallback label of an
// older item whose message is not a known template.
func TestLocalizeIncompleteSummaryStripsTheNodeCountForFallbackLabels(t *testing.T) {
	stored := unresolvedSummaryPrefix + "explore 等 3 个节点: some unknown english message"
	text, ok := localizeIncompleteSummary(stored, 0, 0)
	if !ok {
		t.Fatal("summary was not recognized")
	}
	if !strings.Contains(text, "探索未完成，具体原因与日志见控制台。") {
		t.Fatalf("fallback label of the first node was lost:\n%s", text)
	}
}
