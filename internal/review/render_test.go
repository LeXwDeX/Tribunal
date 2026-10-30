package review

import (
	"fmt"
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
	headingAt := strings.Index(out, "### AI 评审：🔴 BLOCKING")
	if headingAt < 0 {
		t.Fatalf("missing verdict heading: %s", out)
	}
	countsAt := strings.Index(out, "**P1** × 2 · **P2** × 1")
	summaryAt := strings.Index(out, "A blocking change.")
	if headingAt < 0 || countsAt < 0 || summaryAt < 0 || !(headingAt < countsAt && countsAt < summaryAt) {
		t.Fatalf("heading, counts and model summary order = %d, %d, %d:\n%s", headingAt, countsAt, summaryAt, out)
	}
	if strings.Count(out, "**P1** × 2 · **P2** × 1") != 1 {
		t.Fatalf("severity count line rendered more than once:\n%s", out)
	}
	for _, want := range []string{"**P1** × 2 · **P2** × 1", "无法定位到行的问题（1）", "存疑条目（1）", "#### [P1] Unpositioned issue\n", "#### [P1?] Marked issue\n\n`internal/a.go`:10", "head 9f3c2a<span></span>1", "profile deep", "2m14s", "详情"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Dropped issue") {
		t.Fatal("dropped group rendered")
	}
	if strings.Count(out, "Positioned issue") != 1 {
		t.Fatalf("positioned inline finding must appear exactly once in the compact overview:\n%s", out)
	}
	overview := inlineOverview(out)
	if !strings.Contains(overview, "### 行内问题（1）") || countOverviewBullets(overview) != 1 ||
		!strings.Contains(overview, "- **[P2]** Positioned issue · `internal/a.go`:10") {
		t.Fatalf("overview = %q", overview)
	}
	if strings.Contains(out, "#### [P2] Positioned issue") || strings.Contains(out, "Inline body") {
		t.Fatalf("positioned inline finding was duplicated with its full body:\n%s", out)
	}
	for _, unwanted := range []string{"| 级别 |", "**结论**", "P0**", "位置："} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("summary still contains verbose element %q:\n%s", unwanted, out)
		}
	}
}

func TestRenderSummaryOmitsCountLineWithoutFindings(t *testing.T) {
	out, err := RenderSummary(SummaryInput{Verdict: VerdictPass, Summary: "No findings.", Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "×") {
		t.Fatalf("empty review rendered a count line:\n%s", out)
	}
	if !strings.Contains(out, "### AI 评审：🟢 PASS\n\nNo findings.") {
		t.Fatalf("pass summary heading is not followed by the model summary:\n%s", out)
	}
	if overview := inlineOverview(out); overview != "" {
		t.Fatalf("empty review rendered an inline overview:\n%s", overview)
	}
}

func TestRenderFindingIsConcise(t *testing.T) {
	group := renderGroup("concise", ActionPublish, SeverityP1, true, "Concise issue", "Fails when `x` is nil.")
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := "<!-- gitlab-ce-review:finding key=" + group.PublishKey + " -->\n**[P1] Concise issue**\n\nFails when `x` is nil.\n\n**建议**：Handle the error.\n"
	if out != want {
		t.Fatalf("finding =\n%s\nwant\n%s", out, want)
	}

	group.Suggestion = "```suggestion\n\treturn nil\n```"
	out, err = RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "\n\n```suggestion\n\treturn nil\n```\n") || strings.Contains(out, "建议") {
		t.Fatalf("suggestion block must render without a label:\n%s", out)
	}

	marked := renderGroup("concise-mark", ActionMark, SeverityP1, true, "Uncertain issue", "Body")
	markedOut, err := RenderFinding(marked, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markedOut, "<details><summary>证据</summary>") {
		t.Fatalf("marked finding must keep its evidence:\n%s", markedOut)
	}
}

