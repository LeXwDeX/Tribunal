package review

import (
	"strings"
	"testing"
)

func TestProjectBackgroundKeepsRepositoryRulesBeforeContext(t *testing.T) {
	d := &Dossier{
		Language:    "zh-CN",
		CustomRules: []string{"custom-rule"},
		RepoDocs: RepoDocs{
			GitLabReviewConfig: "review-config",
			AGENTS:             "agents-rules",
			CLAUDE:             "claude-rules",
		},
		ContextDocs: "generated-context",
	}
	background := ProjectBackground(d)
	positions := []int{
		strings.Index(background, "评审语言"),
		strings.Index(background, "项目规则"),
		strings.Index(background, ".gitlab-review.yml"),
		strings.Index(background, "AGENTS.md"),
		strings.Index(background, "CLAUDE.md"),
		strings.Index(background, "工作区上下文文档"),
	}
	for i, pos := range positions {
		if pos < 0 {
			t.Fatalf("section %d missing from background:\n%s", i, background)
		}
		if i > 0 && pos < positions[i-1] {
			t.Fatalf("section %d appears before section %d", i, i-1)
		}
	}
	if BackgroundBudgetWarning(d) != "" {
		t.Fatalf("small background warned: %q", BackgroundBudgetWarning(d))
	}
}

func TestBackgroundBudgetWarningDistinguishesRulesFromContext(t *testing.T) {
	rules := &Dossier{RepoDocs: RepoDocs{AGENTS: strings.Repeat("a", MaxBackgroundRunes+1)}}
	if got := BackgroundBudgetWarning(rules); got != backgroundRulesTruncated {
		t.Fatalf("rules warning = %q", got)
	}
	contextOnly := &Dossier{
		RepoDocs:    RepoDocs{AGENTS: "small rules"},
		ContextDocs: strings.Repeat("c", MaxBackgroundRunes+1),
	}
	if got := BackgroundBudgetWarning(contextOnly); got != backgroundContextTruncated {
		t.Fatalf("context warning = %q", got)
	}
}
