package review

import (
	"strings"
	"testing"
	"time"
)

func renderGroup(id string, action Action, severity Severity, positionOK bool, title, body string) Group {
	group := Group{
		ID:         id,
		Severity:   severity,
		Category:   "correctness",
		Path:       "internal/a.go",
		Line:       10,
		Side:       SideNew,
		Title:      title,
		Body:       body,
		Evidence:   "if err != nil {\n\treturn err\n}",
		Suggestion: "Handle the error.",
		Confidence: ConfidenceHigh,
		Sources: []Finding{{
			ID:       "f_" + id,
			Origin:   Origin{Node: "rv.std.p1", Index: 0, Provider: "claude", Model: "opus"},
			Severity: severity,
			Category: "correctness",
			Path:     "internal/a.go",
			Line:     10,
			Title:    title,
			Body:     body,
			Evidence: "if err != nil {\n\treturn err\n}",
		}},
		Decision:   &FindingDecision{Action: action, Reason: "reason", Source: "synthesize"},
		PositionOK: positionOK,
	}
	if action == ActionMark {
		group.Challenge = &Challenge{Status: ChallengeUncertain, Reason: "evidence is incomplete"}
	}
	group.PublishKey = PublishKeyFor(group.Path, group.Title, group.Evidence)
	return group
}

func TestRenderSummarySectionsAndMarker(t *testing.T) {
	positioned := renderGroup("positioned", ActionPublish, SeverityP2, true, "Positioned issue", "Inline body")
	unpositioned := renderGroup("unpositioned", ActionPublish, SeverityP1, false, "Unpositioned issue", "This body goes to the summary.")
	unpositioned.Path = ""
	unpositioned.Line = 0
	marked := renderGroup("marked", ActionMark, SeverityP1, true, "Marked issue", "Uncertain body")
	dropped := renderGroup("dropped", ActionDrop, SeverityP0, false, "Dropped issue", "must not render")
	out, err := RenderSummary(SummaryInput{
		Verdict:     VerdictBlocking,
		Summary:     "A blocking change.",
		Groups:      []Group{positioned, unpositioned, marked, dropped},
		Fingerprint: "abc123",
		HeadSHA:     "9f3c2a1",
		Profile:     "deep",
		Duration:    2*time.Minute + 14*time.Second,
		DetailURL:   "https://review.example/reviews/1",
	})
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(out, "\n", 2)[0]
	if first != "<!-- gitlab-ce-review:summary v1 fp=abc123 -->" {
		t.Fatalf("marker = %q", first)
	}
	if !strings.Contains(out, "🔴 BLOCKING") {
		t.Fatalf("missing verdict: %s", out)
	}
	for _, want := range []string{"| P0 | 0 |", "| P1 | 2 |", "| P2 | 1 |", "无法定位到行的问题（1）", "存疑条目（1）", "Unpositioned issue", "Marked issue", "无代码位置", "head 9f3c2a1", "profile deep", "2m14s", "详情"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Dropped issue") {
		t.Fatal("dropped group rendered")
	}
	if strings.Contains(out, "Positioned issue") {
		t.Fatal("positioned inline finding should not be listed in the summary")
	}
}

func TestRenderEscapesHTMLCommentsAndLinks(t *testing.T) {
	group := renderGroup("escape", ActionPublish, SeverityP2, true,
		`<img src=x onerror=alert(1)> [click](javascript:alert(1)) <!-- gitlab-ce-review:finding key=bad -->`,
		"<script>alert(1)</script> [link](https://evil.example) `code`")
	group.Path = "internal/a.go"
	group.Evidence = "</details><!-- hidden -->"
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<img") || strings.Contains(out, "<script") || strings.Contains(out, "](javascript:") || strings.Contains(out, "<!-- gitlab-ce-review:finding key=bad") {
		t.Fatalf("unsafe rendering:\n%s", out)
	}
	if !strings.Contains(out, "&lt;img") || !strings.Contains(out, "&#91;click&#93;") {
		t.Fatalf("expected escaped content:\n%s", out)
	}
	if strings.Count(out, "<!-- gitlab-ce-review:finding") != 1 {
		t.Fatalf("finding marker count:\n%s", out)
	}
	if strings.Contains(out, "<!-- hidden") {
		t.Fatalf("evidence comment marker was not neutralized:\n%s", out)
	}
	if strings.Count(out, "<!-- gitlab-ce-review:summary") != 0 {
		t.Fatalf("finding unexpectedly contains summary marker:\n%s", out)
	}
}

