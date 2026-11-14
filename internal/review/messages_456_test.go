package review

import (
	"fmt"
	"strings"
	"testing"
)

// productionIncompleteSummary reproduces the summary of review 01a1bfbe: no
// findings and four review lanes that did not complete. The message templates
// are the stored English ones; only the published comment is localized.
func productionIncompleteSummary() string {
	return incompleteSummary([]UnresolvedSignal{
		{Node: "cr.p1", Kind: UnresolvedEvidence, Message: "lane cr.p1 (code-review) did not complete: skipped"},
		{Node: "rv.p1", Kind: UnresolvedEvidence, Message: "lane rv.p1 (review) did not complete: skipped"},
		{Node: "reasoner", Kind: UnresolvedEvidence, Message: "lane reasoner (reasoner) did not complete: failed"},
		{Node: "validate", Kind: UnresolvedEvidence, Message: "lane validate (validate) did not complete: skipped"},
	})
}

// productionFindings reproduces the group view of review 01a1d416: 14 findings
// with Chinese prose that must still render normally.
func productionFindings(t *testing.T) []Group {
	t.Helper()
	groups := make([]Group, 0, 14)
	for i := 0; i < 14; i++ {
		severity := SeverityP2
		if i%5 == 0 {
			severity = SeverityP1
		}
		groups = append(groups, Group{
			ID:         fmt.Sprintf("g_prod_%02d", i),
			Severity:   severity,
			Category:   "logic",
			Path:       "src/module.go",
			Line:       10 + i,
			Side:       SideNew,
			Title:      fmt.Sprintf("条件判断与边界不一致（第 %d 处）", i+1),
			Body:       "当输入为空时条件判断会跳过校验并返回成功，调用方拿到错误结果。",
			Confidence: ConfidenceHigh,
			Decision:   &FindingDecision{Action: ActionPublish, Source: "rules"},
			PositionOK: true,
		})
	}
	return groups
}

func TestProductionIncompleteSummaryRendersChineseReasons(t *testing.T) {
	body, err := RenderSummary(SummaryInput{
		Verdict:     VerdictIncomplete,
		Summary:     productionIncompleteSummary(),
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{
		"cr.p1", "rv.p1", "(code-review)", "(reasoner)", "(validate)",
		"did not complete", "具体缺口",
	} {
		if strings.Contains(body, banned) {
			t.Fatalf("published summary leaks internal diagnostic %q:\n%s", banned, body)
		}
	}
	for _, want := range []string{
		"已评审范围的结论", "未完成原因", "代码审查通道", "推理评审通道", "验证评审通道", "下一步", "控制台",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("localized summary lacks %q:\n%s", want, body)
		}
	}
	// English status words must not survive as the user-facing reason.
	for _, banned := range []string{" skipped", " failed"} {
		if strings.Contains(body, banned) {
			t.Fatalf("English status %q was published:\n%s", banned, body)
		}
	}
}

func TestProductionSynthesizeOutputsRenderWithoutMachineDiagnostics(t *testing.T) {
	const coverageMessage = "diff collection omitted 1 of 20 changed file(s): 19 included in the materials snapshot, 1 ignored by rules; reasons: too_large=1; omitted paths: assets/hero.png"
	cases := []struct {
		name     string
		summary  string
		groups   []Group
		warnings []string
	}{
		{
			name:    "01a1bfbe no findings and four unfinished lanes",
			summary: productionIncompleteSummary(),
		},
		{
			name: "01a1d416 findings with validate failed and coverage too large",
			summary: incompleteSummary([]UnresolvedSignal{
				{Node: "validate", Kind: UnresolvedBlocking, Message: "validate lane reported FAIL without a P0/P1 finding in the review scope"},
				{Node: "coverage", Kind: UnresolvedEvidence, Message: coverageMessage},
			}),
			groups: productionFindings(t),
			warnings: []string{
				"node validate (validate) was failed and its findings are unavailable",
				coverageMessage,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body, err := RenderSummary(SummaryInput{
				Verdict:     VerdictIncomplete,
				Summary:     tc.summary,
				Groups:      tc.groups,
				Warnings:    tc.warnings,
				Fingerprint: "0123456789abcdef0123456789abcdef",
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, banned := range []string{
				"cr.p1", "rv.p1", "did not complete", "lane ", "was failed",
				"diff collection", "too_large", "omitted paths", "validate lane",
			} {
				if strings.Contains(body, banned) {
					t.Fatalf("published comment leaks machine diagnostic %q:\n%s", banned, body)
				}
			}
			for _, want := range []string{"未完成原因", "下一步", "控制台"} {
				if !strings.Contains(body, want) {
					t.Fatalf("published comment lacks %q:\n%s", want, body)
				}
			}
			if len(tc.groups) > 0 && !strings.Contains(body, "**P1** ×") {
				t.Fatalf("finding counts were lost:\n%s", body)
			}
		})
	}
}

func TestUnknownMachineDiagnosticFallsBackToChinese(t *testing.T) {
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking,
		Summary: "found problems",
		Groups:  []Group{},
		Warnings: []string{
			"some new upstream diagnostic: exploded while reading the cache",
			"部分问题的历史讨论内容与当前内容不同，已保留在摘要中。",
		},
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "some new upstream diagnostic") || strings.Contains(body, "exploded") {
		t.Fatalf("unknown machine text was published verbatim:\n%s", body)
	}
	if !strings.Contains(body, "具体原因与日志见控制台") {
		t.Fatalf("unknown diagnostic lacks a Chinese fallback:\n%s", body)
	}
	if !strings.Contains(body, "部分问题的历史讨论内容与当前内容不同") {
		t.Fatalf("user-facing Chinese warning was rewritten:\n%s", body)
	}
}

func TestIncompleteReasonIsLocalized(t *testing.T) {
	cases := map[string]string{
		"review execution failed": "评审执行失败",
		"empty diff":              "diff 为空",
	}
	for reason, want := range cases {
		body, err := RenderSummary(SummaryInput{
			Verdict: VerdictIncomplete, IncompleteReason: reason,
			Fingerprint: "0123456789abcdef0123456789abcdef",
		})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(body, reason) {
			t.Fatalf("reason %q was published verbatim:\n%s", reason, body)
		}
		if !strings.Contains(body, want) {
			t.Fatalf("reason %q lacks %q:\n%s", reason, want, body)
		}
	}
}

func TestKnownBudgetWarningIsLocalized(t *testing.T) {
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "found problems", Groups: []Group{},
		Warnings:    []string{"budget exhausted: some findings were not verified"},
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "budget exhausted") {
		t.Fatalf("budget warning was published in English:\n%s", body)
	}
	if !strings.Contains(body, "预算耗尽") {
		t.Fatalf("budget warning lacks its Chinese mapping:\n%s", body)
	}
}

func TestRepoConfigWarningsKeepTheFieldName(t *testing.T) {
	body, err := RenderSummary(SummaryInput{
		Verdict: VerdictBlocking, Summary: "found problems", Groups: []Group{},
		Warnings: []string{
			".gitlab-review.yml: unknown field review.budget ignored",
			".gitlab-review.yml: language is too long and was ignored",
		},
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"未知字段 review.budget", "语言过长"} {
		if !strings.Contains(body, want) {
			t.Fatalf("repo config warning lacks %q:\n%s", want, body)
		}
	}
	for _, banned := range []string{"unknown field", "language is too long"} {
		if strings.Contains(body, banned) {
			t.Fatalf("repo config warning kept machine text %q:\n%s", banned, body)
		}
	}
}