func TestRenderEscapesHTMLCommentsAndLinks(t *testing.T) {
	group := renderGroup("escape", ActionMark, SeverityP2, true,
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
	if !strings.Contains(out, "```\n</details><!-- hidden -->\n```") {
		t.Fatalf("evidence was not preserved inside its code fence:\n%s", out)
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
	if !strings.Contains(out, "#### [P2] Title &lt;\\!-- gitlab-ce-review\\:summary v1 --<span></span>&gt;") {
		t.Fatalf("summary item heading is missing or unsafe:\n%s", out)
	}
	if !strings.Contains(out, "&lt;!-- gitlab-ce-review") {
		t.Fatalf("summary marker text was not HTML-escaped:\n%s", out)
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

	marked := renderGroup("marked", ActionMark, SeverityP2, true, "Uncertain issue", "Body text.")
	marked.Decision.Reason = "needs <runtime> [check]"
	marked.Suggestion = "Try again."
	markedOut, err := RenderFinding(marked, FindingRenderOptions{ShowModelAttribution: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(markedOut, "**[P2 · 存疑] Uncertain issue**") || strings.Contains(markedOut, "[存疑] [P2?]") || strings.Contains(markedOut, "**标记**") {
		t.Fatalf("marked title:\n%s", markedOut)
	}
	bodyAt := strings.Index(markedOut, "Body text.")
	reasonAt := strings.Index(markedOut, "**存疑原因**：needs &lt;runtime<span></span>&gt; &#91;check&#93;")
	suggestionAt := strings.Index(markedOut, "**建议**：Try again.")
	evidenceAt := strings.Index(markedOut, "<details><summary>证据</summary>")
	attributionAt := strings.Index(markedOut, "claude/opus")
	if bodyAt < 0 || reasonAt < 0 || suggestionAt < 0 || evidenceAt < 0 || attributionAt < 0 ||
		!(bodyAt < reasonAt && reasonAt < suggestionAt && suggestionAt < evidenceAt && evidenceAt < attributionAt) {
		t.Fatalf("body, reason, suggestion, evidence or attribution order is wrong:\n%s", markedOut)
	}
	marked.Decision.Reason = ""
	emptyOut, err := RenderFinding(marked, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(emptyOut, "存疑原因") || strings.Contains(emptyOut, "**标记**") {
		t.Fatalf("empty decision reason rendered a reason block:\n%s", emptyOut)
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
		Warnings:         []string{"provider <slow>", "second warning"},
		Fingerprint:      "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	warnAt := strings.Index(out, "### 评审提示（2）")
	reasonAt := strings.Index(out, "> 评审未完成：provider timed out")
	if warnAt < 0 || reasonAt < 0 || warnAt > reasonAt {
		t.Fatalf("warnings and incomplete reason are missing or out of order:\n%s", out)
	}
	for _, want := range []string{"⚪ 评审未完成", "- provider &lt;slow<span></span>&gt;", "- second warning", "provider timed out"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "×") {
		t.Fatalf("incomplete summary without groups rendered a count line:\n%s", out)
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
	for _, want := range []string{"```go\nif x {", "```", "- 第一条", "- 第二条", "**建议**：改成：", "if !x {"} {
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
	group.Suggestion = "```suggestion\n\treturn nil\n```"
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "## 总览\n\n- 第一项\n- 第二项", Groups: []Group{group}, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"## 总览", "- 第一项", "### 触发条件", "```go\nif x {", "```suggestion\n\treturn nil\n```"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing structured summary content %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "&#96;") || strings.Contains(out, "**建议**：```suggestion") {
		t.Fatalf("summary code was escaped or a fenced suggestion was labeled:\n%s", out)
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

func TestRenderSummaryItemHierarchyAndSeparation(t *testing.T) {
	first := renderGroup("u1", ActionPublish, SeverityP1, false, "First unpositioned", "First body.")
	first.Path = ""
	first.Line = 0
	first.Suggestion = "First suggestion."
	second := renderGroup("u2", ActionPublish, SeverityP2, false, "Second unpositioned", "Second body.")
	second.Suggestion = "Second suggestion."
	firstMarked := renderGroup("m1", ActionMark, SeverityP1, false, "First marked", "First marked body.")
	firstMarked.Challenge.Reason = "missing context"
	firstMarked.Decision.Reason = "on_uncertain=mark"
	firstMarked.Suggestion = "First marked suggestion."
	secondMarked := renderGroup("m2", ActionMark, SeverityP2, false, "Second marked", "Second marked body.")
	secondMarked.Path = ""
	secondMarked.Line = 0
	secondMarked.Suggestion = "Second marked suggestion."

	out, err := RenderSummary(SummaryInput{
		Verdict:     VerdictBlocking,
		Summary:     "Summary.",
		Groups:      []Group{first, second, firstMarked, secondMarked},
		Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"### 无法定位到行的问题（2）",
		"### 存疑条目（2）",
		"#### [P1] First unpositioned\n\nFirst body.",
		"#### [P2] Second unpositioned\n\n`internal/a.go`:10\n\nSecond body.",
		"#### [P1?] First marked\n\n`internal/a.go`:10\n\n**标记原因**：挑战结果：missing context；on_uncertain=mark",
		"#### [P2?] Second marked\n\n**标记原因**：挑战结果：evidence is incomplete；reason",
		"**建议**：First suggestion.",
		"Second marked suggestion.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in:\n%s", want, out)
		}
	}
	for _, want := range []string{
		"First suggestion.\n\n#### [P2] Second unpositioned",
		"Second suggestion.\n\n### 存疑条目（2）",
		"First marked suggestion.\n\n<details><summary>证据</summary>",
		"</details>\n\n#### [P2?] Second marked",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("items are not separated by a blank line: missing %q in:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"**[P1] First unpositioned**", "**[P1?] First marked**"} {
		if strings.Contains(out, unwanted) {
			t.Fatalf("summary item still uses a bare bold title %q:\n%s", unwanted, out)
		}
	}
}

func TestRenderSummaryInlineOverview(t *testing.T) {
	first := renderGroup("p1", ActionPublish, SeverityP1, true, "First positioned", "First inline body")
	first.Suggestion = "First suggestion."
	downgraded := renderGroup("p2", ActionDowngrade, SeverityP1, true, "Downgraded issue", "Downgraded body")
	downgraded.Decision.Severity = SeverityP2
	unpositioned := renderGroup("u1", ActionPublish, SeverityP2, false, "Unpositioned issue", "Full fallback body")
	marked := renderGroup("m1", ActionMark, SeverityP1, true, "Marked issue", "Marked body")
	dropped := renderGroup("d1", ActionDrop, SeverityP0, true, "Dropped issue", "Dropped body")
	noPath := renderGroup("no-path", ActionPublish, SeverityP1, true, "No path issue", "Body")
	noPath.Path, noPath.Line = "", 0
	noLine := renderGroup("no-line", ActionPublish, SeverityP2, true, "No line issue", "Body")
	noLine.Line = 0
	hostile := renderGroup("hostile", ActionPublish, SeverityP2, true,
		"<img src=x onerror=alert(1)> [click](javascript:alert(1)) `raw`", "Body")
	hostile.Path = "internal/weird`path`.go"

	cases := []struct {
		name      string
		groups    []Group
		wantCount int
		wantOut   []string
		once      []string
		notOut    []string
	}{
		{
			name:      "mixed positions, downgrade, mark and drop",
			groups:    []Group{first, downgraded, unpositioned, marked, dropped},
			wantCount: 2,
			wantOut: []string{
				"- **[P1]** First positioned · `internal/a.go`:10\n- **[P2]** Downgraded issue · `internal/a.go`:10",
				"#### [P2] Unpositioned issue", "Full fallback body", "#### [P1?] Marked issue", "**标记原因**",
			},
			once:   []string{"Marked issue", "Unpositioned issue"},
			notOut: []string{"First inline body", "Downgraded body", "First suggestion.", "Dropped issue"},
		},
		{
			name:      "missing path and line",
			groups:    []Group{noPath, noLine},
			wantCount: 2,
			wantOut:   []string{"- **[P1]** No path issue\n- **[P2]** No line issue · `internal/a.go`"},
		},
		{
			name:      "hostile title and path",
			groups:    []Group{hostile},
			wantCount: 1,
			wantOut:   []string{"&lt;img", "&#91;click&#93;", "&#96;raw&#96;", "`` internal/weird`path`.go ``"},
			notOut:    []string{"<img", "](javascript:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary.", Groups: tc.groups, Fingerprint: "abc"})
			if err != nil {
				t.Fatal(err)
			}
			overview := inlineOverview(out)
			if tc.wantCount == 0 {
				if overview != "" || strings.Contains(out, "行内问题") {
					t.Fatalf("empty overview rendered:\n%s", out)
				}
				return
			}
			if !strings.Contains(overview, fmt.Sprintf("### 行内问题（%d）", tc.wantCount)) || countOverviewBullets(overview) != tc.wantCount {
				t.Fatalf("overview heading/bullets do not match %d:\n%s", tc.wantCount, overview)
			}
			for _, want := range tc.wantOut {
				if !strings.Contains(out, want) {
					t.Fatalf("summary is missing %q:\n%s", want, out)
				}
			}
			for _, once := range tc.once {
				if strings.Count(out, once) != 1 {
					t.Fatalf("%q should appear exactly once:\n%s", once, out)
				}
			}
			for _, unwanted := range tc.notOut {
				if strings.Contains(out, unwanted) {
					t.Fatalf("summary contains %q:\n%s", unwanted, out)
				}
			}
		})
	}
}

func TestRenderSummaryInlineOverviewFollowsPositionOKChanges(t *testing.T) {
	first := renderGroup("p1", ActionPublish, SeverityP1, true, "First positioned", "First body")
	second := renderGroup("p2", ActionPublish, SeverityP2, true, "Second positioned", "Second body")
	in := SummaryInput{
		Verdict:     VerdictBlocking,
		Summary:     "Summary.",
		Groups:      []Group{first, second},
		Fingerprint: "abc",
	}

	out, err := RenderSummary(in)
	if err != nil {
		t.Fatal(err)
	}
	if overview := inlineOverview(out); !strings.Contains(overview, "### 行内问题（2）") || countOverviewBullets(overview) != 2 {
		t.Fatalf("initial overview:\n%s", out)
	}
	if strings.Contains(out, "无法定位到行的问题") {
		t.Fatalf("positioned findings rendered a fallback section:\n%s", out)
	}

	in.Groups[0].PositionOK = false
	out, err = RenderSummary(in)
	if err != nil {
		t.Fatal(err)
	}
	overview := inlineOverview(out)
	if !strings.Contains(overview, "### 行内问题（1）") || countOverviewBullets(overview) != 1 ||
		strings.Contains(overview, "First positioned") || !strings.Contains(overview, "Second positioned") {
		t.Fatalf("overview did not follow PositionOK=false:\n%s", out)
	}
	if !strings.Contains(out, "### 无法定位到行的问题（1）") || !strings.Contains(out, "#### [P1] First positioned") {
		t.Fatalf("full fallback is missing after PositionOK=false:\n%s", out)
	}

	in.Groups[1].PositionOK = false
	out, err = RenderSummary(in)
	if err != nil {
		t.Fatal(err)
	}
	if overview := inlineOverview(out); overview != "" {
		t.Fatalf("overview still renders without positioned items:\n%s", overview)
	}
	if !strings.Contains(out, "### 无法定位到行的问题（2）") {
		t.Fatalf("both items should fall back to the full section:\n%s", out)
	}
}

func TestEscapeOneLineBlocksGFMLinksAndReferences(t *testing.T) {
	cases := []struct{ in, want string }{
		{"@user", `\@user`},
		{"!1", `\!1`},
		{"#1", `\#1`},
		{"https://example.invalid", `https\://example.invalid`},
		{"ftp://example.invalid", `ftp\://example.invalid`},
		{"www.example.invalid", `www\.example.invalid`},
		{"person@example.invalid", `person\@example.invalid`},
		{`\@user`, `\\\@user`},
		{`\\@user`, `\\\\\@user`},
	}
	for _, tc := range cases {
		if got := escapeOneLine(tc.in, 500); got != tc.want {
			t.Errorf("escapeOneLine(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRenderOneLineGFMEscapesInSummaryAndFinding(t *testing.T) {
	title := "@user !1 #1 https://example.invalid ftp://example.invalid www.example.invalid person@example.invalid " + `\@user`
	group := renderGroup("gfm", ActionPublish, SeverityP1, true, title, "Body")
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "Summary.", Groups: []Group{group}, Fingerprint: "abc",
	})
	if err != nil {
		t.Fatal(err)
	}
	overview := inlineOverview(out)
	for _, want := range []string{
		`\@user`, `\!1`, `\#1`, `https\://example.invalid`, `ftp\://example.invalid`,
		`www\.example.invalid`, `person\@example.invalid`, `\\\@user`,
	} {
		if !strings.Contains(overview, want) {
			t.Fatalf("overview is missing GFM escape %q:\n%s", want, overview)
		}
	}

	marked := renderGroup("gfm-mark", ActionMark, SeverityP1, true, "@user", "Body")
	marked.Decision.Reason = "see @user at https://example.invalid"
	finding, err := RenderFinding(marked, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(finding, `**[P1 · 存疑] \@user**`) {
		t.Fatalf("finding title lacks the GFM escape:\n%s", finding)
	}
	if !strings.Contains(finding, `**存疑原因**：see \@user at https\://example.invalid`) {
		t.Fatalf("finding reason lacks the GFM escapes:\n%s", finding)
	}
}

func TestRenderMarkedEvidenceForAllPositions(t *testing.T) {
	group := renderGroup("mark-evidence", ActionMark, SeverityP1, true, "Marked", "Body")
	group.Evidence = "line\n```\n</details><!-- hidden -->"
	in := SummaryInput{Verdict: VerdictBlocking, Summary: "Summary.", Groups: []Group{group}, Fingerprint: "abc"}

	positioned, err := RenderSummary(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(positioned, "<details><summary>证据</summary>") != 1 {
		t.Fatalf("positioned marked summary must retain evidence:\n%s", positioned)
	}
	inline, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inline, "<details><summary>证据</summary>") {
		t.Fatalf("positioned marked finding lost evidence:\n%s", inline)
	}

	in.Groups[0].PositionOK = false
	fallback, err := RenderSummary(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(fallback, "<details><summary>证据</summary>") != 1 {
		t.Fatalf("fallback evidence count:\n%s", fallback)
	}
	for _, want := range []string{
		"#### [P1?] Marked",
		"Body",
		"**建议**：Handle the error.\n\n<details><summary>证据</summary>",
		"**标记原因**",
		"````\nline\n```\n</details><!-- hidden -->\n````",
	} {
		if !strings.Contains(fallback, want) {
			t.Fatalf("fallback is missing %q:\n%s", want, fallback)
		}
	}
	in.Groups[0].Evidence = ""
	empty, err := RenderSummary(in)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(empty, "<details><summary>证据</summary>") {
		t.Fatalf("empty evidence rendered a details block:\n%s", empty)
	}
}

// inlineOverview returns the compact 行内问题 section, or "" when absent.
func inlineOverview(out string) string {
	const heading = "### 行内问题"
	start := strings.Index(out, heading)
	if start < 0 {
		return ""
	}
	rest := out[start:]
	if end := strings.Index(rest[len(heading):], "\n### "); end >= 0 {
		rest = rest[:len(heading)+end]
	}
	return rest
}

func countOverviewBullets(section string) int {
	count := 0
	for _, line := range strings.Split(section, "\n") {
		if strings.HasPrefix(line, "- ") {
			count++
		}
	}
	return count
}
