package review

import (
	"fmt"
	"strings"
	"testing"
)

// legacyToolGapSignals mimics a workflow stored before #603: one gap per bound
// tool and node, all with the same cause.
func legacyToolGapSignals(nodes []string, tools int) []UnresolvedSignal {
	var signals []UnresolvedSignal
	for _, node := range nodes {
		for i := 0; i < tools; i++ {
			signals = append(signals, UnresolvedSignal{
				Node:    node,
				Kind:    UnresolvedEvidence,
				Message: fmt.Sprintf("block %s: MCP tools unavailable: tool %q blocked by workspace index identity", node, fmt.Sprintf("mcp__codebase-memory__t%d", i)),
			})
		}
	}
	return signals
}

// A legacy summary counted every per-tool gap, so its "另有 N 项" overstated
// the distinct causes. The console text counts the structured signals instead.
func TestIncompleteSummaryTextForCountsCausesNotLegacyGaps(t *testing.T) {
	signals := legacyToolGapSignals([]string{"explore", "cr.p1", "rv.p1", "reasoner", "validate"}, 7)
	stored := legacyIncompleteSummary(signals)
	if !strings.Contains(stored, "另有 ") {
		t.Fatalf("fixture must carry a legacy overflow count:\n%s", stored)
	}
	text, ok := IncompleteSummaryTextFor(stored, signals)
	if !ok {
		t.Fatal("incomplete summary was not recognized")
	}
	if strings.Contains(text, "另有") {
		t.Fatalf("console summary reports an overflow for one cause:\n%s", text)
	}
	cause := UnresolvedText(signals[0])
	if got := strings.Count(text, strings.TrimSuffix(cause, "。")); got != 1 {
		t.Fatalf("console summary lists the cause %d times, want once:\n%s", got, text)
	}
	if !strings.HasPrefix(text, unresolvedSummaryPrefix) {
		t.Fatalf("console summary lost the stored lead:\n%s", text)
	}
}

func TestIncompleteSummaryTextForCountsOverflowByCauseGroup(t *testing.T) {
	signals := append(legacyToolGapSignals([]string{"explore", "cr.p1"}, 5), distinctGaps(10)...)
	text, ok := IncompleteSummaryTextFor(incompleteSummary(signals), signals)
	if !ok {
		t.Fatal("incomplete summary was not recognized")
	}
	// 1 tool-gap cause + 10 distinct causes = 11 groups; 8 shown, 3 more.
	if !strings.Contains(text, "另有 3 项检查未完成") {
		t.Fatalf("overflow is not counted by cause group:\n%s", text)
	}
}

func TestIncompleteSummaryTextForKeepsEvidenceAndBlockingApart(t *testing.T) {
	signals := []UnresolvedSignal{
		{Node: "rv.p1", Kind: UnresolvedEvidence, Message: "pipeline log unavailable"},
		{Node: "rv.p2", Kind: UnresolvedBlocking, Message: "pipeline log unavailable"},
	}
	text, ok := IncompleteSummaryTextFor(incompleteSummary(signals), signals)
	if !ok {
		t.Fatal("incomplete summary was not recognized")
	}
	if got := strings.Count(text, "pipeline log unavailable"); got != 2 {
		t.Fatalf("evidence and blocking signals with one text were merged (%d):\n%s", got, text)
	}
}

func TestIncompleteSummaryTextForWithoutSignalsParsesTheStoredSummary(t *testing.T) {
	stored := incompleteSummary(distinctGaps(11))
	want, ok := IncompleteSummaryText(stored)
	if !ok {
		t.Fatal("stored summary was not recognized")
	}
	got, ok := IncompleteSummaryTextFor(stored, nil)
	if !ok || got != want {
		t.Fatalf("IncompleteSummaryTextFor without signals = %q, %v; want %q", got, ok, want)
	}
	if text, ok := IncompleteSummaryTextFor("已完成评审，未发现问题。", distinctGaps(2)); ok || text != "" {
		t.Fatalf("a non-incomplete summary produced console text %q", text)
	}
}

// legacyIncompleteSummary renders signals the way a pre-#603 binary stored
// them: the first items verbatim and every remaining signal counted.
func legacyIncompleteSummary(signals []UnresolvedSignal) string {
	var b strings.Builder
	b.WriteString(unresolvedSummaryPrefix)
	shown := signals
	if len(shown) > maxUnresolvedSummaryItems {
		shown = shown[:maxUnresolvedSummaryItems]
	}
	for i, signal := range shown {
		if i > 0 {
			b.WriteString("；")
		}
		b.WriteString(signal.Node + ": " + signal.Message)
	}
	if extra := len(signals) - len(shown); extra > 0 {
		fmt.Fprintf(&b, "；另有 %d 项", extra)
	}
	return b.String()
}
