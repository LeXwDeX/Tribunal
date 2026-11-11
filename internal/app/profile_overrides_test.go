package app

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func intPtr(v int) *int { return &v }

// normalize mirrors what config.Load does with the review profile.
func normalize(raw config.ProfileConfig) config.ProfileConfig {
	out, _ := config.NormalizeReviewProfile(raw)
	return out
}

func nodeByName(t *testing.T, nodes []ProfileNodeView, name string) ProfileNodeView {
	t.Helper()
	for _, node := range nodes {
		if node.Node == name {
			return node
		}
	}
	t.Fatalf("no node %q in %+v", name, nodes)
	return ProfileNodeView{}
}

// slotKeys are the seven configurable items by API node name.
var slotNodes = []struct{ node, key, path string }{
	{"default", "", "default"},
	{"explore", config.NodeExplore, "explore"},
	{"lanes.code_review", config.NodeCodeReview, "lanes.code_review"},
	{"lanes.review", config.NodeReview, "lanes.review"},
	{"lanes.reasoner", config.NodeReasoner, "lanes.reasoner"},
	{"lanes.validate", config.NodeValidate, "lanes.validate"},
	{"challenge", config.NodeChallenge, "challenge"},
}

// overrideBody builds {"<path>": {"model": id}} for a slot path.
func overrideBody(path, id string) string {
	node := `{"model":"` + id + `"}`
	switch {
	case strings.HasPrefix(path, "lanes."):
		return `{"lanes":{"` + strings.TrimPrefix(path, "lanes.") + `":` + node + `}}`
	}
	return `{"` + path + `":` + node + `}`
}

// configuredNode returns a raw profile in which only the node itself names a
// provider ("config" source).
func configuredNode(key, model string) config.ProfileConfig {
	raw := config.ProfileConfig{Challenge: config.ChallengeConfig{}}
	switch key {
	case "":
		raw.Provider = model
	case config.NodeExplore:
		raw.Explore = &config.ExploreConfig{Provider: model}
	case config.NodeCodeReview:
		raw.Lanes = &config.LanesConfig{CodeReview: config.LaneConfig{Provider: model}}
	case config.NodeReview:
		raw.Lanes = &config.LanesConfig{Review: config.LaneConfig{Provider: model}}
	case config.NodeReasoner:
		raw.Lanes = &config.LanesConfig{Reasoner: config.LaneConfig{Provider: model}}
	case config.NodeValidate:
		raw.Lanes = &config.LanesConfig{Validate: config.LaneConfig{Provider: model}}
	case config.NodeChallenge:
		raw.Challenge.Provider = model
	}
	return raw
}

// derivedOnly returns a raw profile with no profile provider and a source for
// the derived fallback that is not the node itself: lanes.code_review, or
// challenge for the code-review node.
func derivedOnly(key string) (config.ProfileConfig, string) {
	if key == config.NodeCodeReview {
		return config.ProfileConfig{Challenge: config.ChallengeConfig{Provider: "from-challenge"}}, "from-challenge"
	}
	return config.ProfileConfig{Lanes: &config.LanesConfig{CodeReview: config.LaneConfig{Provider: "from-code-review"}}}, "from-code-review"
}

// Resolution order for the seven configurable items: node override, profile
// default override, configured node provider, configured profile provider,
// derived fallback (#291, #334). Every case also proves the value the review
// executes with, not only the label.
func TestResolveProfileSourcesTable(t *testing.T) {
	for _, slot := range slotNodes {
		t.Run(slot.node, func(t *testing.T) {
			check := func(t *testing.T, raw config.ProfileConfig, body, wantModel, wantSource string) {
				t.Helper()
				resolved := ResolveProfile(raw, normalize(raw), body)
				node := nodeByName(t, resolved.Nodes, slot.node)
				if node.Model != catalogID(wantModel) || node.Source != wantSource {
					t.Fatalf("%s = %s/%s, want %s/%s", slot.node, node.Model, node.Source, catalogID(wantModel), wantSource)
				}
				if executed := nodeModel(resolved.Effective, slot.key); executed != wantModel {
					t.Fatalf("%s executes with %q, view says %q", slot.node, executed, wantModel)
				}
			}
			t.Run("override", func(t *testing.T) {
				raw := configuredNode(slot.key, "cfg-node")
				raw.Provider = "cfg-default"
				body := overrideBody(slot.path, "user:node")
				check(t, raw, body, "user:node", "override")
			})
			t.Run("override_default", func(t *testing.T) {
				if slot.key == "" {
					t.Skip("the profile default has no default override above it")
				}
				raw := configuredNode(slot.key, "cfg-node")
				raw.Provider = "cfg-default"
				check(t, raw, `{"default":{"model":"user:def"}}`, "user:def", "override_default")
			})
			t.Run("config", func(t *testing.T) {
				raw := configuredNode(slot.key, "cfg-node")
				raw.Provider = "cfg-default"
				if slot.key == "" {
					raw = config.ProfileConfig{Provider: "cfg-node"}
				}
				check(t, raw, `{}`, "cfg-node", "config")
			})
			t.Run("config_default", func(t *testing.T) {
				if slot.key == "" {
					t.Skip("the profile default is the configured profile provider itself")
				}
				check(t, config.ProfileConfig{Provider: "cfg-default"}, `{}`, "cfg-default", "config_default")
			})
			t.Run("derived", func(t *testing.T) {
				raw, from := derivedOnly(slot.key)
				check(t, raw, `{}`, from, "derived")
			})
		})
	}
}

