package review

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRenderFindingPublishesLongBodyVerbatim(t *testing.T) {
	body := "先写触发条件。\n\n" + strings.Repeat("长正文内容。", 1000)
	group := mkGroup("long-body-publish", 20, "logic", SeverityP2, "长正文", body)
	out, err := renderFindingForTest(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, body) {
		t.Fatalf("inline body was clipped:\n%s", out)
	}
	if strings.Contains(out, "内容已截断") || strings.Contains(out, "完整正文见控制台") {
		t.Fatalf("inline body still carries a clipping notice:\n%s", out)
	}
}

func TestRenderFindingPublishesLongEvidenceAndSuggestionVerbatim(t *testing.T) {
	diff := "@@ -1,450 +1,451 @@\n" + strings.Repeat("+generated line for the long evidence block\n", 300)
	suggestion := "```suggestion\n" + strings.Repeat("+patched line\n", 1000) + "```"
	group := mkGroup("long-evidence-publish", 20, "logic", SeverityP2, "长证据", "正文。")
	group.Decision.Action = ActionMark
	group.Evidence = diff
	group.Suggestion = suggestion
	out, err := renderFindingForTest(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{diff, suggestion} {
		if !strings.Contains(out, want) {
			t.Fatalf("inline evidence or suggestion was clipped (missing %d runes):\n%s", utf8.RuneCountInString(want), out)
		}
	}
	if strings.Contains(out, "内容已截断") {
		t.Fatalf("inline evidence or suggestion still carries a clipping notice:\n%s", out)
	}
}

func TestRenderSummaryPublishesLongBodyAndSuggestionVerbatim(t *testing.T) {
	body := strings.Repeat("无法定位的长正文。", 600)
	suggestion := strings.Repeat("建议补丁内容。", 1500)
	group := renderGroup("long-summary-publish", ActionPublish, SeverityP2, false, "长汇总条目", body)
	group.Suggestion = suggestion
	group.PositionOK = false
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "发现问题", Groups: []Group{group}, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{body, suggestion} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary body or suggestion was clipped (missing %d runes):\n%s", utf8.RuneCountInString(want), out)
		}
	}
	if strings.Contains(out, "内容已截断") {
		t.Fatalf("summary item still carries a clipping notice:\n%s", out)
	}
}

func TestRenderPublishesLongDoubtReasonVerbatim(t *testing.T) {
	reason := strings.Repeat("无法确认存疑原因。", 800)
	group := mkGroup("long-reason", 20, "logic", SeverityP2, "长存疑原因", "正文。")
	group.Decision.Action = ActionMark
	group.Decision.Reason = reason
	group.Challenge = &Challenge{Status: ChallengeUncertain, Reason: reason}
	out, err := renderFindingForTest(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, reason) {
		t.Fatalf("inline doubt reason was clipped:\n%s", out)
	}
	group.PositionOK = false
	summary, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "发现问题", Groups: []Group{group}, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, reason) {
		t.Fatalf("summary doubt reason was clipped:\n%s", summary)
	}
	if strings.Contains(out, "内容已截断") || strings.Contains(summary, "内容已截断") {
		t.Fatalf("doubt reason still carries a clipping notice:\n%s\n%s", out, summary)
	}
}

func TestRenderSummaryPlatformLimitOmitsOverBudgetItems(t *testing.T) {
	first := renderGroup("platform-first", ActionPublish, SeverityP1, false, "首条", "首条完整正文。")
	first.PositionOK = false
	first.Suggestion = ""
	second := renderGroup("platform-second", ActionMark, SeverityP2, false, "次条", "次条正文。")
	second.PositionOK = false
	second.Evidence = strings.Repeat("e", MaxPublishedNoteBytes)
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "发现问题", Groups: []Group{first, second}, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out); n > MaxPublishedNoteBytes {
		t.Fatalf("summary note bytes = %d, want at most %d", n, MaxPublishedNoteBytes)
	}
	if !strings.HasSuffix(out, platformLimitNotice) {
		t.Fatalf("over-limit summary lacks the platform notice:\n%s", out)
	}
	if !strings.Contains(out, "首条完整正文。") {
		t.Fatalf("platform budget dropped the complete first item:\n%s", out)
	}
	if strings.Contains(out, "次条正文。") {
		t.Fatal("an over-budget item was cut and kept, want it omitted whole")
	}
	kept := strings.TrimSuffix(out, platformLimitNotice)
	if strings.Count(kept, "```")%2 != 0 {
		t.Fatalf("platform budget cut inside a code block:\n%s", kept)
	}
	if strings.Contains(kept, strings.Repeat("e", 1000)) {
		t.Fatal("platform budget kept a truncated evidence block instead of dropping it whole")
	}
}

