package config

import (
	"fmt"
	"path"
	"strings"
)

// Built-in review_defaults limits. Package config cannot import
// internal/review (review imports config), so these mirror defaultConfigYAML
// and review.HardMaxFiles; keep them in sync.
const (
	reviewDefaultMaxFiles     = 200
	reviewMaxFilesHardLimit   = 200
	reviewDefaultMaxDiffLines = 30000
	reviewDefaultLanguage     = "zh-CN"
)

// normalizeReviewDefaults replaces values that validateReviewDefaultsPolicy
// rejects, so a base config file cannot silently disable diff truncation or
// filter every merge request (#540). Each replacement appends a warning that
// names the field but never repeats its configured value.
func normalizeReviewDefaults(cfg *Config) {
	if cfg == nil {
		return
	}
	warn := func(format string, args ...any) {
		cfg.Warnings = append(cfg.Warnings, fmt.Sprintf(format, args...))
	}
	defaults := &cfg.ReviewDefaults

	switch {
	case defaults.MaxFiles <= 0:
		defaults.MaxFiles = reviewDefaultMaxFiles
		warn("review_defaults.max_files must be positive; the built-in default was used")
	case defaults.MaxFiles > reviewMaxFilesHardLimit:
		defaults.MaxFiles = reviewMaxFilesHardLimit
		warn("review_defaults.max_files exceeds the supported maximum; the supported maximum was used")
	}
	if defaults.MaxDiffLines <= 0 {
		defaults.MaxDiffLines = reviewDefaultMaxDiffLines
		warn("review_defaults.max_diff_lines must be positive; the built-in default was used")
	}
	if strings.TrimSpace(defaults.Language) == "" {
		defaults.Language = reviewDefaultLanguage
		warn("review_defaults.language must not be empty; the built-in default was used")
	}

	defaults.TargetBranches = normalizeTargetBranches(defaults.TargetBranches, warn)
	defaults.LabelsInclude = dropBlankEntries(defaults.LabelsInclude, "review_defaults.labels_include", warn)
	defaults.LabelsExclude = dropBlankEntries(defaults.LabelsExclude, "review_defaults.labels_exclude", warn)
	defaults.Ignore = dropBlankEntries(defaults.Ignore, "review_defaults.ignore", warn)
	defaults.CustomRules = dropBlankEntries(defaults.CustomRules, "review_defaults.custom_rules", warn)

	if len(defaults.ProfileByLabel) > 0 {
		kept := make(map[string]string, len(defaults.ProfileByLabel))
		for _, label := range sortedKeys(defaults.ProfileByLabel) {
			profile := defaults.ProfileByLabel[label]
			switch {
			case strings.TrimSpace(label) == "":
				warn("review_defaults.profile_by_label must not contain an empty label; the entry was ignored")
			case strings.TrimSpace(profile) == "":
				warn("review_defaults.profile_by_label.%s must not be empty; the entry was ignored", label)
			case profile != ReviewProfile:
				warn("review_defaults.profile_by_label.%s refers to an unknown review profile; the entry was ignored", label)
			default:
				kept[label] = profile
			}
		}
		defaults.ProfileByLabel = kept
	}
}

// normalizeTargetBranches drops empty and malformed globs. Dropping the last
// pattern restores the documented empty-list meaning: the project default
// branch only.
func normalizeTargetBranches(patterns []string, warn func(string, ...any)) []string {
	if patterns == nil {
		return nil
	}
	kept := make([]string, 0, len(patterns))
	for i, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" {
			warn("review_defaults.target_branches[%d] is empty and was ignored", i)
			continue
		}
		if _, err := path.Match(pattern, "probe"); err != nil {
			warn("review_defaults.target_branches[%d] is not a valid glob and was ignored", i)
			continue
		}
		kept = append(kept, pattern)
	}
	return kept
}

func dropBlankEntries(values []string, field string, warn func(string, ...any)) []string {
	if values == nil {
		return nil
	}
	kept := make([]string, 0, len(values))
	for i, value := range values {
		if strings.TrimSpace(value) == "" {
			warn("%s[%d] is empty and was ignored", field, i)
			continue
		}
		kept = append(kept, value)
	}
	return kept
}