// A legacy propose entry still configures the code-review lane. The lane runs
// from the propose entry normalization derives, so it is reported as configured
// and the other lanes derive from it.
func TestResolveProfileCodeReviewViaLegacyPropose(t *testing.T) {
	raw := config.ProfileConfig{Propose: []config.ProposeConfig{{Block: "code-review", Provider: "legacy"}}}
	resolved := ResolveProfile(raw, normalize(raw), `{}`)
	codeReview := nodeByName(t, resolved.Nodes, "lanes.code_review")
	if codeReview.Model != "config:legacy" || codeReview.Source != "config" {
		t.Fatalf("code_review = %+v", codeReview)
	}
	if review := nodeByName(t, resolved.Nodes, "lanes.review"); review.Model != "config:legacy" || review.Source != "derived" {
		t.Fatalf("review = %+v", review)
	}
	// An override of the lane wins over the legacy entry and changes Propose[0].
	body := overrideBody("lanes.code_review", "user:new")
	resolved = ResolveProfile(raw, normalize(raw), body)
	if got := resolved.Effective.Propose[0].Provider; got != "user:new" {
		t.Fatalf("Propose[0] = %q, want user:new", got)
	}
}

// Regression for the #327 defect: overrides applied to the already normalized
// profile left Propose[0] (the code-review lane) and every filled lane on the
// configured provider, and the override was neither validated nor hashed.
func TestOverridesChangeTheExecutedProviders(t *testing.T) {
	raw := config.ProfileConfig{Provider: "fake"}
	normalized := normalize(raw)
	before := normalized
	body := `{"default":{"model":"user:def"},"lanes":{"code_review":{"model":"user:new"}}}`
	resolved := ResolveProfile(raw, normalized, body)
	eff := resolved.Effective
	if eff.Propose[0].Provider != "user:new" || eff.Lanes.CodeReview.Provider != "user:new" {
		t.Fatalf("code-review lane runs %q, want user:new", eff.Propose[0].Provider)
	}
	for name, got := range map[string]string{
		"provider": eff.Provider, "explore": eff.Explore.Provider, "review": eff.Lanes.Review.Provider,
		"reasoner": eff.Lanes.Reasoner.Provider, "validate": eff.Lanes.Validate.Provider, "challenge": eff.Challenge.Provider,
	} {
		if got != "user:def" {
			t.Errorf("%s runs %q, want user:def", name, got)
		}
	}
	refs := review.ProfileProviderRefs(review.Profile{Name: "default", Config: eff})
	if !reflect.DeepEqual(refs, []string{"user:def", "user:new"}) {
		t.Fatalf("provider refs = %v, want the override models only", refs)
	}
	// "fake" is not allowed, so the old provider no longer passes unnoticed.
	if err := review.ValidateProfileProviders(review.Profile{Name: "default", Config: eff}, []string{"fake"}); err == nil {
		t.Fatal("override models were not validated against the allowed providers")
	}
	if err := review.ValidateProfileProviders(review.Profile{Name: "default", Config: eff}, []string{"user:def", "user:new"}); err != nil {
		t.Fatalf("override models rejected: %v", err)
	}
	// The shared profiles are not mutated.
	if !reflect.DeepEqual(normalized, before) || !reflect.DeepEqual(raw, config.ProfileConfig{Provider: "fake"}) {
		t.Fatal("ResolveProfile mutated its inputs")
	}
}

