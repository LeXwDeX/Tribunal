package review

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/prompts"
)

// BuildPrompt honours a caller-supplied MaxBackgroundRunes budget instead of
// the package constant, while a value <= 0 falls back to the constant.
func TestBuildPromptUsesConfiguredMaxBackgroundRunes(t *testing.T) {
	bundle, err := prompts.Load("")
	if err != nil {
		t.Fatal(err)
	}

	// A budget above the constant keeps a background that the default would cut.
	large, err := BuildPrompt(PromptInput{
		Bundle:             bundle,
		Role:               prompts.CodeReview,
		ProjectBackground:  strings.Repeat("a", MaxBackgroundRunes+5000),
		MaxBackgroundRunes: MaxBackgroundRunes + 10000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if large.BackgroundTruncated {
		t.Fatal("background truncated despite a budget larger than the content")
	}
	if large.BackgroundRunes != MaxBackgroundRunes+5000 {
		t.Fatalf("background runes = %d, want %d", large.BackgroundRunes, MaxBackgroundRunes+5000)
	}

	// A budget below the content truncates at the caller-supplied limit.
	small, err := BuildPrompt(PromptInput{
		Bundle:             bundle,
		Role:               prompts.CodeReview,
		ProjectBackground:  strings.Repeat("b", 150),
		MaxBackgroundRunes: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !small.BackgroundTruncated {
		t.Fatal("expected truncation at the custom budget")
	}
	if small.BackgroundRunes != 150 {
		t.Fatalf("background runes = %d, want 150", small.BackgroundRunes)
	}
	if n := utf8.RuneCountInString(small.System[2].Text); n > 100+utf8.RuneCountInString(renderBackground("")) {
		t.Fatalf("rendered background runes = %d, exceeds the custom budget 100", n)
	}
}
