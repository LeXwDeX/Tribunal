package app

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestParseSettingsPreservesExplicitFalse(t *testing.T) {
	raw, err := ParseSettings(json.RawMessage(`{"skip_draft":false,"labels_exclude":["no-ai"]}`), testProfiles())
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	want := `{"skip_draft":false,"labels_exclude":["no-ai"]}`
	if string(raw) != want {
		t.Fatalf("settings = %s, want %s", raw, want)
	}
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatal(err)
	}
	if value, ok := generic["skip_draft"]; !ok || value != false {
		t.Fatalf("explicit false was lost: %v", generic)
	}
}

func TestParseSettingsRequiresStrictObject(t *testing.T) {
	tests := []string{
		`[]`,
		`{"unknown":true}`,
		`{"max_files":0}`,
		`{"target_branches":["["]}`,
		`{"profile_by_label":{"label":"missing"}}`,
		`{"labels_include":[""]}`,
		`{} {}`,
	}
	for _, input := range tests {
		if _, err := ParseSettings(json.RawMessage(input), testProfiles()); err == nil {
			t.Errorf("ParseSettings(%s) unexpectedly succeeded", input)
		}
	}
	if raw, err := ParseSettings(nil, testProfiles()); err != nil || string(raw) != `{}` {
		t.Errorf("empty settings = %s, %v", raw, err)
	}
	if raw, err := ParseSettings(json.RawMessage(`null`), testProfiles()); err != nil || string(raw) != `{}` {
		t.Errorf("null settings = %s, %v", raw, err)
	}
}

func TestResolveSettingsDoesNotMutateDefaults(t *testing.T) {
	defaults := testDefaults()
	before := testDefaults()
	raw := json.RawMessage(`{
		"skip_draft": false,
		"ignore": ["**/*.lock", "custom/**"],
		"target_branches": ["release/*"],
		"profile_by_label": {"deep-review": "deep"}
	}`)
	resolved, err := ResolveSettings(raw, defaults)
	if err != nil {
		t.Fatalf("ResolveSettings: %v", err)
	}
	if resolved.SkipDraft {
		t.Error("explicit false was not applied")
	}
	if !reflect.DeepEqual(defaults, before) {
		t.Fatalf("defaults mutated: %+v", defaults)
	}
	wantIgnore := []string{"**/*.lock", "custom/**"}
	if !reflect.DeepEqual(resolved.Ignore, wantIgnore) {
		t.Errorf("ignore = %v, want %v", resolved.Ignore, wantIgnore)
	}
	if !reflect.DeepEqual(resolved.TargetBranches, []string{"release/*"}) {
		t.Errorf("target branches = %v", resolved.TargetBranches)
	}
	if resolved.ProfileByLabel["deep-review"] != "deep" {
		t.Errorf("profile mapping = %v", resolved.ProfileByLabel)
	}
	// Mutating the result must not change the defaults either.
	resolved.Ignore[0] = "changed"
	if defaults.Ignore[0] != "**/*.lock" {
		t.Fatal("resolved slice aliased the defaults")
	}
}

func TestResolveSettingsRejectsCorruptStoredValue(t *testing.T) {
	_, err := ResolveSettings(json.RawMessage(`{"bogus":1}`), testDefaults())
	if err == nil {
		t.Fatal("expected error for corrupt stored settings")
	}
}

func TestBranchAllowed(t *testing.T) {
	cases := []struct {
		branch, defaultBranch string
		patterns              []string
		want                  bool
	}{
		{"main", "main", nil, true},
		{"feature", "main", nil, false},
		{"release/1.0", "main", []string{"release/*"}, true},
		{"release/1.0/x", "main", []string{"release/*"}, false},
		{"main", "main", []string{"[bad"}, false},
		{"", "main", nil, false},
	}
	for _, tt := range cases {
		if got := BranchAllowed(tt.branch, tt.defaultBranch, tt.patterns); got != tt.want {
			t.Errorf("BranchAllowed(%q, %q, %v) = %v, want %v", tt.branch, tt.defaultBranch, tt.patterns, got, tt.want)
		}
	}
}

func TestLabelsAllowed(t *testing.T) {
	cases := []struct {
		labels, include, exclude []string
		want                     bool
	}{
		{[]string{"ok"}, nil, nil, true},
		{[]string{"no-ai"}, nil, []string{"no-ai"}, false},
		{[]string{"docs"}, []string{"review"}, nil, false},
		{[]string{"docs", "review"}, []string{"review"}, nil, true},
	}
	for _, tt := range cases {
		if got := LabelsAllowed(tt.labels, tt.include, tt.exclude); got != tt.want {
			t.Errorf("LabelsAllowed(%v, %v, %v) = %v", tt.labels, tt.include, tt.exclude, got)
		}
	}
}

func TestProfileForLabels(t *testing.T) {
	settings := config.ProjectSettings{ProfileByLabel: map[string]string{
		"deep-review": "deep",
		"broken":      "missing",
	}}
	if got := ProfileForLabels([]string{"deep-review"}, "default", settings); got != "deep" {
		t.Errorf("profile = %q", got)
	}
	if got := ProfileForLabels([]string{"other"}, "default", settings); got != "default" {
		t.Errorf("profile = %q", got)
	}
	if got := ProfileForLabels(nil, "", settings); got != "default" {
		t.Errorf("profile = %q", got)
	}
}

func TestParseSettingsErrorsDoNotLeakValues(t *testing.T) {
	const secret = "glpat-secret-value"
	_, err := ParseSettings(json.RawMessage(`{"unknown":"`+secret+`"}`), testProfiles())
	if err == nil {
		t.Fatal("expected error")
	}
	var appErr *Error
	if !errors.As(err, &appErr) {
		t.Fatal("expected app error")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked input: %v", err)
	}
}

func TestParseSettingsRejectsUnsupportedIgnorePatterns(t *testing.T) {
	for _, input := range []string{`{"ignore":["!keep"]}`, `{"ignore":["secret/[ab].go"]}`} {
		_, err := ParseSettings(json.RawMessage(input), testProfiles())
		if err == nil {
			t.Fatalf("ParseSettings(%s) unexpectedly succeeded", input)
		}
		var appErr *Error
		if !errors.As(err, &appErr) {
			t.Fatalf("ParseSettings(%s) error = %T, want *Error", input, err)
		}
		if len(appErr.Details) != 1 || appErr.Details[0].Field != "settings.ignore[0]" {
			t.Fatalf("ParseSettings(%s) details = %+v", input, appErr.Details)
		}
	}
}