func TestRenderSummaryPublishesChineseBodyUnderPlatformLimit(t *testing.T) {
	body := strings.Repeat("汉字正文。", 58_000) // 290,000 runes = 870,000 bytes
	group := renderGroup("chinese-under-limit", ActionPublish, SeverityP2, false, "中文长正文", body)
	group.PositionOK = false
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "发现问题", Groups: []Group{group}, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, body) {
		t.Fatalf("Chinese body under the byte limit was clipped:\nhead=%d bytes", len(out))
	}
	if strings.Contains(out, platformLimitNotice) {
		t.Fatal("Chinese body under the byte limit triggered the platform fallback")
	}
}

func TestRenderSummaryPlatformLimitCountsBytesAndKeepsUTF8(t *testing.T) {
	firstBody := strings.Repeat("汉字段落。\n\n", 100)
	first := renderGroup("chinese-kept", ActionPublish, SeverityP2, false, "保留条目", firstBody)
	first.PositionOK = false
	first.Suggestion = ""
	paragraph := "汉字段落。\n\n"
	secondBody := strings.Repeat(paragraph, 60_000) // 1,020,000 bytes, 420,000 runes
	second := renderGroup("chinese-over-limit", ActionPublish, SeverityP2, false, "中文超限正文", secondBody)
	second.PositionOK = false
	second.Suggestion = ""
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "发现问题", Groups: []Group{first, second}, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out); n > MaxPublishedNoteBytes {
		t.Fatalf("summary note bytes = %d, want at most %d", n, MaxPublishedNoteBytes)
	}
	if !utf8.ValidString(out) {
		t.Fatal("platform budget cut a multi-byte character")
	}
	if !strings.HasSuffix(out, platformLimitNotice) {
		t.Fatalf("over-limit Chinese summary lacks the platform notice")
	}
	kept := strings.TrimRight(strings.TrimSuffix(out, platformLimitNotice), "\n")
	if !strings.HasSuffix(kept, "汉字段落。") {
		t.Fatal("platform budget did not keep the complete first entry")
	}
	if strings.Contains(kept, "中文超限正文") {
		t.Fatal("the over-budget Chinese entry was cut instead of omitted whole")
	}
}

func TestRenderFindingPlatformLimitOmitsOverBudgetBlocks(t *testing.T) {
	group := mkGroup("platform-inline", 20, "logic", SeverityP2, "平台上限", "正文。")
	group.Decision.Action = ActionMark
	group.Evidence = strings.Repeat("e", MaxPublishedNoteBytes)
	out, err := renderFindingForTest(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if n := len(out); n > MaxPublishedNoteBytes {
		t.Fatalf("inline note bytes = %d, want at most %d", n, MaxPublishedNoteBytes)
	}
	if !strings.HasSuffix(out, platformLimitNotice) {
		t.Fatalf("over-limit inline note lacks the platform notice:\n%s", out)
	}
	if !strings.Contains(out, "平台上限") || !strings.Contains(out, "正文。") {
		t.Fatalf("platform fallback dropped the finding header or body:\n%s", out)
	}
	kept := strings.TrimSuffix(out, platformLimitNotice)
	if strings.Count(kept, "```")%2 != 0 {
		t.Fatalf("platform fallback cut inside the evidence code block:\n%s", kept)
	}
}
