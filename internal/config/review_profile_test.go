package config

import (
	"strings"
	"testing"
)

func TestNormalizeReviewProfileMigratesLegacyProviders(t *testing.T) {
	legacy := ProfileConfig{
		Plan: PlanConfig{Mode: "agent", Provider: "planner", MaxPackages: 4},
		Propose: []ProposeConfig{
			{Block: "review", Provider: "rv"},
			{Block: "code-review", Provider: "cr"},
			{Block: "code-review", Provider: "cr2"},
		},
		Reasoner:  &ReasonerConfig{Provider: "rs", When: "auto"},
		Challenge: ChallengeConfig{Provider: "ch", Only: []string{"P0", "P1"}},
	}
	got, warnings := NormalizeReviewProfile(legacy)
	if got.Flow != ReviewFlow || got.Provider != "cr" {
		t.Fatalf("flow/provider = %q/%q", got.Flow, got.Provider)
	}
	lanes := []string{got.Explore.Provider, got.Lanes.CodeReview.Provider, got.Lanes.Review.Provider, got.Lanes.Reasoner.Provider, got.Lanes.Validate.Provider}
	if strings.Join(lanes, ",") != "cr,cr,rv,rs,cr" {
		t.Fatalf("explore,code_review,review,reasoner,validate = %v", lanes)
	}
	if len(got.Propose) != 1 || got.Propose[0] != (ProposeConfig{Block: "code-review", Provider: "cr"}) {
		t.Fatalf("propose = %+v", got.Propose)
	}
	if got.Plan.Mode != "code" || got.Plan.Provider != "" || got.Plan.MaxPackages != 4 || got.Reasoner != nil || got.Challenge.Provider != "ch" || len(got.Challenge.Only) != 0 {
		t.Fatalf("fixed flow fields = %+v", got)
	}
	joined := strings.Join(warnings, "\n")
	for _, want := range []string{"propose[0] is deprecated", "propose[1] is deprecated", "propose[2] (code-review) was ignored", "reasoner is deprecated", "plan.mode was ignored", "plan.provider was ignored"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings missing %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "challenge.only") {
		t.Errorf("the fixed P0/P1 challenge set must not warn:\n%s", joined)
	}
}

func TestNormalizeReviewProfileExplicitFieldsWin(t *testing.T) {
	got, warnings := NormalizeReviewProfile(ProfileConfig{
		Provider: "base",
		Explore:  &ExploreConfig{Provider: "ex"},
		Lanes:    &LanesConfig{Review: LaneConfig{Provider: "rv"}},
		Propose:  []ProposeConfig{{Block: "review", Provider: "legacy"}},
	})
	lanes := []string{got.Explore.Provider, got.Lanes.CodeReview.Provider, got.Lanes.Review.Provider, got.Lanes.Reasoner.Provider, got.Lanes.Validate.Provider, got.Challenge.Provider}
	if strings.Join(lanes, ",") != "ex,base,rv,base,base,base" {
		t.Fatalf("providers = %v", lanes)
	}
	if len(warnings) != 1 {
		t.Fatalf("warnings = %q, want only the propose deprecation", warnings)
	}
}

func TestNormalizeReviewProfileCurrentShapeHasNoWarnings(t *testing.T) {
	_, warnings := NormalizeReviewProfile(ProfileConfig{
		Provider:    "primary",
		Plan:        PlanConfig{Mode: "code", PackageTokens: 60000},
		OnUncertain: "mark",
		Synthesize:  SynthesizeConfig{Mode: "code"},
	})
	if len(warnings) != 0 {
		t.Fatalf("warnings = %q", warnings)
	}
}
