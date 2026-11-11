package app

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/config"
)

// ProfileNodeOverride is the stored override of one configurable node: a model
// (catalog ID) and request parameters, both optional.
type ProfileNodeOverride struct {
	Model  string             `json:"model,omitempty"`
	Params *config.NodeParams `json:"params,omitempty"`
}

// profileOverrideDocument is the persisted override document (#291). Every
// field is optional; an empty value keeps the configuration.
type profileOverrideDocument struct {
	Default *ProfileNodeOverride `json:"default,omitempty"`
	Explore *ProfileNodeOverride `json:"explore,omitempty"`
	Lanes   struct {
		CodeReview *ProfileNodeOverride `json:"code_review,omitempty"`
		Review     *ProfileNodeOverride `json:"review,omitempty"`
		Reasoner   *ProfileNodeOverride `json:"reasoner,omitempty"`
		Validate   *ProfileNodeOverride `json:"validate,omitempty"`
	} `json:"lanes"`
	Challenge *ProfileNodeOverride `json:"challenge,omitempty"`
}

// profileSlot names one configurable item: its API node name, its NodeParams
// key ("" for the profile default) and its path in the override document.
type profileSlot struct {
	node string
	key  string
	path string
}

// profileSlots lists the seven configurable items in display order.
var profileSlots = []profileSlot{
	{"default", "", "default"},
	{"explore", config.NodeExplore, "explore"},
	{"lanes.code_review", config.NodeCodeReview, "lanes.code_review"},
	{"lanes.review", config.NodeReview, "lanes.review"},
	{"lanes.reasoner", config.NodeReasoner, "lanes.reasoner"},
	{"lanes.validate", config.NodeValidate, "lanes.validate"},
	{"challenge", config.NodeChallenge, "challenge"},
}

func (d *profileOverrideDocument) slot(key string) *ProfileNodeOverride {
	switch key {
	case "":
		return d.Default
	case config.NodeExplore:
		return d.Explore
	case config.NodeCodeReview:
		return d.Lanes.CodeReview
	case config.NodeReview:
		return d.Lanes.Review
	case config.NodeReasoner:
		return d.Lanes.Reasoner
	case config.NodeValidate:
		return d.Lanes.Validate
	case config.NodeChallenge:
		return d.Challenge
	}
	return nil
}

func (o *ProfileNodeOverride) model() string {
	if o == nil {
		return ""
	}
	return strings.TrimSpace(o.Model)
}

func (o *ProfileNodeOverride) params() config.NodeParams {
	if o == nil || o.Params == nil {
		return config.NodeParams{}
	}
	return *o.Params
}

// effective reports whether the document changes anything.
func (d *profileOverrideDocument) effective() bool {
	for _, slot := range profileSlots {
		o := d.slot(slot.key)
		if o.model() != "" || !o.params().IsZero() {
			return true
		}
	}
	return false
}

// parseProfileOverrides reads a stored document leniently: a corrupt body
// behaves like an empty one, as the store validates before saving.
func parseProfileOverrides(body string) profileOverrideDocument {
	var doc profileOverrideDocument
	_ = json.Unmarshal([]byte(body), &doc)
	return doc
}

// OverrideModel is what validation needs to know about a catalog model.
type OverrideModel struct {
	Unavailable              bool
	Protocol                 string
	MaxTokens                int
	AllowReasoningEffortNone bool
}

// anthropicMinBudget is the smallest thinking budget the Anthropic API accepts.
const anthropicMinBudget = 1024

