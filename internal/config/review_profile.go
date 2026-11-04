package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
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

	// reviewMaxFanOut mirrors the graph's default max_fan_out; each package
	// adds a code-review and a review lane, the MR adds reasoner and validate.
	reviewMaxFanOut = 36
	// reviewFixedNodes counts explore, plan, reasoner, validate, gather,
	// adjudicate and synthesize, plus room for two verify challenges.
	reviewFixedNodes   = 9
	defaultMaxNodes    = 30
	defaultMaxPackages = 6
	recommendedTimeout = 30 * time.Minute
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
			migrateProvider(&lanes.CodeReview, propose.Provider, fmt.Sprintf("propose[%d]", i), "lanes.code_review.provider", warn)
		case propose.Block == "review" && !reviewSeen:
			reviewSeen = true
			migrateProvider(&lanes.Review, propose.Provider, fmt.Sprintf("propose[%d]", i), "lanes.review.provider", warn)
		default:
			warn("propose[%d] (%s) was ignored: each lane runs once", i, propose.Block)
		}
	}
	if in.Reasoner != nil {
		migrateProvider(&lanes.Reasoner, in.Reasoner.Provider, "reasoner", "lanes.reasoner.provider", warn)
		warn("reasoner.when was ignored: the reasoner lane always runs")
	}
	challenge := in.Challenge.Provider
	if challenge == "cross" {
		challenge = in.Challenge.Fallback
		if challenge == "" {
			warn("challenge.provider cross is no longer supported; with no challenge.fallback the profile provider challenges")
		} else {
			warn("challenge.provider cross is no longer supported; challenge.fallback is used as the challenge provider")
		}
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
	if base == "" {
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
	if budget.Deadline > 0 && budget.Deadline < recommendedTimeout {
		warn("budget.deadline %s is below %s: explore, the four parallel lanes and the challenges may not finish in time", budget.Deadline, recommendedTimeout)
	}
	plan := PlanConfig{Mode: "code", PackageTokens: in.Plan.PackageTokens, MaxPackages: in.Plan.MaxPackages}
	limit := maxReviewPackages(budget.MaxNodes)
	if plan.MaxPackages == 0 && limit < defaultMaxPackages {
		plan.MaxPackages = limit
	}
	if plan.MaxPackages > limit {
		warn("plan.max_packages %d was lowered to %d: each package adds two lanes, so more packages exceed max_fan_out %d or budget.max_nodes", plan.MaxPackages, limit, reviewMaxFanOut)
		plan.MaxPackages = limit
	}
	out := ProfileConfig{
		Flow:        ReviewFlow,
		Provider:    base,
		Explore:     &explore,
		Lanes:       &lanes,
		Plan:        plan,
		Propose:     []ProposeConfig{{Block: "code-review", Provider: lanes.CodeReview.Provider}},
		Challenge:   ChallengeConfig{Provider: challenge},
		OnUncertain: "mark",
		Synthesize:  SynthesizeConfig{Mode: "code"},
		Budget:      budget,
	}
	return out, warnings
}

// migrateProvider moves a legacy provider choice into lane unless the new
// field is already set; the warning says which one applies.
func migrateProvider(lane *LaneConfig, provider, legacy, field string, warn func(string, ...any)) {
	if strings.TrimSpace(lane.Provider) != "" {
		warn("%s is deprecated and was ignored: %s is set", legacy, field)
		return
	}
	lane.Provider = provider
	warn("%s is deprecated and was migrated to %s", legacy, field)
}

// maxReviewPackages is the largest package count whose lanes fit both the
// fan-out bound and budget.max_nodes.
func maxReviewPackages(maxNodes int) int {
	if maxNodes <= 0 {
		maxNodes = defaultMaxNodes
	}
	limit := (reviewMaxFanOut - 2) / 2
	if byNodes := (maxNodes - reviewFixedNodes) / 2; byNodes < limit {
		limit = byNodes
	}
	if limit < 1 {
		limit = 1
	}
	return limit
}