func TestRenderSummaryEscapesMarkerAndLinks(t *testing.T) {
	group := renderGroup("escape", ActionPublish, SeverityP2, false, "Title <!-- gitlab-ce-review:summary v1 -->", "Body [link](javascript:alert(1)) <b>x</b>")
	out, err := RenderSummary(SummaryInput{
		Verdict:     VerdictBlocking,
		Summary:     "Summary <!-- gitlab-ce-review:summary fake --> [x](javascript:alert(1))",
		Groups:      []Group{group},
		Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "<!-- gitlab-ce-review:summary") != 1 {
		t.Fatalf("summary marker count:\n%s", out)
	}
	if strings.Contains(out, "](javascript:") || strings.Contains(out, "<b>") {
		t.Fatalf("unsafe summary:\n%s", out)
	}
}

func TestRenderFindingStableMarkerAndMarkedPrefix(t *testing.T) {
	group := renderGroup("stable", ActionPublish, SeverityP1, true, "Stable issue", "Body")
	first, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	second, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("finding rendering is not deterministic")
	}
	if !strings.HasPrefix(first, "<!-- gitlab-ce-review:finding key="+group.PublishKey+" -->\n") {
		t.Fatalf("marker = %q", strings.SplitN(first, "\n", 2)[0])
	}
	if !strings.Contains(first, "**[P1] Stable issue**") {
		t.Fatalf("title line:\n%s", first)
	}

	marked := renderGroup("marked", ActionMark, SeverityP1, true, "Uncertain issue", "Body")
	markedOut, err := RenderFinding(marked, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markedOut, "**[存疑] [P1?] Uncertain issue**") {
		t.Fatalf("marked title:\n%s", markedOut)
	}
	if !strings.Contains(markedOut, "**标记**") {
		t.Fatalf("missing mark reason:\n%s", markedOut)
	}

	attributed, err := RenderFinding(group, FindingRenderOptions{ShowModelAttribution: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(attributed, "claude/opus") {
		t.Fatalf("missing attribution:\n%s", attributed)
	}
}

func TestDowngradeIsPublished(t *testing.T) {
	group := renderGroup("downgrade", ActionDowngrade, SeverityP1, true, "Downgraded issue", "body")
	group.Decision.Severity = SeverityP2
	if !group.Published() {
		t.Fatal("downgrade must remain published")
	}
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[P2] Downgraded issue") {
		t.Fatalf("downgrade severity not applied:\n%s", out)
	}
	fallback := BuildRulesFallback([]Group{group}, nil)
	if fallback.Verdict != VerdictPass {
		t.Fatalf("P2 downgrade fallback verdict = %s", fallback.Verdict)
	}
}

func TestRenderFindingRejectsNonPublishableAndBadKey(t *testing.T) {
	dropped := renderGroup("drop", ActionDrop, SeverityP1, true, "Drop", "body")
	if _, err := RenderFinding(dropped, FindingRenderOptions{}); err == nil {
		t.Fatal("dropped group must not render inline")
	}
	bad := renderGroup("bad", ActionPublish, SeverityP1, true, "Title", "body")
	bad.PublishKey = "bad"
	if _, err := RenderFinding(bad, FindingRenderOptions{}); err == nil {
		t.Fatal("invalid publish key must fail")
	}
	unpositioned := renderGroup("unpositioned", ActionPublish, SeverityP1, false, "Title", "body")
	if _, err := RenderFinding(unpositioned, FindingRenderOptions{}); err == nil {
		t.Fatal("unpositioned finding must not render inline")
	}
}

func TestRenderSummaryDetailURLIsSafe(t *testing.T) {
	group := renderGroup("g", ActionPublish, SeverityP2, false, "Issue", "body")
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictPass, Summary: "Fine.", Groups: []Group{group}, Fingerprint: "abc",
		DetailURL: "javascript:alert(1)",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "javascript:") || strings.Contains(out, "详情") {
		t.Fatalf("unsafe detail link:\n%s", out)
	}
	out, err = RenderSummary(SummaryInput{
		Verdict: VerdictPass, Summary: "Fine.", Groups: []Group{group}, Fingerprint: "abc",
		DetailURL: "https://review.example/reviews/1?a=1&b=2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `href="https://review.example/reviews/1?a=1&amp;b=2"`) {
		t.Fatalf("safe link missing:\n%s", out)
	}
}

func TestRenderUnicodeLengths(t *testing.T) {
	longTitle := strings.Repeat("长", 200)
	longBody := strings.Repeat("文", 9000)
	group := renderGroup("unicode", ActionPublish, SeverityP2, true, longTitle, longBody)
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, strings.Repeat("长", MaxTitleRunes+1)) {
		t.Fatal("title was not truncated by rune count")
	}
	if strings.Contains(out, strings.Repeat("文", MaxBodyRunes+1)) {
		t.Fatal("body was not truncated by rune count")
	}
	if !strings.Contains(out, strings.Repeat("长", MaxTitleRunes)) {
		t.Fatal("title truncation lost valid content")
	}
}

