package review

import (
	"strings"
	"testing"
)

const (
	indexIdentityLabel = "代码图谱索引提交与本 MR 目标提交不一致，本次未启用代码图谱工具，相关范围可能未被完整评审。"
	genericToolLabel   = "部分评审工具不可用，相关范围可能未被完整评审。"
)

func TestLocalizeToolGapNamesTheIndexIdentityCause(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "collapsed gap of one node",
			message: "block code-review: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index abcdef01, target 33333333)",
			want:    indexIdentityLabel,
		},
		{
			name:    "collapsed gap with the node prefix of a warning",
			message: "cr.p1: block code-review: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index unknown, target unknown)",
			want:    indexIdentityLabel,
		},
		{
			name:    "per-tool gap stored by an older workflow",
			message: `block explore: MCP tools unavailable: tool "mcp__codebase-memory__search_graph" blocked by workspace index identity`,
			want:    indexIdentityLabel,
		},
		{
			name:    "other unavailable tools keep the generic label",
			message: `block code-review: MCP tools unavailable: mcp: server "codebase-memory": connect failed`,
			want:    genericToolLabel,
		},
		{
			name:    "runtime unavailability keeps the generic label",
			message: `block review: MCP tools unavailable: tool "mcp__codebase-memory__trace_path" became unavailable`,
			want:    genericToolLabel,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, ok := localizeMachineMessage(tc.message)
			if !ok || text != tc.want {
				t.Fatalf("localizeMachineMessage(%q) = %q, %v; want %q", tc.message, text, ok, tc.want)
			}
		})
	}
}

// Nodes of different blocks report the same cause; the published summary lists
// it once and does not push other gaps out behind a long run of repeats.
func TestIncompleteSummaryListsIndexIdentityCauseOnce(t *testing.T) {
	var signals []UnresolvedSignal
	for _, lane := range []struct{ node, block string }{
		{"explore", "explore"}, {"cr.p1", "code-review"}, {"cr.p2", "code-review"},
		{"rv.p1", "review"}, {"reasoner", "reasoner"}, {"validate", "validate"},
	} {
		signals = append(signals, UnresolvedSignal{
			Node: lane.node, Kind: UnresolvedEvidence,
			Message: "block " + lane.block + ": MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index abcdef01, target 33333333)",
		})
	}
	text, ok := localizeIncompleteSummary(incompleteSummary(signals), 0, 0)
	if !ok {
		t.Fatal("summary was not localized")
	}
	if got := strings.Count(text, indexIdentityLabel); got != 1 {
		t.Fatalf("summary lists the index identity cause %d times, want once:\n%s", got, text)
	}
	if strings.Contains(text, "另有") || strings.Contains(text, genericToolLabel) {
		t.Fatalf("summary adds unrelated lines:\n%s", text)
	}
}
