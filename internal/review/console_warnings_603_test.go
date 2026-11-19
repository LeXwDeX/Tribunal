package review

import (
	"reflect"
	"testing"
)

const (
	neutralGapMessage = "block review: MCP tools unavailable: 7 bound tool(s) blocked by workspace index identity (index unknown, target unknown)"
	settledPassText   = "存在未解决的证据不足或阻断，不能判定为通过。"
)

// warningNodes are the node ids of the workflow the warnings belong to.
var warningNodes = []string{"explore", "cr.p1", "rv.p1", "reasoner", "validate"}

// One cause reported by several nodes is one line: the Chinese text followed by
// the nodes, in first-seen order and without repeats.
func TestConsoleWarningsAggregatesNodesPerText(t *testing.T) {
	got := ConsoleWarnings([]string{
		"explore: " + indexGapMessage,
		"cr.p1: " + indexGapMessage,
		"rv.p1: " + indexGapMessage,
		"explore: " + indexGapMessage,
	}, nil, warningNodes)
	want := []string{indexMismatchLabel + "（explore · cr.p1 · rv.p1）"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConsoleWarnings = %q, want %q", got, want)
	}
}

func TestConsoleWarningsKeepsDifferentTextsApartInFirstSeenOrder(t *testing.T) {
	got := ConsoleWarnings([]string{
		"rv.p1: " + neutralGapMessage,
		"unresolved evidence or review-level blocking cannot be settled as PASS",
		"cr.p1: " + indexGapMessage,
		"reasoner: " + neutralGapMessage,
	}, nil, warningNodes)
	want := []string{
		indexUnconfirmedLabel + "（rv.p1 · reasoner）",
		settledPassText,
		indexMismatchLabel + "（cr.p1）",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConsoleWarnings = %q, want %q", got, want)
	}
}

// The panel lists the unresolved signals as well: a warning that says the same
// thing about the same node is dropped so the cause is shown once.
func TestConsoleWarningsDropsTextsThatAnUnresolvedSignalAlreadyShows(t *testing.T) {
	unresolved := []UnresolvedSignal{
		{Node: "explore", Kind: UnresolvedEvidence, Message: indexGapMessage},
		{Node: "cr.p1", Kind: UnresolvedEvidence, Message: indexGapMessage},
		{Node: "coverage", Kind: UnresolvedEvidence, Message: "pipeline log unavailable"},
	}
	got := ConsoleWarnings([]string{
		"explore: " + indexGapMessage,
		"cr.p1: " + indexGapMessage,
		"rv.p1: " + neutralGapMessage,
		"coverage: pipeline log unavailable",
		"unresolved evidence or review-level blocking cannot be settled as PASS",
	}, unresolved, warningNodes)
	want := []string{indexUnconfirmedLabel + "（rv.p1）", settledPassText}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConsoleWarnings = %q, want %q", got, want)
	}
}

// The same text about another node is a different statement: only the node the
// unresolved group lists makes a warning a repeat, and a warning without a node
// is never dropped.
func TestConsoleWarningsDropsADuplicateOnlyForTheNodeOfItsGroup(t *testing.T) {
	unresolved := []UnresolvedSignal{{Node: "explore", Kind: UnresolvedEvidence, Message: indexGapMessage}}
	got := ConsoleWarnings([]string{
		"explore: " + indexGapMessage,
		"cr.p1: " + indexGapMessage,
	}, unresolved, warningNodes)
	if want := []string{indexMismatchLabel + "（cr.p1）"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("another node: ConsoleWarnings = %q, want %q", got, want)
	}
	got = ConsoleWarnings([]string{indexGapMessage}, unresolved, warningNodes)
	if want := []string{indexMismatchLabel}; !reflect.DeepEqual(got, want) {
		t.Fatalf("no node: ConsoleWarnings = %q, want %q", got, want)
	}
}

// Everything dropped is still a non-nil empty result: the API sends it as [] so
// the console does not fall back to the raw warnings.
func TestConsoleWarningsReturnsAnEmptyNonNilListWhenEverythingRepeats(t *testing.T) {
	got := ConsoleWarnings(
		[]string{"explore: " + indexGapMessage},
		[]UnresolvedSignal{{Node: "explore", Kind: UnresolvedEvidence, Message: indexGapMessage}},
		warningNodes,
	)
	if got == nil || len(got) != 0 {
		t.Fatalf("ConsoleWarnings = %#v, want an empty non-nil slice", got)
	}
}

