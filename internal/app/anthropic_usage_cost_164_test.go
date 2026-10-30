package app

import (
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
)

// Anthropic normalization folds cache reads and writes into InputTokens, so
// cost must price those normalized totals once. The cache detail fields are
// non-zero here to catch a cost function that adds them again.
func TestAnthropicUsageCost164PricesNormalizedTokensOnce(t *testing.T) {
	cost := newCostFunc(map[string]config.ProviderConfig{
		"anthropic": {Price: &config.PriceConfig{InputPerMTok: 3, OutputPerMTok: 15}},
	})
	usage := llm.Usage{
		InputTokens:      15, // includes 2 cache-read and 3 cache-write tokens
		OutputTokens:     5,
		CacheReadTokens:  2,
		CacheWriteTokens: 3,
		ReasoningTokens:  4,
	}
	// 15 input tokens * $3/M + 5 output tokens * $15/M = 120 micro-dollars.
	if got := cost("anthropic", usage); got != 120 {
		t.Fatalf("cost = %d micros, want 120 (cache details must not be priced again)", got)
	}
}