func TestResolveProfileWithoutOverridesIsTheNormalizedProfile(t *testing.T) {
	raw := config.ProfileConfig{Provider: "fake"}
	normalized := normalize(raw)
	for _, body := range []string{``, `{}`, `{"default":{}}`, `{"explore":{"params":{}}}`, `not json`} {
		if got := ResolveProfile(raw, normalized, body).Effective; !reflect.DeepEqual(got, normalized) {
			t.Fatalf("body %q changed the profile: %+v", body, got)
		}
	}
}

func TestResolveProfileParams(t *testing.T) {
	raw := config.ProfileConfig{Provider: "fake"}
	body := `{"default":{"params":{"max_tokens":4000,"temperature":0.5}},
		"explore":{"params":{"max_tokens":100,"reasoning":{"effort":"low"}}},
		"challenge":{"params":{"thinking":{"budget_tokens":2048}}}}`
	resolved := ResolveProfile(raw, normalize(raw), body)
	params := resolved.Effective.NodeParams
	explore := params[config.NodeExplore]
	if *explore.MaxTokens != 100 || *explore.Temperature != 0.5 || explore.Reasoning.Effort != "low" || explore.Source != "override" {
		t.Fatalf("explore params = %+v", explore)
	}
	review := params[config.NodeReview]
	if *review.MaxTokens != 4000 || review.Reasoning != nil || review.Source != "override_default" {
		t.Fatalf("review params = %+v", review)
	}
	challenge := params[config.NodeChallenge]
	if challenge.Thinking.BudgetTokens != 2048 || *challenge.MaxTokens != 4000 || challenge.Source != "override" {
		t.Fatalf("challenge params = %+v", challenge)
	}
	view := nodeByName(t, resolved.Nodes, "lanes.review")
	if view.ParamSource != "override_default" || view.Params == nil || *view.Params.MaxTokens != 4000 || view.Override != nil {
		t.Fatalf("review view = %+v", view)
	}
	if view := nodeByName(t, resolved.Nodes, "explore"); view.Override == nil || view.Override.Params == nil || view.ParamSource != "override" {
		t.Fatalf("explore view = %+v", view)
	}
	// A params-only override keeps the configured model.
	if view := nodeByName(t, resolved.Nodes, "explore"); view.Source != "config_default" || view.Model != "config:fake" {
		t.Fatalf("explore model = %s/%s", view.Model, view.Source)
	}
}

func TestValidateProfileOverrides(t *testing.T) {
	models := map[string]OverrideModel{
		"user:ok": {Protocol: "openai-chat", MaxTokens: 8000}, "config:cfg": {Protocol: "openai-chat", MaxTokens: 8000},
		"user:down": {Unavailable: true, Protocol: "openai-chat"}, "user:claude": {Protocol: "anthropic", MaxTokens: 8000},
		"user:deepseek": {Protocol: "openai-chat", MaxTokens: 8000, AllowReasoningEffortNone: true},
	}
	known := func(id string) (OverrideModel, bool) { m, ok := models[id]; return m, ok }
	raw := config.ProfileConfig{Provider: "user:ok"}
	normalized := normalize(raw)
	for _, tc := range []struct {
		name, body string
		fields     []string
	}{
		{"valid", `{"default":{"model":"user:ok","params":{"max_tokens":10000,"temperature":2,"reasoning":{"effort":"high"},"thinking":{"budget_tokens":4000}}},"lanes":{"review":{"model":"config:cfg"}}}`, nil},
		{"unknown model names the field", `{"lanes":{"validate":{"model":"user:gone"}}}`, []string{"overrides.lanes.validate.model"}},
		{"bad params", `{"explore":{"params":{"max_tokens":0,"temperature":3,"reasoning":{"effort":"extreme"},"thinking":{"budget_tokens":-1}}}}`,
			[]string{"overrides.explore.params", "overrides.explore.params", "overrides.explore.params", "overrides.explore.params"}},
		{"whitespace model", `{"explore":{"model":" user:ok"}}`, []string{"overrides.explore.model"}},
		{"unavailable model", `{"explore":{"model":"user:down"}}`, []string{"overrides.explore.model"}},
		{"no-op params", `{"explore":{"params":{"thinking":{"budget_tokens":0},"reasoning":{"effort":""}}}}`,
			[]string{"overrides.explore.params", "overrides.explore.params"}},
		{"anthropic minimum budget", `{"explore":{"model":"user:claude","params":{"thinking":{"budget_tokens":500}}}}`, []string{"overrides.explore.params.thinking.budget_tokens"}},
		{"openai small budget is fine", `{"explore":{"params":{"thinking":{"budget_tokens":500}}}}`, nil},
		{"max_tokens not above budget", `{"explore":{"params":{"max_tokens":2000,"thinking":{"budget_tokens":2000}}}}`, []string{"overrides.explore.params.thinking.budget_tokens"}},
		{"model max_tokens not above budget", `{"explore":{"params":{"thinking":{"budget_tokens":9000}}}}`, []string{"overrides.explore.params.thinking.budget_tokens"}},
		{"default budget reaches an anthropic node", `{"default":{"params":{"thinking":{"budget_tokens":500}}},"lanes":{"review":{"model":"user:claude"}}}`, []string{"overrides.default.params.thinking.budget_tokens"}},
		{"effort none with capability", `{"explore":{"model":"user:deepseek","params":{"reasoning":{"effort":"none"}}}}`, nil},
		{"effort none without capability", `{"explore":{"params":{"reasoning":{"effort":"none"}}}}`, []string{"overrides.explore.params.reasoning.effort"}},
		{"default effort none without capability", `{"default":{"params":{"reasoning":{"effort":"none"}}}}`, []string{"overrides.default.params.reasoning.effort"}},
		{"unknown param", `{"explore":{"params":{"context_window":10}}}`, []string{"overrides"}},
		{"unknown node", `{"plan":{"model":"user:ok"}}`, []string{"overrides"}},
		{"not an object", `[]`, []string{"overrides"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			details := ValidateProfileOverrides([]byte(tc.body), raw, normalized, known)
			var fields []string
			for _, detail := range details {
				fields = append(fields, detail.Field)
			}
			if !reflect.DeepEqual(fields, tc.fields) {
				t.Fatalf("fields = %v (%+v), want %v", fields, details, tc.fields)
			}
		})
	}
}

