package review

import (
	"strings"
	"testing"
)

// readmeWhitelistGroups reproduces the production duplicate report: one inline
// README finding plus two pathless findings that describe the same whitelist
// gap with different prose titles. Only the bodies name the same file and
// section, which is the extra folding evidence this issue allows.
func readmeWhitelistGroups() []Group {
	publish := &FindingDecision{Action: ActionPublish, Source: "rules"}
	inline := Group{
		ID: "g_readme_inline", Path: "README.md", Line: 18, Side: SideNew, Category: "docs",
		Severity: SeverityP2,
		Title:    "文档未说明敏感配置需要排除，读者可能直接提交",
		Body:     "README.md 的「白名单」章节只列出示例配置，没有说明 config/secrets.yaml 必须排除；按该步骤操作会把敏感配置提交到仓库。",
		Decision: publish, PositionOK: true,
	}
	duplicate := Group{
		ID: "g_readme_dup", Category: "docs", Severity: SeverityP2,
		Title:    "部署说明缺少关键排除项",
		Body:     "README.md 的「白名单」章节缺少 config/secrets.yaml 的排除说明，读者按部署步骤操作时会提交敏感配置。",
		Decision: publish,
	}
	informational := Group{
		ID: "g_readme_info", Category: "docs", Severity: SeverityInfo,
		Title:    "示例步骤会引导读者提交机密文件",
		Body:     "README.md 的「白名单」章节没有提到 config/secrets.yaml，示例步骤会引导读者把敏感配置提交到仓库。",
		Decision: publish,
	}
	return []Group{inline, duplicate, informational}
}

func TestReadmeWhitelistTripleFoldsIntoInlineRelated(t *testing.T) {
	body := summaryFor(t, readmeWhitelistGroups())
	if strings.Contains(body, "### 无法定位到行的问题") {
		t.Fatalf("the two pathless duplicates must not be listed separately:\n%s", body)
	}
	if !strings.Contains(body, "### 行内问题（1）") {
		t.Fatalf("only the inline README finding is a positioned entry:\n%s", body)
	}
	if strings.Contains(body, "**P2** × 2") || strings.Contains(body, "**info**") {
		t.Fatalf("header counts still count the folded duplicates:\n%s", body)
	}
	if !strings.Contains(body, "**P2** × 1") {
		t.Fatalf("header count must match the single published entry:\n%s", body)
	}
	if strings.Contains(body, "#### [P2]部署说明缺少关键排除项") ||
		strings.Contains(body, "#### [info]示例步骤会引导读者提交机密文件") {
		t.Fatalf("folded duplicates are still listed as their own entries:\n%s", body)
	}
	if !strings.Contains(body, "相关描述") ||
		!strings.Contains(body, "部署说明缺少关键排除项") ||
		!strings.Contains(body, "示例步骤会引导读者提交机密文件") {
		t.Fatalf("folded duplicates must be referenced under 相关描述:\n%s", body)
	}
}

func TestBodyEvidenceDoesNotFoldDistinctProblems(t *testing.T) {
	inline := mkGroup("cache", 30, "logic", SeverityP2, "缓存淘汰顺序错误",
		"cache.go 的 evict 先删除最新条目而不是最旧条目，缓存命中率下降。")
	other := mkGroup("token", 0, "security", SeverityP2, "令牌写入日志",
		"cache.go 的 writeToken 把完整令牌写入日志，运维人员可以读取。")
	other.Path, other.Line, other.PositionOK = "", 0, false
	body := summaryFor(t, []Group{inline, other})
	if !strings.Contains(body, "### 无法定位到行的问题（1）") || listedSummaryItem(body, "令牌写入日志") == false {
		t.Fatalf("a shared file alone must not fold distinct problems:\n%s", body)
	}
}

func TestRuleSummarySeparatesVerdictCountFromInlineAndSummaryOnly(t *testing.T) {
	body, err := RenderSummary(SummaryInput{
		Verdict:     VerdictBlocking,
		Summary:     "规则模式汇总：共 3 条问题，已发布 3 条。",
		RuleSummary: true,
		Groups:      readmeWhitelistGroups(),
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "已发布 3 条") {
		t.Fatalf("the misleading published count is still rendered:\n%s", body)
	}
	want := "裁决发布 3 条（行内 1 条，仅汇总 0 条，另有 2 条并入相关描述）"
	if !strings.Contains(body, want) {
		t.Fatalf("summary counts must separate verdict, inline and summary-only counts; want %q:\n%s", want, body)
	}
}

func TestRuleSummaryCountsSummaryOnlyEntries(t *testing.T) {
	inline := mkGroup("inline", 10, "logic", SeverityP2, "条件判断遗漏空值", "输入为空时跳过校验。")
	unpositioned := mkGroup("unpositioned", 0, "security", SeverityP1, "令牌写入日志", "日志输出包含完整令牌字符串。")
	unpositioned.Path, unpositioned.Line, unpositioned.PositionOK = "", 0, false
	body, err := RenderSummary(SummaryInput{
		Verdict:     VerdictBlocking,
		Summary:     "规则模式汇总：共 2 条问题，已发布 2 条。",
		RuleSummary: true,
		Groups:      []Group{inline, unpositioned},
		Fingerprint: "0123456789abcdef0123456789abcdef",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := "裁决发布 2 条（行内 1 条，仅汇总 1 条）"
	if !strings.Contains(body, want) {
		t.Fatalf("summary-only entries were not counted separately; want %q:\n%s", want, body)
	}
	if !strings.Contains(body, "### 无法定位到行的问题（1）") {
		t.Fatalf("summary-only section count is wrong:\n%s", body)
	}
}
