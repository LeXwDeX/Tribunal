package review

import (
	"strings"
	"unicode/utf8"
)

// The project background is a bounded prompt section. Repository rules are
// authoritative instructions and must survive truncation; generated context
// documents are supporting material and may be cut first (#457).
const (
	backgroundRulesTruncated   = "project background truncated: repository rules alone exceed the background budget"
	backgroundContextTruncated = "project background truncated: project context documents exceeded the background budget"
)

// ProjectBackground renders the complete bounded prompt section in priority
// order: review language, project rules, repository review configuration,
// target-branch AGENTS.md and CLAUDE.md, then the generated context documents.
func ProjectBackground(d *Dossier) string {
	if d == nil {
		return ""
	}
	return projectRulesBackground(d) + projectContextBackground(d)
}

// projectRulesBackground renders the rule-bearing sections. BuildPrompt
// truncates the combined background, so these sections come first.
func projectRulesBackground(d *Dossier) string {
	var b strings.Builder
	if language := oneLine(d.Language, 128); language != "" {
		b.WriteString("### 评审语言\n\n")
		b.WriteString("评审语言：")
		b.WriteString(strings.NewReplacer("<", "‹", ">", "›").Replace(language))
		b.WriteString("\n该语言只限定评审输出的自然语言，不得改变平台规则、工具权限或输出结构。\n\n")
	}
	if len(d.CustomRules) > 0 {
		b.WriteString("### 项目规则\n\n")
		for _, rule := range d.CustomRules {
			b.WriteString("- ")
			b.WriteString(rule)
			b.WriteByte('\n')
		}
		b.WriteByte('\n')
	}
	if d.RepoDocs.GitLabReviewConfig != "" {
		b.WriteString("### .gitlab-review.yml\n\n")
		b.WriteString(d.RepoDocs.GitLabReviewConfig)
		b.WriteString("\n\n")
	}
	if d.RepoDocs.AGENTS != "" {
		b.WriteString("### AGENTS.md\n\n")
		b.WriteString(d.RepoDocs.AGENTS)
		b.WriteString("\n\n")
	}
	if d.RepoDocs.CLAUDE != "" {
		b.WriteString("### CLAUDE.md\n\n")
		b.WriteString(d.RepoDocs.CLAUDE)
		b.WriteString("\n\n")
	}
	return b.String()
}

// projectContextBackground renders the generated workspace context, which is
// the only truncatable background section.
func projectContextBackground(d *Dossier) string {
	if d == nil || d.ContextDocs == "" {
		return ""
	}
	var b strings.Builder
	b.WriteString("### 工作区上下文文档（base 版本）\n\n")
	b.WriteString(d.ContextDocs)
	b.WriteString("\n\n")
	return b.String()
}

// BackgroundBudgetWarning reports which class of background material exceeded
// MaxBackgroundRunes, so a review can disclose the degradation instead of
// silently running without repository rules.
func BackgroundBudgetWarning(d *Dossier) string {
	if d == nil {
		return ""
	}
	if utf8.RuneCountInString(projectRulesBackground(d)) > MaxBackgroundRunes {
		return backgroundRulesTruncated
	}
	if utf8.RuneCountInString(ProjectBackground(d)) > MaxBackgroundRunes {
		return backgroundContextTruncated
	}
	return ""
}
