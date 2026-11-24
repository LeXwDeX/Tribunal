package app

import (
	"encoding/json"
	"testing"
)

// A stored max_background_runes override replaces the platform default without
// mutating the defaults.
func TestResolveSettingsAppliesMaxBackgroundRunes(t *testing.T) {
	defaults := testDefaults()
	defaults.MaxBackgroundRunes = 20000
	resolved, err := ResolveSettings(json.RawMessage(`{"max_background_runes":40000}`), defaults)
	if err != nil {
		t.Fatalf("ResolveSettings: %v", err)
	}
	if resolved.MaxBackgroundRunes != 40000 {
		t.Fatalf("resolved max_background_runes = %d, want 40000", resolved.MaxBackgroundRunes)
	}
	if defaults.MaxBackgroundRunes != 20000 {
		t.Fatalf("defaults mutated: %d", defaults.MaxBackgroundRunes)
	}
}

// ParseSettings rejects a non-positive budget and round-trips a positive one.
func TestParseSettingsMaxBackgroundRunes(t *testing.T) {
	if _, err := ParseSettings(json.RawMessage(`{"max_background_runes":0}`), testProfiles()); err == nil {
		t.Fatal("expected error for max_background_runes: 0")
	}
	raw, err := ParseSettings(json.RawMessage(`{"max_background_runes":40000}`), testProfiles())
	if err != nil {
		t.Fatalf("ParseSettings: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["max_background_runes"] != float64(40000) {
		t.Fatalf("max_background_runes = %v, want 40000", decoded["max_background_runes"])
	}
}
