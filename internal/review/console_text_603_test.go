package review

import (
	"strings"
	"testing"
)

func TestUnresolvedCategoryNamesToolGapsApartFromEvidenceGaps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		signal UnresolvedSignal
		want   string
	}{
		{
			name:   "collapsed index identity gap",
			signal: UnresolvedSignal{Node: "explore", Kind: UnresolvedEvidence, Message: indexGapMessage},
			want:   UnresolvedCategoryTool,
		},
		{
			name: "per-tool gap of an older workflow",
			signal: UnresolvedSignal{Node: "explore", Kind: UnresolvedEvidence,
				Message: `block explore: MCP tools unavailable: tool "mcp__x" blocked by workspace index identity`},
			want: UnresolvedCategoryTool,
		},
		{
			name: "server connect failure",
			signal: UnresolvedSignal{Node: "cr.p1", Kind: UnresolvedEvidence,
				Message: `block code-review: MCP tools unavailable: mcp: server "codebase-memory": connect failed`},
			want: UnresolvedCategoryTool,
		},
		{
			name:   "evidence gap",
			signal: UnresolvedSignal{Node: "coverage", Kind: UnresolvedEvidence, Message: "pipeline log unavailable"},
			want:   UnresolvedEvidence,
		},
		{
			name:   "model gap that mentions the phrase mid-text",
			signal: UnresolvedSignal{Node: "rv.p1", Kind: UnresolvedEvidence, Message: "the build log says MCP tools unavailable: retry later"},
			want:   UnresolvedEvidence,
		},
		{
			name: "model gap that mentions the index identity mid-text",
			signal: UnresolvedSignal{Node: "rv.p1", Kind: UnresolvedEvidence,
				Message: "evidence is blocked by workspace index identity (index abcdef01, target 33333333)"},
			want: UnresolvedEvidence,
		},
		{
			name:   "the warning form with a node is not the shape of a signal",
			signal: UnresolvedSignal{Node: "cr.p1", Kind: UnresolvedEvidence, Message: "cr.p1: " + indexGapMessage},
			want:   UnresolvedEvidence,
		},
		{
			name:   "a blocking signal is never a tool gap",
			signal: UnresolvedSignal{Node: "cr.p1", Kind: UnresolvedBlocking, Message: indexGapMessage},
			want:   UnresolvedBlocking,
		},
		{
			name:   "review blocking",
			signal: UnresolvedSignal{Node: "review.gate", Kind: UnresolvedBlocking, Message: "review gate reported BLOCKING with no accepted findings"},
			want:   UnresolvedBlocking,
		},
		{
			name:   "unknown kind is passed through, never invented",
			signal: UnresolvedSignal{Node: "x", Kind: "future_kind", Message: "something"},
			want:   "future_kind",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := UnresolvedCategory(tc.signal); got != tc.want {
				t.Fatalf("UnresolvedCategory(%+v) = %q, want %q", tc.signal, got, tc.want)
			}
		})
	}
}

func TestUnresolvedTextIsChineseForKnownMessagesAndRawOtherwise(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		want    string
	}{
		{"index mismatch", indexGapMessage, indexMismatchLabel},
		{"legacy per-tool gap", `block explore: MCP tools unavailable: tool "mcp__x" blocked by workspace index identity`, indexUnconfirmedLabel},
		{"generic tool gap", `block review: MCP tools unavailable: mcp: server "x": connect failed`, genericToolLabel},
		{"lane did not complete", "lane cr.p1 (code-review) did not complete: skipped", "代码审查通道未完成（已跳过）：该通道未运行，建议检查评审配置后重新触发评审。"},
		{"no mapping keeps the raw message", "pipeline log unavailable", "pipeline log unavailable"},
		{"model prose that mentions the phrase keeps its text", "the build log says MCP tools unavailable: retry later", "the build log says MCP tools unavailable: retry later"},
		{
			"model prose that mentions the index identity keeps its text",
			"evidence is blocked by workspace index identity (index abcdef01, target 33333333)",
			"evidence is blocked by workspace index identity (index abcdef01, target 33333333)",
		},
		{"chinese prose stays", "缺少流水线日志", "缺少流水线日志"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := UnresolvedText(UnresolvedSignal{Node: "n", Kind: UnresolvedEvidence, Message: tc.message})
			if got != tc.want {
				t.Fatalf("UnresolvedText(%q) = %q, want %q", tc.message, got, tc.want)
			}
		})
	}
}

