package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/review"
)

// settingsOverride carries only the fields present in a registration request.
// Pointer fields keep an explicit false or empty list distinct from omission.
type settingsOverride struct {
	Language             *string            `json:"language,omitempty"`
	SkipDraft            *bool              `json:"skip_draft,omitempty"`
	TargetBranches       *[]string          `json:"target_branches,omitempty"`
	LabelsInclude        *[]string          `json:"labels_include,omitempty"`
	LabelsExclude        *[]string          `json:"labels_exclude,omitempty"`
	Ignore               *[]string          `json:"ignore,omitempty"`
	MaxFiles             *int               `json:"max_files,omitempty"`
	MaxDiffLines         *int               `json:"max_diff_lines,omitempty"`
	CommitStatus         *bool              `json:"commit_status,omitempty"`
	PostFailureNote      *bool              `json:"post_failure_note,omitempty"`
	ShowModelAttribution *bool              `json:"show_model_attribution,omitempty"`
	AllowRepoConfig      *bool              `json:"allow_repo_config,omitempty"`
	AllowRepoSkills      *bool              `json:"allow_repo_skills,omitempty"`
	ProfileByLabel       *map[string]string `json:"profile_by_label,omitempty"`
	CustomRules          *[]string          `json:"custom_rules,omitempty"`
}

// ParseSettings validates a settings object and returns canonical override
// JSON. The platform review defaults are not merged into the stored value.
func ParseSettings(raw json.RawMessage, profiles map[string]config.ProfileConfig) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return json.RawMessage(`{}`), nil
	}
	if trimmed[0] != '{' {
		return nil, validationError("settings must be a JSON object", Detail{Field: "settings", Message: "must be an object"})
	}

	var override settingsOverride
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&override); err != nil {
		return nil, validationError("invalid settings", Detail{Field: "settings", Message: "contains an unknown field or invalid value"})
	}
	if err := ensureEOF(dec); err != nil {
		return nil, validationError("invalid settings", Detail{Field: "settings", Message: "must contain one JSON object"})
	}

	var details []Detail
	requireNonEmpty := func(field string, values *[]string) {
		if values == nil {
			return
		}
		if *values == nil {
			*values = []string{}
		}
		for i, value := range *values {
			if strings.TrimSpace(value) == "" {
				details = append(details, Detail{
					Field:   fmt.Sprintf("settings.%s[%d]", field, i),
					Message: "must not be empty",
				})
			}
		}
	}
	requireNonEmpty("target_branches", override.TargetBranches)
	requireNonEmpty("labels_include", override.LabelsInclude)
	requireNonEmpty("labels_exclude", override.LabelsExclude)
	requireNonEmpty("ignore", override.Ignore)
	if override.Ignore != nil {
		for i, pattern := range *override.Ignore {
			if err := review.ValidateIgnorePatterns([]string{pattern}); err != nil {
				details = append(details, Detail{
					Field:   fmt.Sprintf("settings.ignore[%d]", i),
					Message: err.Error(),
				})
			}
		}
	}
	requireNonEmpty("custom_rules", override.CustomRules)

	if override.Language != nil && strings.TrimSpace(*override.Language) == "" {
		details = append(details, Detail{Field: "settings.language", Message: "must not be empty"})
	}
	if override.MaxFiles != nil && *override.MaxFiles <= 0 {
		details = append(details, Detail{Field: "settings.max_files", Message: "must be positive"})
	}
	if override.MaxDiffLines != nil && *override.MaxDiffLines <= 0 {
		details = append(details, Detail{Field: "settings.max_diff_lines", Message: "must be positive"})
	}
	if override.TargetBranches != nil {
		for i, pattern := range *override.TargetBranches {
			if pattern == "" {
				continue
			}
			if _, err := path.Match(pattern, "probe"); err != nil {
				details = append(details, Detail{
					Field:   fmt.Sprintf("settings.target_branches[%d]", i),
					Message: "invalid branch glob",
				})
			}
		}
	}
	if override.ProfileByLabel != nil {
		if *override.ProfileByLabel == nil {
			*override.ProfileByLabel = map[string]string{}
		}
		labels := make([]string, 0, len(*override.ProfileByLabel))
		for label := range *override.ProfileByLabel {
			labels = append(labels, label)
		}
		sort.Strings(labels)
		for _, label := range labels {
			profile := (*override.ProfileByLabel)[label]
			if strings.TrimSpace(label) == "" {
				details = append(details, Detail{Field: "settings.profile_by_label", Message: "label must not be empty"})
				continue
			}
			if profile == "" {
				details = append(details, Detail{
					Field:   "settings.profile_by_label." + label,
					Message: "profile must not be empty",
				})
				continue
			}
			if _, ok := profiles[profile]; !ok {
				details = append(details, Detail{
					Field:   "settings.profile_by_label." + label,
					Message: "profile is not defined",
				})
			}
		}
	}
	if len(details) > 0 {
		return nil, validationError("invalid project settings", details...)
	}

	encoded, err := json.Marshal(&override)
	if err != nil {
		return nil, internalError()
	}
	return encoded, nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("app: trailing JSON value")
	}
	return err
}

