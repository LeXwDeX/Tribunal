package review

import (
	"strings"
	"testing"
)

// TestContentLimitConstants pins the generation-side contract limits. They are
// not publishing clips; the renderer publishes accepted values in full.
func TestContentLimitConstants(t *testing.T) {
	if MaxEvidenceRunes != 64_000 {
		t.Fatalf("MaxEvidenceRunes = %d, want 64000", MaxEvidenceRunes)
	}
	if MaxSuggestionRunes != 64_000 {
		t.Fatalf("MaxSuggestionRunes = %d, want 64000", MaxSuggestionRunes)
	}
	if MaxReasonRunes != 8_000 {
		t.Fatalf("MaxReasonRunes = %d, want 8000", MaxReasonRunes)
	}
}

// TestFindingContentLimitsAreGenerationContracts guards the #493 review: the
// generation-side limits stay finite (providers such as openai-chat ignore
// jsonschema maxLength, so Validate is the only enforcement point) while the
// renderer keeps publishing accepted values in full.
func TestFindingContentLimitsAreGenerationContracts(t *testing.T) {
	const (
		evidenceLimit   = 64_000
		suggestionLimit = 64_000
	)
	cases := []struct {
		name   string
		mutate func(*FindingOut)
		valid  bool
	}{
		{name: "evidence at limit", mutate: func(f *FindingOut) { f.Evidence = strings.Repeat("证", evidenceLimit) }, valid: true},
		{name: "evidence over limit", mutate: func(f *FindingOut) { f.Evidence = strings.Repeat("证", evidenceLimit+1) }},
		{name: "suggestion at limit", mutate: func(f *FindingOut) { f.Suggestion = strings.Repeat("议", suggestionLimit) }, valid: true},
		{name: "suggestion over limit", mutate: func(f *FindingOut) { f.Suggestion = strings.Repeat("议", suggestionLimit+1) }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			finding := validFindingOut()
			tc.mutate(&finding)
			err := finding.Validate()
			if tc.valid && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tc.valid {
				if err == nil {
					t.Fatal("Validate accepted an over-limit field")
				}
				if !strings.Contains(err.Error(), "must be at most") {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}

const reasonLimit = 8_000

func TestReasonContentLimitIsGenerationContract(t *testing.T) {
	cases := []struct {
		name  string
		value string
		valid bool
	}{
		{name: "at limit", value: strings.Repeat("因", reasonLimit), valid: true},
		{name: "over limit", value: strings.Repeat("因", reasonLimit+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decision := Decision{FindingID: "g_1", Action: "publish", Reason: tc.value}
			err := decision.Validate()
			if tc.valid && err != nil {
				t.Fatalf("Decision.Validate: %v", err)
			}
			if !tc.valid {
				if err == nil {
					t.Fatal("Decision.Validate accepted an over-limit reason")
				}
				if !strings.Contains(err.Error(), "must be at most") {
					t.Fatalf("unexpected error: %v", err)
				}
			}
		})
	}
}
