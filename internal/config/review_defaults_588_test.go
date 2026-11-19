package config

import (
	"strings"
	"testing"
)

// #588: the loader clamps review_defaults.max_files to the supported maximum
// and names the field in a warning.
func TestLoadClampsReviewDefaultsMaxFilesAboveHardLimit(t *testing.T) {
	path := writeConfig(t, "config.yaml", `
gitlab:
  url: "https://gitlab.example.com"
review_defaults:
  max_files: 300
`)
	cfg := loadConfig(t, path)
	if cfg.ReviewDefaults.MaxFiles != 200 {
		t.Fatalf("max_files = %d, want the supported maximum 200", cfg.ReviewDefaults.MaxFiles)
	}
	warnings := strings.Join(cfg.Warnings, "\n")
	if !strings.Contains(warnings, "review_defaults.max_files") {
		t.Fatalf("warnings do not name review_defaults.max_files: %q", warnings)
	}
}

// #588: the console write path must reject the same out-of-range value the
// loader clamps instead of accepting it and silently storing 300.
func TestOverlayWriterRejectsReviewDefaultsMaxFilesAboveHardLimit(t *testing.T) {
	harness := newOverlayHarness(t, reviewDefaultsPolicyBase, "")
	_, err := harness.apply(harness.request(OverlayChange{Path: []string{"review_defaults", "max_files"}, Value: 300}))
	writeErr := requireOverlayError(t, err, OverlayErrorInvalidConfig)
	details := strings.Join(writeErr.Details, "\n")
	if !strings.Contains(details, "review_defaults.max_files") {
		t.Fatalf("details do not name review_defaults.max_files: %v", writeErr.Details)
	}
	if strings.Contains(details, "300") {
		t.Fatalf("details leaked the configured value: %v", writeErr.Details)
	}
}

// An out-of-range value inherited from the base file still does not block a
// write to an unrelated field; the candidate is shown clamped (#588, #540).
func TestOverlayWriterAllowsUnrelatedPatchOverInheritedMaxFilesAboveHardLimit(t *testing.T) {
	base := strings.Replace(reviewDefaultsPolicyBase, "review_defaults:", "review_defaults:\n  max_files: 300", 1)
	harness := newOverlayHarness(t, base, "")
	result, err := harness.apply(harness.request(OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 2000}))
	if err != nil {
		t.Fatalf("apply over inherited out-of-range max_files: %v", err)
	}
	if got := result.Candidate.Config.ReviewDefaults.MaxFiles; got != 200 {
		t.Fatalf("candidate max_files = %d, want 200", got)
	}
	if !strings.Contains(strings.Join(result.Candidate.Warnings, "\n"), "review_defaults.max_files") {
		t.Fatalf("candidate has no max_files warning: %v", result.Candidate.Warnings)
	}
}
