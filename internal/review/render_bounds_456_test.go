package review

import (
	"strings"
	"testing"
)

func TestReasonerLongTitleClipsAtSentenceBoundary(t *testing.T) {
	finding := strings.Repeat("甲", 60) + "。" + strings.Repeat("乙", 100)
	proposal := ProposalFromReasoner("reasoner", "provider", "model", 1, ReasonerOut{
		ResultType: "FINDINGS",
		Findings: []ReasonerFinding{{
			Severity: "notable", Dimension: "logic", Finding: finding,
			Where: "MR description", Why: "why", Confidence: ConfidenceHigh,
		}},
	})
	title := proposal.Findings[0].Title
	if runes := []rune(title); len(runes) > MaxTitleRunes {
		t.Fatalf("title runes = %d, want at most %d", len(runes), MaxTitleRunes)
	}
	if !strings.HasSuffix(title, "…") {
		t.Fatalf("clipped title lacks an ellipsis: %q", title)
	}
	if !strings.HasPrefix(title, strings.Repeat("甲", 60)+"。") {
		t.Fatalf("clipped title lost the complete first sentence: %q", title)
	}
	if strings.Contains(title, "乙") {
		t.Fatalf("clipped title kept content past the sentence boundary: %q", title)
	}
	if err := proposal.Findings[0].Validate(); err != nil {
		t.Fatalf("clipped title must stay a valid finding title: %v", err)
	}
}

func TestReasonerLongTitleClipsAtWordBoundary(t *testing.T) {
	finding := strings.TrimSpace(strings.Repeat("alpha beta ", 40))
	proposal := ProposalFromReasoner("reasoner", "provider", "model", 1, ReasonerOut{
		ResultType: "FINDINGS",
		Findings: []ReasonerFinding{{
			Severity: "notable", Dimension: "logic", Finding: finding,
			Where: "MR description", Why: "why", Confidence: ConfidenceHigh,
		}},
	})
	title := proposal.Findings[0].Title
	if runes := []rune(title); len(runes) > MaxTitleRunes {
		t.Fatalf("title runes = %d, want at most %d", len(runes), MaxTitleRunes)
	}
	if !strings.HasSuffix(title, "alpha…") && !strings.HasSuffix(title, "beta…") {
		t.Fatalf("title was cut inside a word: %q", title)
	}
}

func TestRelatedExcerptEndsAtSentenceBoundary(t *testing.T) {
	body := strings.Repeat("第一段说明文字。", 20) + strings.Repeat("尾巴", 40)
	rep := mkGroup("rep", 20, "logic", SeverityP2, "read_token 把支付令牌打印到标准输出", body)
	duplicate := mkGroup("dup", 20, "logic", SeverityP2, "read_token 把支付令牌打印到标准输出", body)
	out := MergePresentation([]Group{rep, duplicate}, nil)
	var display Group
	for _, group := range out {
		if group.Published() {
			display = group
		}
	}
	if len(display.Related) != 1 {
		t.Fatalf("expected one folded related item, got %+v", display.Related)
	}
	excerpt := display.Related[0].Excerpt
	if runes := []rune(excerpt); len(runes) > relatedExcerptRunes {
		t.Fatalf("excerpt runes = %d, want at most %d", len(runes), relatedExcerptRunes)
	}
	if !strings.HasSuffix(excerpt, "。…") {
		t.Fatalf("excerpt does not end at a sentence boundary with an ellipsis: %q", excerpt)
	}
	if strings.Contains(excerpt, "尾巴") {
		t.Fatalf("excerpt kept content past the boundary: %q", excerpt)
	}
	if !display.Related[0].ExcerptTruncated {
		t.Fatalf("clipped excerpt must record that it was truncated: %+v", display.Related[0])
	}
	rendered, err := RenderFinding(display, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rendered, "编号 "+display.Related[0].ID) {
		t.Fatalf("clipped related excerpt must keep the finding ID:\n%s", rendered)
	}
}

func TestInlineBodyTruncationPointsAtConsole(t *testing.T) {
	body := strings.Repeat("长正文内容。", 120)
	group := mkGroup("long-body", 20, "logic", SeverityP2, "很长的正文", body)
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "内容已截断") {
		t.Fatalf("long inline body lacks its truncation notice:\n%s", out)
	}
	if !strings.Contains(out, "完整正文见控制台") {
		t.Fatalf("truncated inline discussion must point at the console:\n%s", out)
	}
	if briefBodyRunes != 400 {
		t.Fatalf("briefBodyRunes = %d, the #451 product limit must stay 400", briefBodyRunes)
	}
}

func TestShortInlineBodyHasNoConsoleNotice(t *testing.T) {
	group := mkGroup("short-body", 20, "logic", SeverityP2, "短正文", "一句话说明。")
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "完整正文见控制台") {
		t.Fatalf("untruncated body must not point at the console:\n%s", out)
	}
}

func TestLongWarningClipsAtSentenceBoundary(t *testing.T) {
	warning := strings.Repeat("这是一条较长的评审提示。", 60)
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "found problems", Groups: []Group{},
		Warnings:    []string{warning},
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, warning) {
		t.Fatalf("long warning was published unbounded:\n%s", body)
	}
	if !strings.Contains(body, "。…\n") {
		t.Fatalf("long warning does not end at a sentence boundary with an ellipsis:\n%s", body)
	}
}

func TestLongSummaryClipsAtLineBoundary(t *testing.T) {
	summary := strings.Repeat("总览句子。", 700)
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: summary,
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "。\n\n…（内容已截断）") {
		t.Fatalf("long summary does not end at a sentence boundary:\n%s", body)
	}
}

func TestUnresolvedMessageClipKeepsSentenceBoundary(t *testing.T) {
	message := strings.Repeat("无法读取流水线日志。", 100)
	summary := incompleteSummary([]UnresolvedSignal{{Node: "rv.std.p1", Kind: UnresolvedEvidence, Message: message}})
	if strings.Contains(summary, message) {
		t.Fatalf("unresolved message was not bounded: %q", summary)
	}
	if !strings.Contains(summary, "。…") {
		t.Fatalf("unresolved message does not end at a sentence boundary: %q", summary)
	}
}