// ValidateProfileOverrides checks an override document before it is stored:
// unknown fields, model IDs that are not trimmed, missing or unavailable in the
// catalog (lookup), parameters outside the whitelist or the model rules, and
// parameter combinations the node's effective model cannot honour. Each
// problem names the path of the offending field under "overrides". A nil
// lookup skips the catalog checks.
func ValidateProfileOverrides(body []byte, raw, normalized config.ProfileConfig, lookup func(id string) (OverrideModel, bool)) []Detail {
	var doc profileOverrideDocument
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&doc); err != nil {
		return []Detail{{Field: "overrides", Message: "overrides must be a JSON object of known fields: " + err.Error()}}
	}
	var details []Detail
	for _, slot := range profileSlots {
		o := doc.slot(slot.key)
		if o == nil {
			continue
		}
		field := "overrides." + slot.path
		if id := o.Model; id != strings.TrimSpace(id) {
			details = append(details, Detail{Field: field + ".model", Message: "model must not have leading or trailing whitespace"})
		} else if id != "" && lookup != nil {
			switch model, ok := lookup(id); {
			case !ok:
				details = append(details, Detail{Field: field + ".model", Message: "model does not exist"})
			case model.Unavailable:
				details = append(details, Detail{Field: field + ".model", Message: "model is unavailable"})
			}
		}
		if o.Params != nil {
			if err := config.ValidateNodeParams(field+".params", *o.Params); err != nil {
				var joined interface{ Unwrap() []error }
				if errors.As(err, &joined) {
					for _, item := range joined.Unwrap() {
						details = append(details, Detail{Field: field + ".params", Message: item.Error()})
					}
				} else {
					details = append(details, Detail{Field: field + ".params", Message: err.Error()})
				}
			}
		}
	}
	if len(details) > 0 || lookup == nil {
		return details
	}
	// The remaining checks depend on the model each node ends up with.
	resolved := ResolveProfile(raw, normalized, string(body))
	for _, slot := range profileSlots {
		if slot.key == "" {
			continue
		}
		params, ok := resolved.Effective.NodeParams[slot.key]
		if !ok {
			continue
		}
		field := "overrides." + slot.path
		if doc.slot(slot.key).params().IsZero() {
			field = "overrides.default"
		}
		field += ".params"
		model, modelKnown := lookup(nodeModelID(resolved.Nodes, slot.node))
		add := func(path, message string) {
			detail := Detail{Field: field + path, Message: message}
			if !slices.Contains(details, detail) {
				details = append(details, detail)
			}
		}
		if params.Reasoning != nil && params.Reasoning.Effort == "none" && modelKnown && !model.AllowReasoningEffortNone {
			add(".reasoning.effort", "none requires a model with allow_reasoning_effort_none")
		}
		// thinking.type is the openai-chat thinking switch; other protocols
		// derive thinking from an Anthropic budget or Responses effort and
		// would silently ignore the override.
		if params.Thinking != nil && params.Thinking.Type != "" && modelKnown && model.Protocol != "openai-chat" {
			add(".thinking.type", "is only supported by openai-chat models")
		}
		if params.Thinking == nil || params.Thinking.BudgetTokens <= 0 {
			continue
		}
		budget := params.Thinking.BudgetTokens
		if modelKnown && model.Protocol == "anthropic" && budget < anthropicMinBudget {
			add(".thinking.budget_tokens", "must be at least 1024 for anthropic models")
		}
		maxTokens := model.MaxTokens
		if params.MaxTokens != nil {
			maxTokens = *params.MaxTokens
		}
		if (modelKnown || params.MaxTokens != nil) && maxTokens > 0 && maxTokens <= budget {
			add(".thinking.budget_tokens", "must be less than max_tokens")
		}
	}
	return details
}

func nodeModelID(nodes []ProfileNodeView, node string) string {
	for _, n := range nodes {
		if n.Node == node {
			return n.Model
		}
	}
	return ""
}

// ProfileNodeView is one configurable node: what the configuration alone
// gives, the stored override, and the effective value with its source.
type ProfileNodeView struct {
	Node string `json:"node"`
	// Model is the effective catalog model ID of the node.
	Model string `json:"model"`
	// Source is override, override_default, config, config_default or derived.
	Source string `json:"source"`
	// Configured is the model the configuration alone resolves to.
	Configured       string `json:"configured"`
	ConfiguredSource string `json:"configured_source"`
	// Override is this node's own stored override, if any.
	Override *ProfileNodeOverride `json:"override,omitempty"`
	// Params are the effective request parameter overrides: the node's own
	// over the profile default's. ParamSource is override or override_default.
	Params      *config.NodeParams `json:"params,omitempty"`
	ParamSource string             `json:"param_source,omitempty"`
}

