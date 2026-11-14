package review

import (
	"strings"
	"testing"
)

const ruleSummary3 = "规则模式汇总：共 4 条问题，已发布 3 条。"

func countsFixture() []Group {
	a := laneGroup("a", "cr.p1", 10, "logic", SeverityP1, "余额校验方向写反", "条件写成小于号。")
	b := laneGroup("b", "rv.p1", 10, "correctness", SeverityP2, "余额校验方向写反导致拒绝", "另一段说明。")
	c := laneGroup("c", "cr.p1", 40, "security", SeverityP1, "令牌写入日志", "日志输出包含完整令牌字符串。")
	d := laneGroup("d", "cr.p1", 50, "logic", SeverityP2, "已丢弃", "x")
	d.Decision = &FindingDecision{Action: ActionDrop, Source: "rules"}
	return []Group{a, b, c, d}
}

func TestRuleSummaryCountsExcludeFoldedGroupsAndStateThem(t *testing.T) {
	groups := MergePresentation(countsFixture(), nil)
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: ruleSummary3, RuleSummary: true, Groups: groups,
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "共 4 条问题，裁决发布 3 条（行内 2 条，仅汇总 0 条，另有 1 条并入相关描述）") {
		t.Fatalf("summary counts not aligned with the listed entries:\n%s", body)
	}
	if strings.Contains(body, "已发布 3 条") {
		t.Fatalf("summary still counts the folded group:\n%s", body)
	}
	if !strings.Contains(body, "**P1** × 2") {
		t.Fatalf("header counts changed:\n%s", body)
	}
}

func TestRuleSummaryCountsStateBreakdownWithoutFolding(t *testing.T) {
	groups := countsFixture()[2:]
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "规则模式汇总：共 2 条问题，已发布 1 条。", RuleSummary: true, Groups: groups,
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "共 2 条问题，裁决发布 1 条（行内 1 条，仅汇总 0 条）") || strings.Contains(body, "并入相关描述") {
		t.Fatalf("unfolded summary must state the verdict and inline counts:\n%s", body)
	}
}

func TestRuleSummaryCountsIncludeSummaryFoldedGroups(t *testing.T) {
	listed := laneGroup("a", "cr.p1", 10, "logic", SeverityP1, "read_token 把支付令牌打印到标准输出", "read_token 调用 print 输出支付令牌到标准输出，日志可读取。")
	dup := laneGroup("b", "reasoner", 0, "security", SeverityP1, "read_token 把支付令牌打印到标准输出", "read_token 调用 print 输出支付令牌到标准输出，日志可读取。")
	dup.Path, dup.PositionOK = "", false
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "规则模式汇总：共 2 条问题，已发布 2 条。", RuleSummary: true, Groups: []Group{listed, dup},
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "裁决发布 2 条（行内 1 条，仅汇总 0 条，另有 1 条并入相关描述）") {
		t.Fatalf("summary-folded group not stated:\n%s", body)
	}
}

func TestPublishKeyConflictWarningSaysWhereTheItemIs(t *testing.T) {
	listed := laneGroup("a", "cr.p1", 10, "logic", SeverityP1, "read_token 把支付令牌打印到标准输出", "read_token 调用 print 输出支付令牌到标准输出，日志可读取。")
	dup := laneGroup("b", "reasoner", 0, "security", SeverityP1, "read_token 把支付令牌打印到标准输出", "read_token 调用 print 输出支付令牌到标准输出，日志可读取。")
	dup.Path, dup.PositionOK = "", false
	other := laneGroup("c", "cr.p1", 40, "security", SeverityP1, "令牌写入日志", "日志输出包含完整令牌字符串。")
	other.PositionOK = false

	got := PublishKeyConflictWarning([]Group{listed, dup, other}, map[string]bool{"b": true})
	if !strings.Contains(got, "相关描述") || !strings.Contains(got, "read_token") || strings.Contains(got, "已保留在摘要中") {
		t.Fatalf("folded item: %q", got)
	}
	got = PublishKeyConflictWarning([]Group{listed, dup, other}, map[string]bool{"c": true})
	if !strings.Contains(got, "摘要中单独列出") || strings.Contains(got, "相关描述") {
		t.Fatalf("listed item: %q", got)
	}
	got = PublishKeyConflictWarning([]Group{listed, dup, other}, map[string]bool{"b": true, "c": true})
	if !strings.Contains(got, "相关描述") || !strings.Contains(got, "单独列出") {
		t.Fatalf("mixed: %q", got)
	}
}

func TestModelSummaryTextIsNeverRewritten(t *testing.T) {
	groups := MergePresentation(countsFixture(), nil)
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: ruleSummary3, Groups: groups,
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "已发布 3 条。") || strings.Contains(body, "并入相关描述）") {
		t.Fatalf("non-rule summary text was rewritten:\n%s", body)
	}
}
