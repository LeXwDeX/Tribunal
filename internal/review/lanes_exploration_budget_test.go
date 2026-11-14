package review

import (
	"fmt"
	"strings"
	"testing"
)

func TestExplorationBudgetKeepsUnknownsBeforeFacts(t *testing.T) {
	out := ExploreOut{
		Summary:  strings.Repeat("s", 2000),
		Unknowns: make([]string, 0, 20),
	}
	for i := 0; i < 30; i++ {
		out.Facts = append(out.Facts, ExploreFact{
			Statement: fmt.Sprintf("fact-%02d", i), Path: "a.go", Evidence: strings.Repeat("e", 1000),
		})
	}
	for i := 0; i < 20; i++ {
		out.Inferences = append(out.Inferences, fmt.Sprintf("inference-%02d", i))
		out.Unknowns = append(out.Unknowns, fmt.Sprintf("unknown-%02d", i))
	}
	rendered := renderExploration(out)
	if n := len([]rune(rendered)); n > MaxExplorationRunes {
		t.Fatalf("exploration runes = %d, max %d", n, MaxExplorationRunes)
	}
	unknowns := strings.Index(rendered, "未知项")
	if unknowns < 0 {
		t.Fatalf("unknowns were dropped entirely:\n%s", rendered)
	}
	if !strings.Contains(rendered, "unknown-00") {
		t.Fatalf("the unknown section lost all items:\n%s", rendered)
	}
	if !strings.Contains(rendered, "事实丢弃") {
		t.Fatalf("dropped fact count is not reported:\n%s", rendered)
	}
	if !strings.Contains(rendered, "已截断") {
		t.Fatal("a bounded exploration must say it was truncated")
	}
	// Inferences are reserved before facts too.
	if !strings.Contains(rendered, "inference-00") {
		t.Fatalf("the inference section lost all items:\n%s", rendered)
	}
}

func TestExplorationBudgetReportsEachSection(t *testing.T) {
	out := ExploreOut{
		Summary:    "short summary",
		Facts:      []ExploreFact{{Statement: "only fact", Evidence: "only evidence"}},
		Inferences: []string{"one inference"},
		Unknowns:   []string{"one unknown"},
	}
	rendered := renderExploration(out)
	if strings.Contains(rendered, "已截断") || strings.Contains(rendered, "丢弃") {
		t.Fatalf("small exploration reported truncation:\n%s", rendered)
	}
	for _, want := range []string{"摘要：short summary", "only fact", "one inference", "one unknown"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("missing %q from:\n%s", want, rendered)
		}
	}
}