// ResolvedProfile is a raw profile with stored overrides applied.
type ResolvedProfile struct {
	// Effective is the profile the review executes. Without overrides it is
	// the normalized profile unchanged.
	Effective config.ProfileConfig
	Nodes     []ProfileNodeView
}

// ResolveProfile merges a stored override document into the raw profile and
// normalizes it once, so every derived field (the code-review propose entry,
// lane and challenge defaults) follows the override. normalized is the
// profile as loaded and is returned unchanged when nothing is overridden. It
// never writes to raw or normalized, which services share.
func ResolveProfile(raw, normalized config.ProfileConfig, body string) ResolvedProfile {
	doc := parseProfileOverrides(body)
	base, _ := config.NormalizeReviewProfile(raw)
	merged := normalized
	if doc.effective() {
		merged, _ = config.NormalizeReviewProfile(mergeRawProfile(raw, &doc))
		if params := resolveNodeParams(&doc); len(params) > 0 {
			merged.NodeParams = params
		}
	}
	out := ResolvedProfile{Effective: merged}
	for _, slot := range profileSlots {
		out.Nodes = append(out.Nodes, nodeView(slot, raw, base, merged, &doc))
	}
	return out
}

// ApplyProfileOverrides returns the effective profile for a stored document.
func ApplyProfileOverrides(raw, normalized config.ProfileConfig, body string) config.ProfileConfig {
	return ResolveProfile(raw, normalized, body).Effective
}

// providerRef maps a catalog ID to the provider name profiles use: configured
// providers are listed as config:<name> in the catalog and as <name> in
// profiles; user models keep their user:<id> name.
func providerRef(id string) string {
	return strings.TrimPrefix(strings.TrimSpace(id), "config:")
}

func catalogID(ref string) string {
	switch {
	case ref == "":
		return ""
	case strings.HasPrefix(ref, "user:"), strings.HasPrefix(ref, "config:"):
		return ref
	}
	return "config:" + ref
}

// mergeRawProfile applies the models of the document to a copy of the raw
// profile in the documented order: the node's override, then the default
// override, then whatever the configuration says.
func mergeRawProfile(raw config.ProfileConfig, doc *profileOverrideDocument) config.ProfileConfig {
	out := raw
	lanes := config.LanesConfig{}
	if raw.Lanes != nil {
		lanes = *raw.Lanes
	}
	explore := config.ExploreConfig{}
	if raw.Explore != nil {
		explore = *raw.Explore
	}
	defaultModel := providerRef(doc.Default.model())
	pick := func(slot *ProfileNodeOverride) string {
		if model := providerRef(slot.model()); model != "" {
			return model
		}
		return defaultModel
	}
	if defaultModel != "" {
		out.Provider = defaultModel
	}
	if model := pick(doc.Explore); model != "" {
		explore.Provider = model
	}
	if model := pick(doc.Lanes.CodeReview); model != "" {
		lanes.CodeReview.Provider = model
	}
	if model := pick(doc.Lanes.Review); model != "" {
		lanes.Review.Provider = model
	}
	if model := pick(doc.Lanes.Reasoner); model != "" {
		lanes.Reasoner.Provider = model
	}
	if model := pick(doc.Lanes.Validate); model != "" {
		lanes.Validate.Provider = model
	}
	if model := pick(doc.Challenge); model != "" {
		out.Challenge.Provider = model
		out.Challenge.Fallback = ""
	}
	out.Lanes = &lanes
	out.Explore = &explore
	return out
}

// resolveNodeParams returns the effective parameter overrides per node key.
func resolveNodeParams(doc *profileOverrideDocument) map[string]config.NodeParams {
	defaults := doc.Default.params()
	out := make(map[string]config.NodeParams)
	for _, slot := range profileSlots {
		if slot.key == "" {
			continue
		}
		own := doc.slot(slot.key).params()
		merged := own.Overlay(defaults)
		if merged.IsZero() {
			continue
		}
		merged.Source = "override_default"
		if !own.IsZero() {
			merged.Source = "override"
		}
		out[slot.key] = merged
	}
	return out
}