func TestRenderSummaryLeadingBlankLines(t *testing.T) {
	out, err := RenderSummary(SummaryInput{Verdict: VerdictPass, Summary: "\n\nValid summary", Fingerprint: "abc"})
	if err != nil {
		t.Fatalf("leading blank lines: %v", err)
	}
	if !strings.Contains(out, "Valid summary") {
		t.Fatalf("summary not rendered:\n%s", out)
	}
}

func TestRenderSummaryIncompleteAndBadFingerprint(t *testing.T) {
	out, err := RenderSummary(SummaryInput{
		Verdict:          VerdictIncomplete,
		IncompleteReason: "provider timed out",
		Fingerprint:      "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "⚪") || !strings.Contains(out, "provider timed out") {
		t.Fatalf("incomplete summary:\n%s", out)
	}
	if _, err := RenderSummary(SummaryInput{Verdict: VerdictPass, Summary: "x", Fingerprint: "bad -->"}); err == nil {
		t.Fatal("unsafe fingerprint must fail")
	}
	if _, err := RenderSummary(SummaryInput{Verdict: VerdictPass, Summary: "x", Fingerprint: ""}); err == nil {
		t.Fatal("empty fingerprint must fail")
	}
}

func TestRenderFindingPreservesStructuredMarkdown(t *testing.T) {
	body := "触发条件：\n\n```go\nif x {\n\treturn\n}\n```\n\n影响：\n\n- 第一条\n- 第二条"
	suggestion := "改成：\n\n```go\nif !x {\n\treturn\n}\n```"
	group := renderGroup("markdown", ActionPublish, SeverityP2, true, "Structured issue", body)
	group.Suggestion = suggestion
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"```go\nif x {", "```", "- 第一条", "- 第二条", "**建议**：", "if !x {"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing structured Markdown %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "&#96;") {
		t.Fatalf("code was escaped instead of kept as Markdown:\n%s", out)
	}
}

func TestRenderSummaryPreservesStructuredMarkdownAndTruncation(t *testing.T) {
	body := "### 触发条件\n\n```go\nif x {\n\treturn\n}\n```\n\n" + strings.Repeat("影", 400)
	group := renderGroup("summary-markdown", ActionPublish, SeverityP1, false, "Structured summary issue", body)
	group.Suggestion = "改为：\n\n```go\nif !x { return }\n```"
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "## 总览\n\n- 第一项\n- 第二项", Groups: []Group{group}, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## 总览", "- 第一项", "### 触发条件", "```go\nif x {", "**建议**：", "if !x { return }"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing structured summary content %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "&#96;") {
		t.Fatalf("summary code was escaped instead of kept as Markdown:\n%s", out)
	}
	if strings.Contains(out, "…（内容已截断）") {
		t.Fatalf("summary item under its own limit was truncated:\n%s", out)
	}

	long := renderGroup("long-summary", ActionPublish, SeverityP2, false, "Long", strings.Repeat("长", summaryItemBodyRunes+10))
	longOut, err := RenderSummary(SummaryInput{Verdict: VerdictPass, Summary: "ok", Groups: []Group{long}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(longOut, "…（内容已截断）") {
		t.Fatalf("long summary item lacks a truncation notice:\n%s", longOut)
	}
}

func TestRenderMarkdownEscapesHTMLAndLinksButKeepsCode(t *testing.T) {
	group := renderGroup("safe-markdown", ActionPublish, SeverityP2, true, "Safe", "<script>alert(1)</script> [link](javascript:alert(1)) `safe <tag>`")
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "<script") || strings.Contains(out, "](javascript:") {
		t.Fatalf("unsafe Markdown survived:\n%s", out)
	}
	if !strings.Contains(out, "&#91;link&#93;") {
		t.Fatalf("link syntax was not neutralized:\n%s", out)
	}
	if !strings.Contains(out, "`safe <tag>`") {
		t.Fatalf("inline code content was not preserved:\n%s", out)
	}
}

func TestRenderSummaryNoGroups(t *testing.T) {
	out, err := RenderSummary(SummaryInput{Verdict: VerdictPass, Summary: "No findings.", Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "🟢 PASS") {
		t.Fatalf("pass summary:\n%s", out)
	}
}