type fakeOverrideStore struct {
	store.ProfileOverrideStore
	body string
}

func (f fakeOverrideStore) GetProfileOverrides(context.Context, string) (*store.ProfileOverrides, error) {
	return &store.ProfileOverrides{Profile: "default", OverridesJSON: f.body, Revision: 1}, nil
}

// profileFor feeds the executor: the override changes the graph's explore
// provider and the providers the graph validates, and the hash follows.
func TestProfileForAppliesOverridesToRawProfile(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	raw := config.ProfileConfig{Provider: "fake"}
	f.service.rawProfiles = map[string]config.ProfileConfig{"default": raw}
	f.service.profiles["default"] = normalize(raw)
	f.service.overrides = fakeOverrideStore{body: `{"default":{"model":"user:def"},"lanes":{"code_review":{"model":"user:new"}}}`}
	_, cfg, err := f.service.profileFor(context.Background(), &store.Project{Profile: "default"}, "", "", nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Propose[0].Provider != "user:new" || cfg.Explore.Provider != "user:def" || cfg.Challenge.Provider != "user:def" {
		t.Fatalf("profile = %+v", cfg)
	}
	if f.service.profiles["default"].Propose[0].Provider != "fake" {
		t.Fatal("the shared profile was mutated")
	}
	profile := review.Profile{Name: "default", Config: cfg}
	if _, err := review.BuildGraph(profile, testDossier(t, reviewTestHead), review.GraphConfig{AllowedProviders: []string{"fake"}}); err == nil {
		t.Fatal("graph accepted providers outside the allowed list")
	}
	spec, err := review.BuildGraph(profile, testDossier(t, reviewTestHead), review.GraphConfig{AllowedProviders: []string{"user:def", "user:new"}})
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	for _, node := range spec.Nodes {
		if node.Kind != review.KindExplore {
			continue
		}
		var params struct{ Provider string }
		if err := json.Unmarshal(node.Params, &params); err != nil || params.Provider != "user:def" {
			t.Fatalf("explore node params = %s (%v)", node.Params, err)
		}
	}
}

// No override, no new bytes: node_params is omitted and the hash is unchanged,
// while a parameter override changes it.
func TestNodeParamsEnterTheProfileHashOnlyWhenSet(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	profile := review.Profile{Name: "default", Config: f.profiles["default"]}
	base := f.service.reviewProfileHash(profile)
	profile.Config.NodeParams = map[string]config.NodeParams{}
	if got := f.service.reviewProfileHash(profile); got != base {
		t.Fatal("an empty node_params map changed the hash")
	}
	profile.Config.NodeParams = map[string]config.NodeParams{config.NodeExplore: {MaxTokens: intPtr(10)}}
	changed := f.service.reviewProfileHash(profile)
	if changed == base {
		t.Fatal("a parameter override did not change the hash")
	}
	profile.Config.NodeParams = map[string]config.NodeParams{config.NodeExplore: {MaxTokens: intPtr(10), Source: "override"}}
	if f.service.reviewProfileHash(profile) != changed {
		t.Fatal("the audit-only source changed the hash")
	}
}

type failingOverrideStore struct{ store.ProfileOverrideStore }

func (failingOverrideStore) GetProfileOverrides(context.Context, string) (*store.ProfileOverrides, error) {
	return nil, errors.New("overrides unavailable")
}

// An unreadable override store must not make a review run without the
// overrides: a new review fails retryably before any workflow exists.
func TestNewReviewFailsRetryablyWhenOverridesCannotBeRead(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.overrides = failingOverrideStore{}
	job := f.seedReview(t, "job-overrides-unreadable")
	err := f.service.RunReview(context.Background(), job)
	if err == nil || IsPermanentJob(err) {
		t.Fatalf("RunReview error = %v, want a retryable error", err)
	}
	if _, err := f.db.GetWorkflowByJob(context.Background(), job.ID); err == nil {
		t.Fatal("a workflow was created without the overrides")
	}
}

// Recovery must surface the read error instead of cancelling the interrupted
// workflow for a profile hash change that never happened.
func TestRecoveryDoesNotCancelWhenOverridesCannotBeRead(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.overrides = fakeOverrideStore{body: `{"default":{"model":"fake"}}`}
	job := f.seedReview(t, "job-recovery-overrides-unreadable")
	id := interruptReview(t, f, job)
	f.service.overrides = failingOverrideStore{}
	_ = f.service.Recover(context.Background())
	wf, err := f.db.GetWorkflow(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status == "cancelled" {
		snapshot, _ := f.service.engine.Snapshot(context.Background(), id)
		t.Fatalf("workflow was cancelled (%s) because the overrides could not be read", snapshot.CancelReason)
	}
}

func snapshotProfile(t *testing.T, cfg config.ProfileConfig) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// Regression for the dev-server finding: resolving overrides must never write
// through the Lanes/Explore pointers the services share, or a deleted override
// keeps showing (and executing) its model until restart.
func TestResolveProfileNeverMutatesSharedProfiles(t *testing.T) {
	raw := config.ProfileConfig{
		Provider: "base",
		Explore:  &config.ExploreConfig{Provider: "cfg-explore"},
		Lanes:    &config.LanesConfig{Reasoner: config.LaneConfig{Provider: "cfg-reasoner"}},
	}
	normalized := normalize(raw)
	rawBefore, normBefore := snapshotProfile(t, raw), snapshotProfile(t, normalized)
	bodies := []string{
		`{"lanes":{"reasoner":{"model":"user:r"},"review":{"model":"user:v"},"code_review":{"model":"user:c"},"validate":{"model":"user:x"}}}`,
		`{"explore":{"model":"user:e"},"challenge":{"model":"user:h"}}`,
		`{"default":{"model":"user:d","params":{"max_tokens":9}}}`,
	}
	for i := 0; i < 3; i++ {
		for _, body := range bodies {
			ResolveProfile(raw, normalized, body)
		}
	}
	if got := snapshotProfile(t, raw); got != rawBefore {
		t.Fatalf("raw profile mutated:\n%s\nwant\n%s", got, rawBefore)
	}
	if got := snapshotProfile(t, normalized); got != normBefore {
		t.Fatalf("normalized profile mutated:\n%s\nwant\n%s", got, normBefore)
	}
	after := ResolveProfile(raw, normalized, `{}`)
	if !reflect.DeepEqual(after.Effective, normalized) {
		t.Fatalf("resolving without overrides = %+v, want the original %+v", after.Effective, normalized)
	}
	if node := nodeByName(t, after.Nodes, "lanes.reasoner"); node.Model != "config:cfg-reasoner" || node.Source != "config" {
		t.Fatalf("reasoner after reset = %+v", node)
	}
}

func TestResolveProfileConcurrentlyIsRaceFree(t *testing.T) {
	raw := config.ProfileConfig{Provider: "base"}
	normalized := normalize(raw)
	before := snapshotProfile(t, normalized)
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := `{"lanes":{"reasoner":{"model":"user:r"}},"explore":{"model":"user:e"},"challenge":{"model":"user:h"}}`
			if i%2 == 0 {
				body = `{}`
			}
			for j := 0; j < 50; j++ {
				ResolveProfile(raw, normalized, body)
			}
		}(i)
	}
	wg.Wait()
	if snapshotProfile(t, normalized) != before {
		t.Fatal("shared profile changed")
	}
}
