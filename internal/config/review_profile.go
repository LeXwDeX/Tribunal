package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
)

// Tribunal has exactly one review profile and one onboarding profile.
const (
	// ReviewProfile is the only review profile: explore, then code-review,
	// review, reasoner and validate in parallel, then adjudication and the
	// report.
	ReviewProfile = "default"
	// OnboardingProfile drives project onboarding; it is not a review profile.
	OnboardingProfile = "onboarding"
	// ReviewFlow is the graph of the review profile.
	ReviewFlow = "lanes"
)

// normalizeProfiles keeps only the review and onboarding profiles and maps
// the review profile onto the single flow. Removed profiles and legacy fields
// never fail startup; each one becomes a warning.
func normalizeProfiles(c *Config) []string {
	var warnings []string
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		switch name {
		case OnboardingProfile:
		case ReviewProfile:
			profile, profileWarnings := NormalizeReviewProfile(c.Profiles[name])
			c.Profiles[name] = profile
			warnings = append(warnings, profileWarnings...)
		default:
			delete(c.Profiles, name)
			warnings = append(warnings, fmt.Sprintf(
				"profiles.%s was removed: Tribunal has one review profile (%s); projects, labels, repository configuration and /review commands that name it use %s",
				name, ReviewProfile, ReviewProfile))
		}
	}
	return warnings
}

// NormalizeReviewProfile maps a review profile, including one written for an
// earlier release, onto the single review flow. Provider choices of legacy
// fields are migrated; flow choices are fixed and their legacy fields are
// ignored. The returned warnings name each migrated or ignored field.
func NormalizeReviewProfile(in ProfileConfig) (ProfileConfig, []string) {
	const field = "profiles." + ReviewProfile
	var warnings []string
	warn := func(format string, args ...any) {
		warnings = append(warnings, field+"."+fmt.Sprintf(format, args...))
	}
	lanes := LanesConfig{}
	if in.Lanes != nil {
		lanes = *in.Lanes
	}
	explore := ExploreConfig{}
	if in.Explore != nil {
		explore = *in.Explore
	}

	// Legacy provider choices are migrated when the new field is unset.
	codeReviewSeen, reviewSeen := false, false
	for i, propose := range in.Propose {
		switch {
		case propose.Block == "code-review" && !codeReviewSeen:
			codeReviewSeen = true
			if lanes.CodeReview.Provider == "" {
				lanes.CodeReview.Provider = propose.Provider
			}
			warn("propose[%d] is deprecated and was migrated to lanes.code_review.provider", i)
		case propose.Block == "review" && !reviewSeen:
			reviewSeen = true
			if lanes.Review.Provider == "" {
				lanes.Review.Provider = propose.Provider
			}
			warn("propose[%d] is deprecated and was migrated to lanes.review.provider", i)
		default:
			warn("propose[%d] (%s) was ignored: each lane runs once", i, propose.Block)
		}
	}
	if in.Reasoner != nil {
		if lanes.Reasoner.Provider == "" {
			lanes.Reasoner.Provider = in.Reasoner.Provider
		}
		warn("reasoner is deprecated and was migrated to lanes.reasoner.provider; the reasoner lane always runs, so reasoner.when is ignored")
	}
	challenge := in.Challenge.Provider
	if challenge == "cross" {
		challenge = in.Challenge.Fallback
		warn("challenge.provider cross is no longer supported; challenge.fallback is used as the challenge provider")
	} else if in.Challenge.Fallback != "" {
		warn("challenge.fallback was ignored")
	}
	if len(in.Challenge.Only) > 0 && !slices.Equal(in.Challenge.Only, []string{"P0", "P1"}) {
		warn("challenge.only was ignored: P0 and P1 findings are always challenged")
	}

	// Flow choices are fixed.
	if in.Plan.Mode != "" && in.Plan.Mode != "code" {
		warn("plan.mode was ignored: planning always runs in code mode after explore")
	}
	if in.Plan.Provider != "" {
		warn("plan.provider was ignored: planning does not call a model")
	}
	if in.Consensus != nil {
		warn("consensus was ignored")
	}
	if in.OnUncertain != "" && in.OnUncertain != "mark" {
		warn("on_uncertain was ignored: uncertain findings are marked")
	}
	if (in.Synthesize.Mode != "" && in.Synthesize.Mode != "code") || in.Synthesize.Provider != "" {
		warn("synthesize was ignored: the report is assembled by code")
	}
	if in.Budget.MaxExtends != 0 {
		warn("budget.max_extends was ignored: the report does not extend the graph")
	}
	if in.Agent != nil {
		warn("agent was ignored: it applies to the onboarding profile only")
	}

	base := strings.TrimSpace(in.Provider)
	if base == "" {
		base = lanes.CodeReview.Provider
	}
	if base == "" && challenge != "cross" {
		base = challenge
	}
	for _, lane := range []*LaneConfig{&lanes.CodeReview, &lanes.Review, &lanes.Reasoner, &lanes.Validate} {
		if strings.TrimSpace(lane.Provider) == "" {
			lane.Provider = base
		}
	}
	if strings.TrimSpace(explore.Provider) == "" {
		explore.Provider = base
	}
	if challenge == "" {
		challenge = base
	}
	budget := in.Budget
	budget.MaxExtends = 0
	out := ProfileConfig{
		Flow:        ReviewFlow,
		Provider:    base,
		Explore:     &explore,
		Lanes:       &lanes,
		Plan:        PlanConfig{Mode: "code", PackageTokens: in.Plan.PackageTokens, MaxPackages: in.Plan.MaxPackages},
		Propose:     []ProposeConfig{{Block: "code-review", Provider: lanes.CodeReview.Provider}},
		Challenge:   ChallengeConfig{Provider: challenge},
		OnUncertain: "mark",
		Synthesize:  SynthesizeConfig{Mode: "code"},
		Budget:      budget,
	}
	return out, warnings
}