// ResolveSettings merges stored overrides over the platform defaults without
// mutating the defaults. ignore lists are appended; all other lists replace.
func ResolveSettings(raw json.RawMessage, defaults config.ProjectSettings) (config.ProjectSettings, error) {
	result := cloneSettings(defaults)
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return result, nil
	}

	var override settingsOverride
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&override); err != nil {
		return config.ProjectSettings{}, errors.New("app: invalid stored settings")
	}
	if err := ensureEOF(dec); err != nil {
		return config.ProjectSettings{}, errors.New("app: invalid stored settings")
	}

	if override.Language != nil {
		result.Language = *override.Language
	}
	if override.SkipDraft != nil {
		result.SkipDraft = *override.SkipDraft
	}
	if override.TargetBranches != nil {
		result.TargetBranches = append([]string(nil), (*override.TargetBranches)...)
	}
	if override.LabelsInclude != nil {
		result.LabelsInclude = append([]string(nil), (*override.LabelsInclude)...)
	}
	if override.LabelsExclude != nil {
		result.LabelsExclude = append([]string(nil), (*override.LabelsExclude)...)
	}
	if override.Ignore != nil {
		result.Ignore = appendUnique(result.Ignore, *override.Ignore)
	}
	if override.MaxFiles != nil {
		result.MaxFiles = *override.MaxFiles
	}
	if override.MaxDiffLines != nil {
		result.MaxDiffLines = *override.MaxDiffLines
	}
	if override.CommitStatus != nil {
		result.CommitStatus = *override.CommitStatus
	}
	if override.PostFailureNote != nil {
		result.PostFailureNote = *override.PostFailureNote
	}
	if override.ShowModelAttribution != nil {
		result.ShowModelAttribution = *override.ShowModelAttribution
	}
	if override.AllowRepoConfig != nil {
		result.AllowRepoConfig = *override.AllowRepoConfig
	}
	if override.AllowRepoSkills != nil {
		result.AllowRepoSkills = *override.AllowRepoSkills
	}
	if override.ProfileByLabel != nil {
		result.ProfileByLabel = make(map[string]string, len(*override.ProfileByLabel))
		for key, value := range *override.ProfileByLabel {
			result.ProfileByLabel[key] = value
		}
	}
	if override.CustomRules != nil {
		result.CustomRules = append([]string(nil), (*override.CustomRules)...)
	}
	return result, nil
}

func cloneSettings(in config.ProjectSettings) config.ProjectSettings {
	out := in
	out.TargetBranches = append([]string(nil), in.TargetBranches...)
	out.LabelsInclude = append([]string(nil), in.LabelsInclude...)
	out.LabelsExclude = append([]string(nil), in.LabelsExclude...)
	out.Ignore = append([]string(nil), in.Ignore...)
	out.CustomRules = append([]string(nil), in.CustomRules...)
	if in.ProfileByLabel != nil {
		out.ProfileByLabel = make(map[string]string, len(in.ProfileByLabel))
		for key, value := range in.ProfileByLabel {
			out.ProfileByLabel[key] = value
		}
	}
	return out
}

func appendUnique(base, extra []string) []string {
	seen := make(map[string]struct{}, len(base)+len(extra))
	result := make([]string, 0, len(base)+len(extra))
	for _, values := range [][]string{base, extra} {
		for _, value := range values {
			if _, ok := seen[value]; ok {
				continue
			}
			seen[value] = struct{}{}
			result = append(result, value)
		}
	}
	return result
}

// ProfileForLabels returns the first label-mapped profile, else fallback.
func ProfileForLabels(labels []string, fallback string, settings config.ProjectSettings) string {
	for _, label := range labels {
		if profile, ok := settings.ProfileByLabel[label]; ok && profile != "" {
			return profile
		}
	}
	if fallback != "" {
		return fallback
	}
	return "default"
}

// BranchAllowed reports whether branch matches the configured patterns. An
// empty pattern list means the project default branch only.
func BranchAllowed(branch, defaultBranch string, patterns []string) bool {
	if branch == "" {
		return false
	}
	if len(patterns) == 0 {
		return branch == defaultBranch
	}
	for _, pattern := range patterns {
		if ok, err := path.Match(pattern, branch); err == nil && ok {
			return true
		}
	}
	return false
}

// LabelsAllowed applies exclude-first then include semantics.
func LabelsAllowed(labels, include, exclude []string) bool {
	for _, label := range labels {
		for _, blocked := range exclude {
			if label == blocked {
				return false
			}
		}
	}
	if len(include) == 0 {
		return true
	}
	for _, label := range labels {
		for _, wanted := range include {
			if label == wanted {
				return true
			}
		}
	}
	return false
}