func TestIncompleteSummaryTextIsPlainChinese(t *testing.T) {
	signals := indexGapSignals("explore", "cr.p1", "cr.p2")
	signals = append(signals,
		UnresolvedSignal{Node: "cr.p1", Kind: UnresolvedEvidence, Message: "lane cr.p1 (code-review) did not complete: skipped"},
		UnresolvedSignal{Node: "coverage", Kind: UnresolvedEvidence, Message: "pipeline log unavailable"},
	)
	text, ok := IncompleteSummaryText(incompleteSummary(signals))
	if !ok {
		t.Fatal("incomplete summary was not recognized")
	}
	if !strings.HasPrefix(text, unresolvedSummaryPrefix) {
		t.Fatalf("plain summary lost the stored lead:\n%s", text)
	}
	for _, want := range []string{
		strings.TrimSuffix(indexMismatchLabel, "。"),
		"代码审查通道未完成（已跳过）",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("plain summary lacks %q:\n%s", want, text)
		}
	}
	for _, banned := range []string{"**", "\n", "下一步", "MCP tools", "did not complete", "explore", "cr.p1", "等 3 个节点"} {
		if strings.Contains(text, banned) {
			t.Fatalf("plain summary contains %q:\n%s", banned, text)
		}
	}
	if got := strings.Count(text, strings.TrimSuffix(indexMismatchLabel, "。")); got != 1 {
		t.Fatalf("plain summary lists the cause %d times, want once:\n%s", got, text)
	}
	if strings.Contains(text, "。；") || strings.Contains(text, "。。") {
		t.Fatalf("plain summary has doubled punctuation:\n%s", text)
	}
}

func TestIncompleteSummaryTextReportsTheOverflow(t *testing.T) {
	text, ok := IncompleteSummaryText(incompleteSummary(distinctGaps(11)))
	if !ok {
		t.Fatal("incomplete summary was not recognized")
	}
	if !strings.Contains(text, "另有 3 项检查未完成") {
		t.Fatalf("plain summary does not report the overflow:\n%s", text)
	}
}

func TestIncompleteSummaryTextIgnoresOtherSummaries(t *testing.T) {
	for _, summary := range []string{"", "   ", "评审未完成：上游证据不足，不能给出 PASS。", "已完成评审，未发现问题。"} {
		if text, ok := IncompleteSummaryText(summary); ok || text != "" {
			t.Fatalf("IncompleteSummaryText(%q) = %q, %v; want no text", summary, text, ok)
		}
	}
}

// The plain text and the published Markdown come from one parse: the same
// causes in the same order.
func TestIncompleteSummaryTextAgreesWithThePublishedReasons(t *testing.T) {
	stored := incompleteSummary(append(indexGapSignals("explore", "cr.p1"), distinctGaps(2)...))
	plain, ok := IncompleteSummaryText(stored)
	if !ok {
		t.Fatal("plain summary missing")
	}
	published, ok := localizeIncompleteSummary(stored, 0, 0)
	if !ok {
		t.Fatal("published summary missing")
	}
	for _, reason := range []string{strings.TrimSuffix(indexMismatchLabel, "。"), "具体原因与日志见控制台"} {
		if !strings.Contains(plain, reason) || !strings.Contains(published, reason) {
			t.Fatalf("reason %q is not in both renderings:\nplain: %s\npublished: %s", reason, plain, published)
		}
	}
}