// nodeModel reads the provider a normalized profile executes the node with.
// The code-review lane runs from the propose entry the normalization derives.
func nodeModel(cfg config.ProfileConfig, key string) string {
	switch key {
	case "":
		return cfg.Provider
	case config.NodeExplore:
		if cfg.Explore != nil {
			return cfg.Explore.Provider
		}
	case config.NodeCodeReview:
		if len(cfg.Propose) > 0 {
			return cfg.Propose[0].Provider
		}
	case config.NodeReview:
		if cfg.Lanes != nil {
			return cfg.Lanes.Review.Provider
		}
	case config.NodeReasoner:
		if cfg.Lanes != nil {
			return cfg.Lanes.Reasoner.Provider
		}
	case config.NodeValidate:
		if cfg.Lanes != nil {
			return cfg.Lanes.Validate.Provider
		}
	case config.NodeChallenge:
		return cfg.Challenge.Provider
	}
	return ""
}

// configuredSource classifies where the configuration alone gets the node's
// model: its own provider (including the legacy propose and reasoner fields
// that normalization migrates), the profile provider, or a derived fallback.
func configuredSource(raw config.ProfileConfig, key string) string {
	if strings.TrimSpace(raw.Provider) == "" && key == "" {
		return "derived"
	}
	own := ""
	switch key {
	case "":
		own = raw.Provider
	case config.NodeExplore:
		if raw.Explore != nil {
			own = raw.Explore.Provider
		}
	case config.NodeCodeReview:
		if raw.Lanes != nil {
			own = raw.Lanes.CodeReview.Provider
		}
		own = firstNonEmpty(own, legacyPropose(raw, "code-review"))
	case config.NodeReview:
		if raw.Lanes != nil {
			own = raw.Lanes.Review.Provider
		}
		own = firstNonEmpty(own, legacyPropose(raw, "review"))
	case config.NodeReasoner:
		if raw.Lanes != nil {
			own = raw.Lanes.Reasoner.Provider
		}
		if own == "" && raw.Reasoner != nil {
			own = raw.Reasoner.Provider
		}
	case config.NodeValidate:
		if raw.Lanes != nil {
			own = raw.Lanes.Validate.Provider
		}
	case config.NodeChallenge:
		own = raw.Challenge.Provider
		if own == "cross" {
			own = raw.Challenge.Fallback
		}
	}
	switch {
	case strings.TrimSpace(own) != "":
		return "config"
	case strings.TrimSpace(raw.Provider) != "":
		return "config_default"
	}
	return "derived"
}

func legacyPropose(raw config.ProfileConfig, block string) string {
	for _, propose := range raw.Propose {
		if propose.Block == block {
			return propose.Provider
		}
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func nodeView(slot profileSlot, raw, base, merged config.ProfileConfig, doc *profileOverrideDocument) ProfileNodeView {
	own := doc.slot(slot.key)
	view := ProfileNodeView{
		Node:             slot.node,
		Model:            nodeModel(merged, slot.key),
		Configured:       nodeModel(base, slot.key),
		ConfiguredSource: configuredSource(raw, slot.key),
	}
	view.Model = catalogID(view.Model)
	view.Configured = catalogID(view.Configured)
	switch {
	case own.model() != "":
		view.Source = "override"
	case doc.Default.model() != "" && slot.key != "":
		view.Source = "override_default"
	default:
		view.Source = view.ConfiguredSource
	}
	if own != nil && (own.model() != "" || !own.params().IsZero()) {
		copy := *own
		view.Override = &copy
	}
	if slot.key != "" {
		if params, ok := merged.NodeParams[slot.key]; ok {
			view.ParamSource = params.Source
			view.Params = &params
		}
	} else if p := doc.Default.params(); !p.IsZero() {
		p.Source = "override"
		view.Params, view.ParamSource = &p, "override"
	}
	return view
}