func TestConsoleWarningsReadsTheNodePrefixOnlyWhenItIsANode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		warning string
		want    string
	}{
		{
			name:    "a node before an anchored template",
			warning: "validate: lane validate (validate) did not complete: failed",
			want:    "验证评审通道未完成（失败）：该通道执行失败，建议查看控制台中的失败原因后重新触发评审。（validate）",
		},
		{
			name:    "a warning without a node has no parentheses",
			warning: "unresolved evidence or review-level blocking cannot be settled as PASS",
			want:    settledPassText,
		},
		{
			name:    "words before the colon are part of the message",
			warning: "budget exhausted: some findings were not verified",
			want:    "预算耗尽：部分问题未完成二次验证，已按存疑处理。",
		},
		{
			name:    "a file name is not a node",
			warning: ".gitlab-review.yml: unknown field review.foo ignored",
			want:    "仓库配置 .gitlab-review.yml 中的未知字段 review.foo 已忽略。",
		},
		{
			name:    "a code-owned check name is a node",
			warning: "coverage: diff collection omitted 1 of 20 changed file(s): 19 included in the materials snapshot, 1 ignored by rules; reasons: too_large=1; omitted paths: assets/hero.png",
			want:    "变更文件收集不完整：共 20 个变更文件，有 1 个未进入评审材料（原因：文件过大 1 个）；未进入评审的路径：assets/hero.png。（coverage）",
		},
		{
			name:    "unmapped English keeps its text and the node",
			warning: "provenance: something odd happened",
			want:    "something odd happened（provenance）",
		},
		{
			name:    "Chinese prose stays as written",
			warning: "验证通道 BLOCKED：没有流水线",
			want:    "验证通道 BLOCKED：没有流水线",
		},
		{
			name:    "a verification failure names no node",
			warning: "verify failed for group f1: node failed",
			want:    "部分问题的二次验证未完成，已按存疑处理。",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ConsoleWarnings([]string{tc.warning}, nil, warningNodes)
			if len(got) != 1 || got[0] != tc.want {
				t.Fatalf("ConsoleWarnings(%q) = %q, want [%q]", tc.warning, got, tc.want)
			}
		})
	}
}

// A prefix is read as a node only when it names a node of this workflow, a node
// of an unresolved signal or a code-owned check. Any other word before a colon
// belongs to the warning and stays in its text.
func TestConsoleWarningsKeepsTheWholeWarningForAnUnknownPrefix(t *testing.T) {
	for _, warning := range []string{
		"error: connect failed",
		"P1: missing null check in the loader",
		"cr.p9: " + indexGapMessage,
		"error: " + indexGapMessage,
	} {
		got := ConsoleWarnings([]string{warning}, nil, warningNodes)
		if len(got) != 1 || got[0] != warning {
			t.Fatalf("ConsoleWarnings(%q) = %q, want the warning unchanged", warning, got)
		}
	}
	// The node of an unresolved signal is known even when it is not a DAG node.
	got := ConsoleWarnings([]string{"plan.x: " + indexGapMessage}, []UnresolvedSignal{{Node: "plan.x", Kind: UnresolvedEvidence, Message: "other"}}, nil)
	if want := []string{indexMismatchLabel + "（plan.x）"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ConsoleWarnings = %q, want %q", got, want)
	}
}

// Warnings and signals are worded by the shape the executor writes, not by a
// phrase, so a model sentence that mentions it keeps its own wording.
func TestConsoleWarningsDoNotWordModelTextAsAToolGap(t *testing.T) {
	for _, tc := range []struct{ warning, want string }{
		{
			"rv.p1: the build log says MCP tools unavailable: retry later",
			"the build log says MCP tools unavailable: retry later（rv.p1）",
		},
		{
			"rv.p1: evidence is blocked by workspace index identity (index abcdef01, target 33333333)",
			"evidence is blocked by workspace index identity (index abcdef01, target 33333333)（rv.p1）",
		},
	} {
		got := ConsoleWarnings([]string{tc.warning}, nil, warningNodes)
		if len(got) != 1 || got[0] != tc.want {
			t.Fatalf("ConsoleWarnings(%q) = %q, want [%q]", tc.warning, got, tc.want)
		}
	}
}

func TestConsoleWarningsSkipsBlankWarningsAndMergesNodelessRepeats(t *testing.T) {
	got := ConsoleWarnings([]string{"  ", "cr.p1: " + indexGapMessage, indexGapMessage, ""}, nil, warningNodes)
	want := []string{indexMismatchLabel + "（cr.p1）"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ConsoleWarnings = %q, want %q", got, want)
	}
}
