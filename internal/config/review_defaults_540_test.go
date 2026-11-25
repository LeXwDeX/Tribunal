package config

import (
	"reflect"
	"strings"
	"testing"
)

// #540: a base config that sets review_defaults to values the console policy
// rejects must be normalized at load and warned about, not accepted silently.
func TestLoadNormalizesInvalidReviewDefaults(t *testing.T) {
	path := writeConfig(t, "config.yaml", `
review_defaults:
  language: " "
  target_branches: ["release/*", "release/*[", ""]
  labels_include: ["ok", " "]
  labels_exclude: [""]
  ignore: ["  "]
  custom_rules: ["rule", ""]
  max_files: 0
  max_diff_lines: 0
  profile_by_label:
    bug: removed
    ok: default
`)
	cfg := loadConfig(t, path)
	defaults := cfg.ReviewDefaults

	if defaults.MaxFiles != 1000 || defaults.MaxDiffLines != 300000 {
		t.Fatalf("normalized limits = %d/%d, want 1000/300000", defaults.MaxFiles, defaults.MaxDiffLines)
	}
	if defaults.Language != "zh-CN" {
		t.Fatalf("normalized language = %q, want zh-CN", defaults.Language)
	}
	if !reflect.DeepEqual(defaults.TargetBranches, []string{"release/*"}) {
		t.Fatalf("target_branches = %v, want [release/*]", defaults.TargetBranches)
	}
	if !reflect.DeepEqual(defaults.LabelsInclude, []string{"ok"}) {
		t.Fatalf("labels_include = %v, want [ok]", defaults.LabelsInclude)
	}
	if len(defaults.LabelsExclude) != 0 || len(defaults.Ignore) != 0 {
		t.Fatalf("empty list entries survived: exclude=%v ignore=%v", defaults.LabelsExclude, defaults.Ignore)
	}
	if !reflect.DeepEqual(defaults.CustomRules, []string{"rule"}) {
		t.Fatalf("custom_rules = %v, want [rule]", defaults.CustomRules)
	}
	if !reflect.DeepEqual(defaults.ProfileByLabel, map[string]string{"ok": "default"}) {
		t.Fatalf("profile_by_label = %v, want {ok: default}", defaults.ProfileByLabel)
	}

	warnings := strings.Join(cfg.Warnings, "\n")
	for _, field := range []string{
		"max_files", "max_diff_lines", "language", "target_branches",
		"labels_include", "labels_exclude", "ignore", "custom_rules", "profile_by_label",
	} {
		if !strings.Contains(warnings, "review_defaults."+field) {
			t.Errorf("warnings do not name review_defaults.%s: %q", field, warnings)
		}
	}
	if strings.Contains(warnings, "release/*[") || strings.Contains(warnings, "removed") {
		t.Fatalf("warnings leaked configuration values: %q", warnings)
	}
}

func TestLoadKeepsValidReviewDefaultsWithoutWarnings(t *testing.T) {
	cfg := loadConfig(t)
	if len(cfg.Warnings) != 0 {
		t.Fatalf("valid defaults produced warnings: %v", cfg.Warnings)
	}
	if cfg.ReviewDefaults.MaxFiles != 1000 || cfg.ReviewDefaults.MaxDiffLines != 300000 {
		t.Fatalf("valid defaults changed: %+v", cfg.ReviewDefaults)
	}
}

const reviewDefaultsPolicyBase = `
gitlab:
  url: "https://gitlab.example.com"
storage:
  driver: sqlite
  dsn: "file:data/review.db"
secrets:
  master_key_env: REVIEW_MASTER_KEY
auth:
  bootstrap_token_env: REVIEW_ADMIN_TOKEN
providers:
  primary:
    protocol: anthropic
    model: claude-sonnet
    api_key_env: PRIMARY_KEY
    max_tokens: 16000
review_defaults:
  max_diff_lines: 0
  target_branches: ["release/*["]
`

// #540: one bad inherited review_defaults value must not make every unrelated
// overlay write fail. The candidate is shown normalized with warnings.
func TestOverlayWriterAllowsUnrelatedPatchOverInvalidInheritedDefaults(t *testing.T) {
	harness := newOverlayHarness(t, reviewDefaultsPolicyBase, "")
	result, err := harness.apply(harness.request(OverlayChange{Path: []string{"providers", "primary", "max_tokens"}, Value: 2000}))
	if err != nil {
		t.Fatalf("apply over invalid inherited review_defaults: %v", err)
	}
	defaults := result.Candidate.Config.ReviewDefaults
	if defaults.MaxDiffLines != 300000 || len(defaults.TargetBranches) != 0 {
		t.Fatalf("candidate defaults were not normalized: %+v", defaults)
	}
	if len(result.Candidate.Warnings) == 0 {
		t.Fatal("candidate snapshot has no normalization warnings")
	}
}

// A valid write to one review_defaults field must still be accepted while an
// invalid sibling inherited from the base file is normalized (#540).
func TestOverlayWriterAppliesValidReviewDefaultsOverInvalidInherited(t *testing.T) {
	harness := newOverlayHarness(t, reviewDefaultsPolicyBase, "")
	result, err := harness.apply(harness.request(OverlayChange{Path: []string{"review_defaults", "max_files"}, Value: 150}))
	if err != nil {
		t.Fatalf("apply valid review_defaults change: %v", err)
	}
	defaults := result.Candidate.Config.ReviewDefaults
	if defaults.MaxFiles != 150 || defaults.MaxDiffLines != 300000 {
		t.Fatalf("candidate defaults = %+v, want max_files 150 and normalized max_diff_lines", defaults)
	}
}

// The API still rejects an invalid value in the field the request touches.
func TestOverlayWriterRejectsTouchedInvalidReviewDefaults(t *testing.T) {
	harness := newOverlayHarness(t, reviewDefaultsPolicyBase, "")
	_, err := harness.apply(harness.request(OverlayChange{Path: []string{"review_defaults", "max_files"}, Value: 0}))
	requireOverlayError(t, err, OverlayErrorInvalidConfig)
}
