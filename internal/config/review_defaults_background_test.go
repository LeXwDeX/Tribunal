package config

import (
	"strings"
	"testing"
)

// The built-in default budget must stay at 100000 so existing deployments keep
// the historical background limit.
func TestReviewDefaultsMaxBackgroundRunesDefault(t *testing.T) {
	cfg := loadConfig(t)
	if cfg.ReviewDefaults.MaxBackgroundRunes != 100000 {
		t.Fatalf("default max_background_runes = %d, want 100000", cfg.ReviewDefaults.MaxBackgroundRunes)
	}
	if len(cfg.Warnings) != 0 {
		t.Fatalf("valid defaults produced warnings: %v", cfg.Warnings)
	}
}

// A non-positive max_background_runes is normalized to the built-in default
// with a warning instead of being accepted silently.
func TestReviewDefaultsMaxBackgroundRunesNormalizesNonPositive(t *testing.T) {
	path := writeConfig(t, "config.yaml", `
review_defaults:
  max_background_runes: 0
`)
	cfg := loadConfig(t, path)
	if cfg.ReviewDefaults.MaxBackgroundRunes != 100000 {
		t.Fatalf("normalized max_background_runes = %d, want 100000", cfg.ReviewDefaults.MaxBackgroundRunes)
	}
	found := false
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "review_defaults.max_background_runes") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a warning naming review_defaults.max_background_runes, got %v", cfg.Warnings)
	}
}

// A positive configured budget is preserved without a normalization warning.
func TestReviewDefaultsMaxBackgroundRunesKeepsPositive(t *testing.T) {
	path := writeConfig(t, "config.yaml", `
review_defaults:
  max_background_runes: 50000
`)
	cfg := loadConfig(t, path)
	if cfg.ReviewDefaults.MaxBackgroundRunes != 50000 {
		t.Fatalf("max_background_runes = %d, want 50000", cfg.ReviewDefaults.MaxBackgroundRunes)
	}
	for _, warning := range cfg.Warnings {
		if strings.Contains(warning, "max_background_runes") {
			t.Fatalf("a valid value produced a warning: %q", warning)
		}
	}
}
