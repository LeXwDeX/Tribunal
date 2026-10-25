package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

// Block names used by profiles, prompt roles, and node kinds.
const (
	BlockPlan            = "plan"
	BlockCodeReview      = "code-review"
	BlockReviewStandards = "review.standards"
	BlockReviewIntent    = "review.intent"
	BlockReviewGate      = "review.gate"
	BlockReasoner        = "reasoner"
	BlockGather          = "gather"
	BlockVerify          = "verify"
	BlockAdjudicate      = "adjudicate"
	BlockSynthesize      = "synthesize"
	BlockOnboarding      = "onboarding"
)

// Node kinds are identical to block names; node IDs add package/sequence
// qualifiers.
const (
	KindPlan       = BlockPlan
	KindCodeReview = BlockCodeReview
	KindReviewStd  = BlockReviewStandards
	KindReviewInt  = BlockReviewIntent
	KindReviewGate = BlockReviewGate
	KindReasoner   = BlockReasoner
	KindGather     = BlockGather
	KindVerify     = BlockVerify
	KindAdjudicate = BlockAdjudicate
	KindSynthesize = BlockSynthesize
	KindOnboarding = BlockOnboarding
)

// Node ID prefixes from docs/design/04-dag-and-contracts.md.
const (
	NodePlan       = "plan"
	NodeGather     = "gather"
	NodeAdjudicate = "adjudicate"
	NodeSynth      = "synthesize"
	NodeSynth2     = "synthesize.2"
	NodeReasoner   = "reasoner"
)

const (
	DefaultPlanTimeout       = 5 * time.Minute
	DefaultReviewTimeout     = 10 * time.Minute
	DefaultGatherTimeout     = 2 * time.Minute
	DefaultAdjudicateTimeout = 2 * time.Minute
	DefaultSynthesizeTimeout = 10 * time.Minute
	DefaultOnboardingTimeout = 15 * time.Minute
	DefaultMaxFanOut         = 36
	DefaultDeadline          = 20 * time.Minute
	DefaultPackageTokens     = 60000
)

// ErrInvalidProfile reports a profile or graph-policy violation.
var ErrInvalidProfile = errors.New("review: invalid profile")

// ErrExtensionRejected reports an expansion that violates policy or budget.
var ErrExtensionRejected = errors.New("review: extension rejected")

// ProfileError identifies a failing profile field.
type ProfileError struct {
	Field   string
	Problem string
}

func (e *ProfileError) Error() string {
	if e.Field == "" {
		return ErrInvalidProfile.Error() + ": " + e.Problem
	}
	return ErrInvalidProfile.Error() + ": " + e.Field + ": " + e.Problem
}

func (e *ProfileError) Unwrap() error { return ErrInvalidProfile }

// ExtensionError identifies a rejected graph extension.
type ExtensionError struct {
	Field   string
	Problem string
}

func (e *ExtensionError) Error() string {
	if e.Field == "" {
		return ErrExtensionRejected.Error() + ": " + e.Problem
	}
	return ErrExtensionRejected.Error() + ": " + e.Field + ": " + e.Problem
}

func (e *ExtensionError) Unwrap() error { return ErrExtensionRejected }

// Block describes one block and its default node contract.
type Block struct {
	Name          string
	Kind          string
	Required      bool
	ReadOnlyTools bool
	Contract      string
}

var blockTable = []Block{
	{Name: BlockPlan, Kind: KindPlan, Required: true, ReadOnlyTools: true, Contract: ContractPlan},
	{Name: BlockCodeReview, Kind: KindCodeReview, Required: true, ReadOnlyTools: true, Contract: ContractCodeReview},
	{Name: BlockReviewStandards, Kind: KindReviewStd, Required: true, ReadOnlyTools: true, Contract: ContractReviewStandards},
	{Name: BlockReviewIntent, Kind: KindReviewInt, Required: true, ReadOnlyTools: true, Contract: ContractReviewIntent},
	{Name: BlockReviewGate, Kind: KindReviewGate, Required: true, ReadOnlyTools: false, Contract: ContractReviewGate},
	{Name: BlockReasoner, Kind: KindReasoner, Required: false, ReadOnlyTools: true, Contract: ContractReasoner},
	{Name: BlockGather, Kind: KindGather, Required: true, Contract: ""},
	{Name: BlockVerify, Kind: KindVerify, Required: false, ReadOnlyTools: true, Contract: ContractVerify},
	{Name: BlockAdjudicate, Kind: KindAdjudicate, Required: true, Contract: ""},
	{Name: BlockSynthesize, Kind: KindSynthesize, Required: true, ReadOnlyTools: true, Contract: ContractSynthesize},
	{Name: BlockOnboarding, Kind: KindOnboarding, Required: true, ReadOnlyTools: true, Contract: ContractOnboarding},
}

// Blocks returns the documented block table in stable order.
func Blocks() []Block {
	out := make([]Block, len(blockTable))
	copy(out, blockTable)
	return out
}

// BlockByName returns one block definition.
func BlockByName(name string) (Block, bool) {
	for _, block := range blockTable {
		if block.Name == name {
			return block, true
		}
	}
	return Block{}, false
}

// Profile names a config.ProfileConfig. The name is used in metadata and
// errors; Config holds the validated YAML model.
type Profile struct {
	Name   string
	Config config.ProfileConfig
}

// GraphConfig is the graph-policy context supplied by the app layer.
type GraphConfig struct {
	AllowedProviders []string
	MaxFanOut        int
}

// BuildGraph creates the initial graph for a profile. plan and gather (and
// synthesize in agent mode) expand later; see the expanders below.
func BuildGraph(profile Profile, d *Dossier, cfg GraphConfig) (dag.GraphSpec, error) {
	normalized, err := ValidateProfile(profile)
	if err != nil {
		return dag.GraphSpec{}, err
	}
	if err := ValidateProfileProviders(normalized, cfg.AllowedProviders); err != nil {
		return dag.GraphSpec{}, err
	}
	if cfg.MaxFanOut == 0 {
		cfg.MaxFanOut = DefaultMaxFanOut
	}
	if err := validateFanOut(normalized, cfg); err != nil {
		return dag.GraphSpec{}, err
	}
	budget := graphBudget(normalized.Config.Budget)
	nodes := make([]dag.NodeSpec, 0, 6)

	if isOnboardingProfile(normalized.Config) {
		params, err := encodeParams(onboardingParams{Provider: normalized.Config.Agent.Provider})
		if err != nil {
			return dag.GraphSpec{}, err
		}
		nodes = append(nodes, dag.NodeSpec{
			ID:       KindOnboarding,
			Kind:     KindOnboarding,
			Required: true,
			Timeout:  DefaultOnboardingTimeout,
			Params:   params,
		})
		spec := dag.GraphSpec{Nodes: nodes, Budget: budget}
		if err := dag.ValidateGraph(spec); err != nil {
			return dag.GraphSpec{}, err
		}
		return spec, nil
	}
	if d == nil {
		return dag.GraphSpec{}, fmt.Errorf("%w: dossier is required", ErrInvalidProfile)
	}

	challengeOnly, err := parseChallengeOnly(normalized.Config.Challenge.Only)
	if err != nil {
		return dag.GraphSpec{}, err
	}
	planParamsValue := planParams{
		Mode:          normalized.Config.Plan.Mode,
		Provider:      normalized.Config.Plan.Provider,
		PackageTokens: normalized.Config.Plan.PackageTokens,
		MaxPackages:   normalized.Config.Plan.MaxPackages,
		Propose:       proposeRefs(normalized.Config.Propose),
		ChallengeOnly: challengeOnly,
		AllowReasoner: normalized.Config.Reasoner != nil && normalized.Config.Reasoner.When != "never",
		ReasonerWhen:  reasonerWhen(normalized.Config),
	}
	planRaw, err := encodeParams(planParamsValue)
	if err != nil {
		return dag.GraphSpec{}, err
	}
	nodes = append(nodes, dag.NodeSpec{
		ID:        NodePlan,
		Kind:      KindPlan,
		Required:  true,
		Expands:   true,
		Timeout:   DefaultPlanTimeout,
		Params:    planRaw,
		TokenFree: normalized.Config.Plan.Mode == "code",
	})

	if normalized.Config.Reasoner != nil && normalized.Config.Reasoner.When != "never" {
		reasonerRaw, err := encodeParams(reasonerParams{Provider: normalized.Config.Reasoner.Provider, When: normalized.Config.Reasoner.When})
		if err != nil {
			return dag.GraphSpec{}, err
		}
		reasoner := dag.NodeSpec{
			ID:        NodeReasoner,
			Kind:      KindReasoner,
			DependsOn: []string{NodePlan},
			Required:  false,
			Timeout:   DefaultReviewTimeout,
			Params:    reasonerRaw,
		}
		if normalized.Config.Reasoner.When == "auto" {
			// dag treats an optional dependency that skips as deps_skipped.
			// The executor performs the auto check, so the graph condition
			// stays true while still exercising the condition grammar.
			reasoner.Condition = `plan.output.run_reasoner == true || plan.output.run_reasoner == false`
		}
		nodes = append(nodes, reasoner)
	}

	gatherDeps := []string{NodePlan}
	if nodes[len(nodes)-1].ID == NodeReasoner {
		gatherDeps = append(gatherDeps, NodeReasoner)
	}
	gatherRaw, err := encodeParams(gatherParams{
		ChallengeOnly:     challengeOnly,
		MinVotes:          minVotes(normalized.Config),
		SameModelWeight:   sameModelWeight(normalized.Config),
		ChallengeProvider: normalized.Config.Challenge.Provider,
		FallbackProvider:  normalized.Config.Challenge.Fallback,
		AllowedProviders:  gatherAllowedProviders(normalized.Config),
	})
	if err != nil {
		return dag.GraphSpec{}, err
	}
	nodes = append(nodes, dag.NodeSpec{
		ID:        NodeGather,
		Kind:      KindGather,
		DependsOn: gatherDeps,
		Required:  true,
		Expands:   true,
		Timeout:   DefaultGatherTimeout,
		Params:    gatherRaw,
		TokenFree: true,
	})

	adjudicateRaw, err := encodeParams(adjudicateParams{
		MinVotes:      minVotes(normalized.Config),
		ChallengeOnly: challengeOnly,
		OnUncertain:   OnUncertain(normalized.Config.OnUncertain),
	})
	if err != nil {
		return dag.GraphSpec{}, err
	}
	nodes = append(nodes, dag.NodeSpec{
		ID:        NodeAdjudicate,
		Kind:      KindAdjudicate,
		DependsOn: []string{NodeGather},
		Required:  true,
		Timeout:   DefaultAdjudicateTimeout,
		Params:    adjudicateRaw,
		TokenFree: true,
	})

	synthesizeRaw, err := encodeParams(synthesizeParams{
		Mode:        normalized.Config.Synthesize.Mode,
		Provider:    normalized.Config.Synthesize.Provider,
		MaxExtends:  normalized.Config.Budget.MaxExtends,
		OnUncertain: normalized.Config.OnUncertain,
	})
	if err != nil {
		return dag.GraphSpec{}, err
	}
	nodes = append(nodes, dag.NodeSpec{
		ID:        NodeSynth,
		Kind:      KindSynthesize,
		DependsOn: []string{NodeAdjudicate},
		Required:  true,
		Expands:   normalized.Config.Synthesize.Mode == "agent" && normalized.Config.Budget.MaxExtends > 0,
		Timeout:   DefaultSynthesizeTimeout,
		Params:    synthesizeRaw,
		TokenFree: true,
	})

	spec := dag.GraphSpec{Nodes: nodes, Budget: budget}
	if err := dag.ValidateGraph(spec); err != nil {
		return dag.GraphSpec{}, err
	}
	return spec, nil
}

// ValidateProfile applies defaults and rejects unsupported blocks, unknown
// providers, impossible budgets, and oversized fan-out.
func ValidateProfile(profile Profile) (Profile, error) {
	out := Profile{Name: strings.TrimSpace(profile.Name), Config: profile.Config}
	if out.Name == "" {
		out.Name = "custom"
	}
	cfg := &out.Config

	if cfg.Plan.Mode == "" {
		cfg.Plan.Mode = "code"
	}
	if cfg.Plan.Mode != "code" && cfg.Plan.Mode != "agent" {
		return Profile{}, &ProfileError{Field: "plan.mode", Problem: "must be code or agent"}
	}
	if cfg.Plan.PackageTokens < 0 {
		return Profile{}, &ProfileError{Field: "plan.package_tokens", Problem: "must not be negative"}
	}
	if cfg.Plan.PackageTokens == 0 {
		cfg.Plan.PackageTokens = DefaultPackageTokens
	}
	if cfg.Plan.MaxPackages == 0 {
		cfg.Plan.MaxPackages = DefaultMaxPackages
	}
	if cfg.Plan.MaxPackages < 0 || cfg.Plan.MaxPackages > HardMaxPackages {
		return Profile{}, &ProfileError{Field: "plan.max_packages", Problem: fmt.Sprintf("must be between 1 and %d", HardMaxPackages)}
	}

	onboarding := isOnboardingProfile(*cfg)
	if !onboarding {
		if len(cfg.Propose) == 0 {
			return Profile{}, &ProfileError{Field: "propose", Problem: "must contain at least one block"}
		}
		allowedBlocks := map[string]bool{BlockCodeReview: true, "review": true}
		for i, propose := range cfg.Propose {
			if !allowedBlocks[propose.Block] {
				return Profile{}, &ProfileError{Field: fmt.Sprintf("propose[%d].block", i), Problem: `must be code-review or review`}
			}
			if strings.TrimSpace(propose.Provider) == "" {
				return Profile{}, &ProfileError{Field: fmt.Sprintf("propose[%d].provider", i), Problem: "must not be empty"}
			}
		}
		if cfg.Plan.Mode == "agent" && strings.TrimSpace(cfg.Plan.Provider) == "" {
			return Profile{}, &ProfileError{Field: "plan.provider", Problem: "is required for agent planning"}
		}
		if cfg.Challenge.Provider == "" {
			return Profile{}, &ProfileError{Field: "challenge.provider", Problem: "must not be empty"}
		}
		if cfg.Challenge.Provider == "cross" && cfg.Challenge.Fallback == "" {
			return Profile{}, &ProfileError{Field: "challenge.fallback", Problem: "is required when challenge.provider is cross"}
		}
	}

	if cfg.OnUncertain == "" {
		cfg.OnUncertain = string(OnUncertainMark)
	}
	switch OnUncertain(cfg.OnUncertain) {
	case OnUncertainDrop, OnUncertainMark, OnUncertainJudge:
	default:
		return Profile{}, &ProfileError{Field: "on_uncertain", Problem: "must be drop, mark, or judge"}
	}
	if cfg.Synthesize.Mode == "" {
		cfg.Synthesize.Mode = "code"
	}
	if cfg.Synthesize.Mode != "code" && cfg.Synthesize.Mode != "agent" {
		return Profile{}, &ProfileError{Field: "synthesize.mode", Problem: "must be code or agent"}
	}
	if cfg.OnUncertain == string(OnUncertainJudge) && cfg.Synthesize.Mode != "agent" {
		return Profile{}, &ProfileError{Field: "on_uncertain", Problem: "judge requires synthesize.mode agent"}
	}
	if cfg.Synthesize.Mode == "agent" && strings.TrimSpace(cfg.Synthesize.Provider) == "" {
		return Profile{}, &ProfileError{Field: "synthesize.provider", Problem: "is required for agent synthesis"}
	}
	if cfg.Reasoner != nil {
		if cfg.Reasoner.When == "" {
			cfg.Reasoner.When = "auto"
		}
		switch cfg.Reasoner.When {
		case "auto", "always", "never":
		default:
			return Profile{}, &ProfileError{Field: "reasoner.when", Problem: "must be auto, always, or never"}
		}
		if cfg.Reasoner.When != "never" && strings.TrimSpace(cfg.Reasoner.Provider) == "" {
			return Profile{}, &ProfileError{Field: "reasoner.provider", Problem: "is required when the reasoner runs"}
		}
	}

	if cfg.Budget.MaxNodes == 0 {
		cfg.Budget.MaxNodes = 30
	}
	if cfg.Budget.MaxNodes < 0 || cfg.Budget.MaxNodes > 100 {
		return Profile{}, &ProfileError{Field: "budget.max_nodes", Problem: "must be between 1 and 100"}
	}
	if cfg.Budget.MaxExtends < 0 || cfg.Budget.MaxExtends > HardMaxExtends {
		return Profile{}, &ProfileError{Field: "budget.max_extends", Problem: fmt.Sprintf("must be between 0 and %d", HardMaxExtends)}
	}
	if cfg.Budget.Deadline < 0 {
		return Profile{}, &ProfileError{Field: "budget.deadline", Problem: "must not be negative"}
	}
	if cfg.Budget.Deadline == 0 {
		cfg.Budget.Deadline = DefaultDeadline
	}
	if cfg.Budget.MaxTokens < 0 {
		return Profile{}, &ProfileError{Field: "budget.max_tokens", Problem: "must not be negative"}
	}
	if onboarding {
		if cfg.Agent == nil || strings.TrimSpace(cfg.Agent.Provider) == "" {
			return Profile{}, &ProfileError{Field: "agent.provider", Problem: "is required for onboarding"}
		}
	}
	return out, nil
}

// ValidateProfileProviders checks provider references against the registry
// names known by the app. A nil allowed list skips the check.
func ValidateProfileProviders(profile Profile, allowed []string) error {
	if allowed == nil {
		return nil
	}
	known := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		if name != "" {
			known[name] = struct{}{}
		}
	}
	check := func(field, provider string) error {
		if provider == "" || provider == "cross" {
			return nil
		}
		if _, ok := known[provider]; !ok {
			return &ProfileError{Field: field, Problem: fmt.Sprintf("provider %q is not configured", provider)}
		}
		return nil
	}
	cfg := profile.Config
	if err := check("plan.provider", cfg.Plan.Provider); err != nil {
		return err
	}
	for i, propose := range cfg.Propose {
		if err := check(fmt.Sprintf("propose[%d].provider", i), propose.Provider); err != nil {
			return err
		}
	}
	if cfg.Reasoner != nil {
		if err := check("reasoner.provider", cfg.Reasoner.Provider); err != nil {
			return err
		}
	}
	if cfg.Challenge.Provider != "cross" {
		if err := check("challenge.provider", cfg.Challenge.Provider); err != nil {
			return err
		}
	}
	if err := check("challenge.fallback", cfg.Challenge.Fallback); err != nil {
		return err
	}
	if err := check("synthesize.provider", cfg.Synthesize.Provider); err != nil {
		return err
	}
	if cfg.Agent != nil {
		if err := check("agent.provider", cfg.Agent.Provider); err != nil {
			return err
		}
	}
	return nil
}

func isOnboardingProfile(cfg config.ProfileConfig) bool {
	if cfg.Agent == nil || len(cfg.Propose) != 0 {
		return false
	}
	return cfg.Plan.Mode == "" || cfg.Plan.Mode == "code"
}

func graphBudget(in config.BudgetConfig) dag.Budget {
	budget := dag.Budget{
		MaxNodes:   in.MaxNodes,
		MaxExtends: in.MaxExtends,
		Deadline:   in.Deadline,
		MaxTokens:  in.MaxTokens,
	}
	if budget.MaxNodes == 0 {
		budget.MaxNodes = 30
	}
	if budget.Deadline == 0 {
		budget.Deadline = DefaultDeadline
	}
	return budget
}

func parseChallengeOnly(values []string) ([]Severity, error) {
	if len(values) == 0 {
		return []Severity{SeverityP0, SeverityP1}, nil
	}
	out := make([]Severity, 0, len(values))
	seen := make(map[Severity]struct{}, len(values))
	for i, value := range values {
		severity := Severity(value)
		if !severity.Valid() {
			return nil, &ProfileError{Field: fmt.Sprintf("challenge.only[%d]", i), Problem: "must be P0, P1, P2, or info"}
		}
		if _, ok := seen[severity]; ok {
			return nil, &ProfileError{Field: fmt.Sprintf("challenge.only[%d]", i), Problem: "duplicate severity"}
		}
		seen[severity] = struct{}{}
		out = append(out, severity)
	}
	return out, nil
}

func reasonerWhen(cfg config.ProfileConfig) string {
	if cfg.Reasoner == nil {
		return "never"
	}
	return cfg.Reasoner.When
}

func minVotes(cfg config.ProfileConfig) float64 {
	if cfg.Consensus == nil {
		return 0
	}
	return float64(cfg.Consensus.MinVotes)
}

func sameModelWeight(cfg config.ProfileConfig) float64 {
	if cfg.Consensus == nil {
		return 0
	}
	return cfg.Consensus.SameModelWeight
}

func proposeRefs(values []config.ProposeConfig) []proposeRef {
	out := make([]proposeRef, len(values))
	for i, value := range values {
		out[i] = proposeRef{Block: value.Block, Provider: value.Provider}
	}
	return out
}

func gatherAllowedProviders(cfg config.ProfileConfig) []string {
	out := allowedProposeProviders(cfg)
	seen := make(map[string]struct{}, len(out)+3)
	for _, provider := range out {
		seen[provider] = struct{}{}
	}
	add := func(provider string) {
		if provider == "" || provider == "cross" {
			return
		}
		if _, ok := seen[provider]; ok {
			return
		}
		seen[provider] = struct{}{}
		out = append(out, provider)
	}
	add(cfg.Challenge.Provider)
	add(cfg.Challenge.Fallback)
	if cfg.Reasoner != nil {
		add(cfg.Reasoner.Provider)
	}
	add(cfg.Synthesize.Provider)
	add(cfg.Plan.Provider)
	return out
}

func allowedProposeProviders(cfg config.ProfileConfig) []string {
	seen := make(map[string]struct{}, len(cfg.Propose))
	out := make([]string, 0, len(cfg.Propose))
	for _, propose := range cfg.Propose {
		if propose.Provider == "" {
			continue
		}
		if _, ok := seen[propose.Provider]; ok {
			continue
		}
		seen[propose.Provider] = struct{}{}
		out = append(out, propose.Provider)
	}
	return out
}

func encodeParams(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("review: encode node params: %w", err)
	}
	return json.RawMessage(raw), nil
}

func decodeParams[T any](raw json.RawMessage) (T, error) {
	var zero T
	if len(bytes.TrimSpace(raw)) == 0 {
		return zero, errors.New("review: node params are empty")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var out T
	if err := dec.Decode(&out); err != nil {
		return zero, fmt.Errorf("review: decode node params: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return zero, errors.New("review: trailing node params")
		}
		return zero, fmt.Errorf("review: trailing node params: %w", err)
	}
	return out, nil
}

// planParams is the plan node's persisted profile context.
type planParams struct {
	Mode          string       `json:"mode"`
	Provider      string       `json:"provider,omitempty"`
	PackageTokens int          `json:"package_tokens"`
	MaxPackages   int          `json:"max_packages"`
	Propose       []proposeRef `json:"propose"`
	ChallengeOnly []Severity   `json:"challenge_only"`
	AllowReasoner bool         `json:"allow_reasoner"`
	ReasonerWhen  string       `json:"reasoner_when,omitempty"`
}

type proposeRef struct {
	Block    string `json:"block"`
	Provider string `json:"provider"`
}

type reviewNodeParams struct {
	Package  string   `json:"package"`
	Channel  string   `json:"channel,omitempty"` // empty for code-review
	Files    []string `json:"files"`
	Focus    string   `json:"focus"`
	Provider string   `json:"provider"`
}

type channelRef struct {
	Node string `json:"node"`
	Kind string `json:"kind"`
}

type gateParams struct {
	Package  string       `json:"package"`
	Files    []string     `json:"files"`
	Channels []channelRef `json:"channels"`
}

type reasonerParams struct {
	Provider string `json:"provider"`
	When     string `json:"when"`
}

type gatherParams struct {
	ChallengeOnly     []Severity `json:"challenge_only"`
	MinVotes          float64    `json:"min_votes"`
	SameModelWeight   float64    `json:"same_model_weight"`
	ChallengeProvider string     `json:"challenge_provider"`
	FallbackProvider  string     `json:"fallback_provider,omitempty"`
	AllowedProviders  []string   `json:"allowed_providers,omitempty"`
}

type adjudicateParams struct {
	MinVotes      float64     `json:"min_votes"`
	ChallengeOnly []Severity  `json:"challenge_only"`
	OnUncertain   OnUncertain `json:"on_uncertain"`
}

type verifyParams struct {
	GroupID   string     `json:"group_id"`
	Finding   FindingOut `json:"finding"`
	Provider  string     `json:"provider"`
	SameModel bool       `json:"same_model,omitempty"`
	Sequence  int        `json:"sequence"`
	Extension int        `json:"extension,omitempty"`
}

type synthesizeParams struct {
	Mode            string `json:"mode"`
	Provider        string `json:"provider,omitempty"`
	MaxExtends      int    `json:"max_extends"`
	OnUncertain     string `json:"on_uncertain"`
	PreviousNode    string `json:"previous_node,omitempty"`
	PreviousAttempt int    `json:"previous_attempt,omitempty"`
}

type onboardingParams struct {
	Provider string `json:"provider"`
}

// ExpanderSet dispatches expansions by node kind. The engine takes one
// Expander; pass an ExpanderSet as that single value.
type ExpanderSet map[string]dag.Expander

// Expand implements dag.Expander.
func (s ExpanderSet) Expand(ctx context.Context, wf dag.WorkflowView, from dag.NodeSpec, out json.RawMessage) (dag.Extension, error) {
	if exp, ok := s[from.ID]; ok && exp != nil {
		return exp.Expand(ctx, wf, from, out)
	}
	if exp, ok := s[from.Kind]; ok && exp != nil {
		return exp.Expand(ctx, wf, from, out)
	}
	return dag.Extension{}, &ExtensionError{Field: "from", Problem: fmt.Sprintf("no expander for node %q (kind %q)", from.ID, from.Kind)}
}

// BuildExpanders returns the plan, gather, and synthesize expanders keyed by
// node kind for the given profile.
func BuildExpanders(profile Profile, d *Dossier, cfg GraphConfig) (map[string]dag.Expander, error) {
	normalized, err := ValidateProfile(profile)
	if err != nil {
		return nil, err
	}
	if d == nil {
		return nil, fmt.Errorf("%w: dossier is required", ErrInvalidProfile)
	}
	if cfg.MaxFanOut == 0 {
		cfg.MaxFanOut = DefaultMaxFanOut
	}
	if cfg.MaxFanOut < 0 {
		return nil, fmt.Errorf("%w: max_fan_out must not be negative", ErrInvalidProfile)
	}
	if err := ValidateProfileProviders(normalized, cfg.AllowedProviders); err != nil {
		return nil, err
	}
	if err := validateFanOut(normalized, cfg); err != nil {
		return nil, err
	}
	return map[string]dag.Expander{
		KindPlan:       PlanExpander{Profile: normalized, Dossier: d, Config: cfg},
		KindGather:     GatherExpander{Profile: normalized, Dossier: d, Config: cfg},
		KindSynthesize: SynthesizeExpander{Profile: normalized, Dossier: d, Config: cfg},
	}, nil
}

func validateFanOut(profile Profile, cfg GraphConfig) error {
	if cfg.MaxFanOut <= 0 {
		return nil
	}
	perPackage := 0
	hasReview := false
	for _, propose := range profile.Config.Propose {
		switch propose.Block {
		case BlockCodeReview:
			perPackage++
		case "review":
			perPackage += 2
			hasReview = true
		}
	}
	if hasReview {
		perPackage++
	}
	upper := profile.Config.Plan.MaxPackages * perPackage
	if perPackage == 0 {
		return nil
	}
	if upper > cfg.MaxFanOut {
		return &ProfileError{
			Field:   "plan.max_packages",
			Problem: fmt.Sprintf("worst-case fan-out %d exceeds max_fan_out %d", upper, cfg.MaxFanOut),
		}
	}
	return nil
}

// ExtensionPolicy bounds one expansion.
type ExtensionPolicy struct {
	Source       string
	MaxNodes     int
	MaxExtends   int
	MaxVerify    int
	MaxReview    int
	AllowedKinds []string
}

// ValidateExtension checks the extension against graph policy and budget. It
// is a deterministic gate; the engine repeats structural validation.
func ValidateExtension(ext dag.Extension, wf dag.WorkflowView, policy ExtensionPolicy) error {
	source := ext.Source
	if source == "" {
		source = dag.ExtensionSourceFanOut
	}
	if source != dag.ExtensionSourceFanOut && source != dag.ExtensionSourceExtend {
		return &ExtensionError{Field: "source", Problem: fmt.Sprintf("unknown source %q", source)}
	}
	if policy.Source != "" && source != policy.Source {
		return &ExtensionError{Field: "source", Problem: fmt.Sprintf("expected %q, got %q", policy.Source, source)}
	}
	if source == dag.ExtensionSourceExtend && policy.MaxExtends > 0 && wf.ExtendsUsed >= policy.MaxExtends {
		return &ExtensionError{Field: "extends", Problem: fmt.Sprintf("max_extends %d reached", policy.MaxExtends)}
	}
	if policy.MaxNodes > 0 && len(wf.Spec.Nodes)+len(ext.Nodes) > policy.MaxNodes {
		return &ExtensionError{
			Field:   "nodes",
			Problem: fmt.Sprintf("graph would have %d nodes, max_nodes is %d", len(wf.Spec.Nodes)+len(ext.Nodes), policy.MaxNodes),
		}
	}
	allowed := make(map[string]struct{}, len(policy.AllowedKinds))
	for _, kind := range policy.AllowedKinds {
		allowed[kind] = struct{}{}
	}
	verifyCount, reviewCount := 0, 0
	seen := make(map[string]struct{}, len(wf.Spec.Nodes)+len(ext.Nodes))
	for _, node := range wf.Spec.Nodes {
		seen[node.ID] = struct{}{}
	}
	for i, node := range ext.Nodes {
		if _, exists := seen[node.ID]; exists {
			return &ExtensionError{Field: fmt.Sprintf("nodes[%d].id", i), Problem: fmt.Sprintf("node %q already exists", node.ID)}
		}
		seen[node.ID] = struct{}{}
		if len(allowed) > 0 {
			if _, ok := allowed[node.Kind]; !ok {
				return &ExtensionError{Field: fmt.Sprintf("nodes[%d].kind", i), Problem: fmt.Sprintf("kind %q is not allowed", node.Kind)}
			}
		}
		switch node.Kind {
		case KindVerify:
			verifyCount++
		case KindCodeReview:
			reviewCount++
		}
	}
	if policy.MaxVerify > 0 && verifyCount > policy.MaxVerify {
		return &ExtensionError{Field: "verify", Problem: fmt.Sprintf("would add %d verify nodes, limit is %d", verifyCount, policy.MaxVerify)}
	}
	if policy.MaxReview > 0 && reviewCount > policy.MaxReview {
		return &ExtensionError{Field: "review", Problem: fmt.Sprintf("would add %d review nodes, limit is %d", reviewCount, policy.MaxReview)}
	}
	for i, patch := range ext.DepPatches {
		target, ok := wf.Nodes[patch.Node]
		if !ok {
			return &ExtensionError{Field: fmt.Sprintf("dep_patches[%d].node", i), Problem: fmt.Sprintf("unknown node %q", patch.Node)}
		}
		if target.Status != dag.NodePending {
			return &ExtensionError{Field: fmt.Sprintf("dep_patches[%d].node", i), Problem: fmt.Sprintf("node %q is %s, patches require pending", patch.Node, target.Status)}
		}
	}

	candidate := wf.Spec
	candidate.Nodes = append(append([]dag.NodeSpec(nil), wf.Spec.Nodes...), ext.Nodes...)
	for i := range candidate.Nodes {
		for _, patch := range ext.DepPatches {
			if candidate.Nodes[i].ID == patch.Node {
				candidate.Nodes[i].DependsOn = append(candidate.Nodes[i].DependsOn, patch.Add...)
			}
		}
	}
	if err := dag.ValidateGraph(candidate); err != nil {
		return &ExtensionError{Field: "graph", Problem: err.Error()}
	}
	return nil
}

var unsafeIDChars = regexp.MustCompile(`[^a-z0-9_-]+`)

func providerSuffix(provider string) string {
	suffix := unsafeIDChars.ReplaceAllString(strings.ToLower(provider), "-")
	suffix = strings.Trim(suffix, "-")
	if len(suffix) > 32 {
		suffix = suffix[:32]
	}
	if suffix == "" {
		suffix = "provider"
	}
	return suffix
}

// PlanExpander turns PlanOut into review nodes and patches gather.
type PlanExpander struct {
	Profile Profile
	Dossier *Dossier
	Config  GraphConfig
}

// Expand implements dag.Expander.
func (p PlanExpander) Expand(ctx context.Context, wf dag.WorkflowView, from dag.NodeSpec, out json.RawMessage) (dag.Extension, error) {
	if from.ID != NodePlan || from.Kind != KindPlan {
		return dag.Extension{}, &ExtensionError{Field: "from", Problem: "plan expander requires the plan node"}
	}
	params, err := decodeParams[planParams](from.Params)
	if err != nil {
		return dag.Extension{}, err
	}
	plan, err := DecodeStrict[*PlanOut](out)
	if err != nil {
		return dag.Extension{}, fmt.Errorf("%w: plan output: %v", ErrExtensionRejected, err)
	}
	policy := PlanPolicy{
		AllowedBlocks:      planAllowedBlocks(params.Propose),
		AllowedProviders:   planAllowedProviders(params.Propose),
		ChangedFiles:       dossierPaths(p.Dossier),
		MaxPackages:        params.MaxPackages,
		MaxFilesPerPackage: DefaultMaxFiles,
		AllowReasoner:      params.AllowReasoner,
	}
	if err := ValidatePlan(*plan, policy); err != nil {
		return dag.Extension{}, fmt.Errorf("%w: plan policy: %v", ErrExtensionRejected, err)
	}
	nodes := make([]dag.NodeSpec, 0)
	proposerIDs := make([]string, 0)
	for _, pkg := range plan.Packages {
		for _, block := range pkg.Blocks {
			switch block {
			case BlockCodeReview:
				refs := filterPropose(params.Propose, BlockCodeReview)
				for i, ref := range refs {
					node, err := codeReviewNode(pkg, ref, i, len(refs), from.ID)
					if err != nil {
						return dag.Extension{}, err
					}
					nodes = append(nodes, node)
					proposerIDs = append(proposerIDs, node.ID)
				}
			case "review":
				refs := filterPropose(params.Propose, "review")
				channels := make([]channelRef, 0, len(refs)*2)
				for i, ref := range refs {
					std, intent, err := reviewChannelNodes(pkg, ref, i, len(refs), from.ID)
					if err != nil {
						return dag.Extension{}, err
					}
					nodes = append(nodes, std, intent)
					proposerIDs = append(proposerIDs, std.ID, intent.ID)
					channels = append(channels, channelRef{Node: std.ID, Kind: KindReviewStd}, channelRef{Node: intent.ID, Kind: KindReviewInt})
				}
				if len(channels) > 0 {
					gate, err := reviewGateNode(pkg, channels)
					if err != nil {
						return dag.Extension{}, err
					}
					nodes = append(nodes, gate)
					proposerIDs = append(proposerIDs, gate.ID)
				}
			}
		}
	}
	ext := dag.Extension{
		Nodes:      nodes,
		DepPatches: []dag.DepPatch{{Node: NodeGather, Add: proposerIDs}},
		Source:     dag.ExtensionSourceFanOut,
		FromNode:   from.ID,
	}
	policyExtension := ExtensionPolicy{
		Source:       dag.ExtensionSourceFanOut,
		MaxNodes:     wf.Spec.Budget.MaxNodes,
		MaxExtends:   wf.Spec.Budget.MaxExtends,
		AllowedKinds: []string{KindCodeReview, KindReviewStd, KindReviewInt, KindReviewGate},
	}
	if err := ValidateExtension(ext, wf, policyExtension); err != nil {
		return dag.Extension{}, err
	}
	if p.Config.MaxFanOut > 0 && len(proposerIDs) > p.Config.MaxFanOut {
		return dag.Extension{}, &ExtensionError{Field: "plan", Problem: fmt.Sprintf("fan-out %d exceeds max_fan_out %d", len(proposerIDs), p.Config.MaxFanOut)}
	}
	return ext, nil
}

func planAllowedBlocks(refs []proposeRef) []string {
	seen := make(map[string]struct{}, len(refs))
	out := make([]string, 0, 2)
	for _, ref := range refs {
		if ref.Block == BlockCodeReview {
			if _, ok := seen[BlockCodeReview]; !ok {
				seen[BlockCodeReview] = struct{}{}
				out = append(out, BlockCodeReview)
			}
		}
		if ref.Block == "review" {
			if _, ok := seen["review"]; !ok {
				seen["review"] = struct{}{}
				out = append(out, "review")
			}
		}
	}
	return out
}

func planAllowedProviders(refs []proposeRef) []string {
	seen := make(map[string]struct{}, len(refs))
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		if _, ok := seen[ref.Provider]; ok {
			continue
		}
		seen[ref.Provider] = struct{}{}
		out = append(out, ref.Provider)
	}
	return out
}

func filterPropose(refs []proposeRef, block string) []proposeRef {
	out := make([]proposeRef, 0, len(refs))
	for _, ref := range refs {
		if ref.Block == block {
			out = append(out, ref)
		}
	}
	return out
}

func dossierPaths(d *Dossier) []string {
	if d == nil {
		return nil
	}
	out := make([]string, 0, len(d.Files))
	for _, file := range d.Files {
		out = append(out, file.Path)
	}
	return out
}

func codeReviewNode(pkg Package, ref proposeRef, index, total int, dep string) (dag.NodeSpec, error) {
	provider := ref.Provider
	if pkg.Provider != "" {
		provider = pkg.Provider
	}
	params, err := encodeParams(reviewNodeParams{
		Package:  pkg.ID,
		Files:    append([]string(nil), pkg.Files...),
		Focus:    pkg.Focus,
		Provider: provider,
	})
	if err != nil {
		return dag.NodeSpec{}, err
	}
	id := "cr." + pkg.ID
	if total > 1 {
		id += "." + providerSuffix(provider)
		if index > 0 {
			id = fmt.Sprintf("%s.%d", id, index)
		}
	}
	return dag.NodeSpec{
		ID:        id,
		Kind:      KindCodeReview,
		DependsOn: []string{dep},
		Required:  true,
		Timeout:   DefaultReviewTimeout,
		Params:    params,
	}, nil
}

func reviewChannelNodes(pkg Package, ref proposeRef, index, total int, dep string) (dag.NodeSpec, dag.NodeSpec, error) {
	provider := ref.Provider
	if pkg.Provider != "" {
		provider = pkg.Provider
	}
	base := "rv.std." + pkg.ID
	intentBase := "rv.intent." + pkg.ID
	if total > 1 {
		suffix := providerSuffix(provider)
		base += "." + suffix
		intentBase += "." + suffix
		if index > 0 {
			base = fmt.Sprintf("%s.%d", base, index)
			intentBase = fmt.Sprintf("%s.%d", intentBase, index)
		}
	}
	stdParams, err := encodeParams(reviewNodeParams{
		Package:  pkg.ID,
		Channel:  KindReviewStd,
		Files:    append([]string(nil), pkg.Files...),
		Focus:    pkg.Focus,
		Provider: provider,
	})
	if err != nil {
		return dag.NodeSpec{}, dag.NodeSpec{}, err
	}
	intentParams, err := encodeParams(reviewNodeParams{
		Package:  pkg.ID,
		Channel:  KindReviewInt,
		Files:    append([]string(nil), pkg.Files...),
		Focus:    pkg.Focus,
		Provider: provider,
	})
	if err != nil {
		return dag.NodeSpec{}, dag.NodeSpec{}, err
	}
	std := dag.NodeSpec{
		ID:        base,
		Kind:      KindReviewStd,
		DependsOn: []string{dep},
		Required:  true,
		Timeout:   DefaultReviewTimeout,
		Params:    stdParams,
	}
	intent := dag.NodeSpec{
		ID:        intentBase,
		Kind:      KindReviewInt,
		DependsOn: []string{dep},
		Required:  true,
		Timeout:   DefaultReviewTimeout,
		Params:    intentParams,
	}
	return std, intent, nil
}

func reviewGateNode(pkg Package, channels []channelRef) (dag.NodeSpec, error) {
	params, err := encodeParams(gateParams{
		Package:  pkg.ID,
		Files:    append([]string(nil), pkg.Files...),
		Channels: channels,
	})
	if err != nil {
		return dag.NodeSpec{}, err
	}
	deps := make([]string, 0, len(channels))
	for _, channel := range channels {
		deps = append(deps, channel.Node)
	}
	return dag.NodeSpec{
		ID:        "rv.gate." + pkg.ID,
		Kind:      KindReviewGate,
		DependsOn: deps,
		Required:  true,
		Timeout:   DefaultReviewTimeout,
		Params:    params,
	}, nil
}

// GatherExpander turns gather targets into verify nodes and patches
// adjudicate.
type GatherExpander struct {
	Profile Profile
	Dossier *Dossier
	Config  GraphConfig
}

// Expand implements dag.Expander.
func (g GatherExpander) Expand(ctx context.Context, wf dag.WorkflowView, from dag.NodeSpec, out json.RawMessage) (dag.Extension, error) {
	if from.ID != NodeGather || from.Kind != KindGather {
		return dag.Extension{}, &ExtensionError{Field: "from", Problem: "gather expander requires the gather node"}
	}
	gathered, err := decodeGatherOut(out)
	if err != nil {
		return dag.Extension{}, fmt.Errorf("%w: gather output: %v", ErrExtensionRejected, err)
	}
	byID := make(map[string]Group, len(gathered.Groups))
	for _, group := range gathered.Groups {
		byID[group.ID] = group
	}
	for _, target := range gathered.ToChallenge {
		if _, ok := byID[target.GroupID]; !ok {
			return dag.Extension{}, &ExtensionError{Field: "to_challenge", Problem: fmt.Sprintf("unknown group %q", target.GroupID)}
		}
	}
	targets, _ := limitChallengeTargets(byID, gathered.ToChallenge, challengeLimit(wf))
	if len(targets) == 0 {
		return dag.Extension{Source: dag.ExtensionSourceFanOut, FromNode: from.ID}, nil
	}
	nodes := make([]dag.NodeSpec, 0, len(targets))
	verifyIDs := make([]string, 0, len(targets))
	for i, target := range targets {
		group, ok := byID[target.GroupID]
		if !ok {
			return dag.Extension{}, &ExtensionError{Field: "to_challenge", Problem: fmt.Sprintf("unknown group %q", target.GroupID)}
		}
		sequence := target.Sequence
		if sequence <= 0 {
			sequence = i + 1
		}
		params, err := encodeParams(verifyParams{
			GroupID:   target.GroupID,
			Finding:   findingOutFromGroup(group),
			Provider:  target.Provider,
			SameModel: target.SameModel,
			Sequence:  sequence,
		})
		if err != nil {
			return dag.Extension{}, err
		}
		id := fmt.Sprintf("verify.%d", sequence)
		node := dag.NodeSpec{
			ID:        id,
			Kind:      KindVerify,
			DependsOn: []string{NodeGather},
			Required:  false,
			Timeout:   DefaultReviewTimeout,
			Params:    params,
		}
		nodes = append(nodes, node)
		verifyIDs = append(verifyIDs, id)
	}
	ext := dag.Extension{
		Nodes:      nodes,
		DepPatches: []dag.DepPatch{{Node: NodeAdjudicate, Add: verifyIDs}},
		Source:     dag.ExtensionSourceFanOut,
		FromNode:   from.ID,
	}
	if err := ValidateExtension(ext, wf, ExtensionPolicy{
		Source:       dag.ExtensionSourceFanOut,
		MaxNodes:     wf.Spec.Budget.MaxNodes,
		MaxExtends:   wf.Spec.Budget.MaxExtends,
		AllowedKinds: []string{KindVerify},
	}); err != nil {
		return dag.Extension{}, err
	}
	return ext, nil
}

// SynthesizeExpander turns extend_requests into verification/review nodes and
// synthesize.2. It never permits a second extension.
type SynthesizeExpander struct {
	Profile Profile
	Dossier *Dossier
	Config  GraphConfig
}

// Expand implements dag.Expander.
func (s SynthesizeExpander) Expand(ctx context.Context, wf dag.WorkflowView, from dag.NodeSpec, out json.RawMessage) (dag.Extension, error) {
	if from.Kind != KindSynthesize {
		return dag.Extension{}, &ExtensionError{Field: "from", Problem: "synthesize expander requires a synthesize node"}
	}
	if from.ID == NodeSynth2 {
		return dag.Extension{}, &ExtensionError{Field: "from", Problem: "synthesize.2 cannot extend"}
	}
	params, err := decodeParams[synthesizeParams](from.Params)
	if err != nil {
		return dag.Extension{}, err
	}
	result, err := decodeSynthesisResult(out)
	if err != nil {
		return dag.Extension{}, fmt.Errorf("%w: synthesize output: %v", ErrExtensionRejected, err)
	}
	requests := result.Output.ExtendRequests
	if len(requests) == 0 || result.Fallback {
		return dag.Extension{Source: dag.ExtensionSourceFanOut, FromNode: from.ID}, nil
	}
	if limit := challengeLimit(wf); limit >= 0 && len(requests)+1 > limit {
		return dag.Extension{Source: dag.ExtensionSourceFanOut, FromNode: from.ID}, nil
	}
	if params.MaxExtends <= 0 {
		return dag.Extension{}, &ExtensionError{Field: "extend_requests", Problem: "profile does not allow extensions"}
	}
	remaining := params.MaxExtends - wf.ExtendsUsed
	if remaining <= 0 {
		return dag.Extension{}, &ExtensionError{Field: "extend_requests", Problem: "max_extends reached"}
	}
	if len(requests) > remaining {
		return dag.Extension{}, &ExtensionError{Field: "extend_requests", Problem: fmt.Sprintf("%d requests exceed remaining budget %d", len(requests), remaining)}
	}
	extension := wf.ExtendsUsed + 1
	adjudicated, err := decodeAdjudicateNodeOutput(wf, NodeAdjudicate)
	if err != nil {
		return dag.Extension{}, &ExtensionError{Field: "adjudicate", Problem: err.Error()}
	}
	groups := make(map[string]Group, len(adjudicated.Groups)+len(result.Groups))
	for _, group := range adjudicated.Groups {
		groups[group.ID] = group
	}
	for _, group := range result.Groups {
		groups[group.ID] = group
	}
	nodes := make([]dag.NodeSpec, 0, len(requests)+1)
	newIDs := make([]string, 0, len(requests))
	for i, request := range requests {
		switch request.Kind {
		case "verify":
			node, err := s.verifyExtensionNode(wf, from, params, &result, groups, request, extension, i+1)
			if err != nil {
				return dag.Extension{}, err
			}
			nodes = append(nodes, node)
			newIDs = append(newIDs, node.ID)
		case "review":
			node, err := s.reviewExtensionNode(wf, from, params, request, extension, i+1)
			if err != nil {
				return dag.Extension{}, err
			}
			nodes = append(nodes, node)
			newIDs = append(newIDs, node.ID)
		default:
			return dag.Extension{}, &ExtensionError{Field: fmt.Sprintf("extend_requests[%d].kind", i), Problem: "must be verify or review"}
		}
	}
	// synthesize.2 inherits the profile budget so ValidateSynthesis accepts
	// ExtendsUsed=1; the synthesize.2 node ID is what forbids another extend.
	synth2Params, err := encodeParams(synthesizeParams{
		Mode:            "agent",
		Provider:        params.Provider,
		MaxExtends:      params.MaxExtends,
		OnUncertain:     params.OnUncertain,
		PreviousNode:    from.ID,
		PreviousAttempt: wf.Nodes[from.ID].Attempt,
	})
	if err != nil {
		return dag.Extension{}, err
	}
	deps := append([]string{from.ID, NodeAdjudicate}, newIDs...)
	synth2 := dag.NodeSpec{
		ID:        NodeSynth2,
		Kind:      KindSynthesize,
		DependsOn: deps,
		Required:  true,
		Timeout:   DefaultSynthesizeTimeout,
		Params:    synth2Params,
		TokenFree: true,
	}
	nodes = append(nodes, synth2)
	ext := dag.Extension{
		Nodes:    nodes,
		Source:   dag.ExtensionSourceExtend,
		FromNode: from.ID,
	}
	if err := ValidateExtension(ext, wf, ExtensionPolicy{
		Source:       dag.ExtensionSourceExtend,
		MaxNodes:     wf.Spec.Budget.MaxNodes,
		MaxExtends:   params.MaxExtends,
		MaxVerify:    remaining,
		MaxReview:    remaining,
		AllowedKinds: []string{KindVerify, KindCodeReview, KindSynthesize},
	}); err != nil {
		return dag.Extension{}, err
	}
	return ext, nil
}

func (s SynthesizeExpander) verifyExtensionNode(wf dag.WorkflowView, from dag.NodeSpec, params synthesizeParams, result *SynthesisResult, groups map[string]Group, request ExtendRequest, extension, sequence int) (dag.NodeSpec, error) {
	groupID := request.FindingID
	var finding FindingOut
	switch {
	case request.NewFindingIndex != nil:
		index := *request.NewFindingIndex
		if index < 0 || index >= len(result.Output.NewFindings) {
			return dag.NodeSpec{}, &ExtensionError{Field: "new_finding_index", Problem: "does not select a new finding"}
		}
		previousNode := params.PreviousNode
		if previousNode == "" {
			previousNode = from.ID
		}
		previousAttempt := params.PreviousAttempt
		if previousAttempt == 0 {
			previousAttempt = wf.Nodes[from.ID].Attempt
		}
		groupID = StableFindingID(wf.ID, previousNode, previousAttempt, index)
		finding = result.Output.NewFindings[index]
	default:
		group, ok := groups[request.FindingID]
		if !ok {
			return dag.NodeSpec{}, &ExtensionError{Field: "finding_id", Problem: fmt.Sprintf("unknown group %q", request.FindingID)}
		}
		finding = findingOutFromGroup(group)
	}
	provider := s.challengeProvider()
	sameModel := false
	if group, ok := groups[groupID]; ok {
		for _, source := range group.Sources {
			if source.Origin.Provider == provider {
				sameModel = true
				break
			}
		}
	}
	raw, err := encodeParams(verifyParams{
		GroupID:   groupID,
		Finding:   finding,
		Provider:  provider,
		SameModel: sameModel,
		Sequence:  sequence,
		Extension: extension,
	})
	if err != nil {
		return dag.NodeSpec{}, err
	}
	return dag.NodeSpec{
		ID:        fmt.Sprintf("verify.x%d.%d", extension, sequence),
		Kind:      KindVerify,
		DependsOn: []string{NodeAdjudicate},
		Required:  false,
		Timeout:   DefaultReviewTimeout,
		Params:    raw,
	}, nil
}

func (s SynthesizeExpander) reviewExtensionNode(wf dag.WorkflowView, from dag.NodeSpec, params synthesizeParams, request ExtendRequest, extension, sequence int) (dag.NodeSpec, error) {
	files := request.Files
	if len(files) == 0 {
		files = dossierPaths(s.Dossier)
	}
	provider := s.reviewExtensionProvider()
	raw, err := encodeParams(reviewNodeParams{
		Package:  fmt.Sprintf("ext%d_%d", extension, sequence),
		Files:    files,
		Focus:    request.Focus,
		Provider: provider,
	})
	if err != nil {
		return dag.NodeSpec{}, err
	}
	return dag.NodeSpec{
		ID:        fmt.Sprintf("rv.ext.%d.%d", extension, sequence),
		Kind:      KindCodeReview,
		DependsOn: []string{NodeAdjudicate},
		Required:  false,
		Timeout:   DefaultReviewTimeout,
		Params:    raw,
	}, nil
}

func (s SynthesizeExpander) reviewExtensionProvider() string {
	for _, propose := range s.Profile.Config.Propose {
		if propose.Block == BlockCodeReview && propose.Provider != "" {
			return propose.Provider
		}
	}
	for _, propose := range s.Profile.Config.Propose {
		if propose.Provider != "" {
			return propose.Provider
		}
	}
	return s.fallbackProvider()
}

func (s SynthesizeExpander) challengeProvider() string {
	cfg := s.Profile.Config.Challenge
	if cfg.Provider == "cross" {
		if cfg.Fallback != "" {
			return cfg.Fallback
		}
		return s.fallbackProvider()
	}
	return cfg.Provider
}

func (s SynthesizeExpander) fallbackProvider() string {
	if s.Profile.Config.Challenge.Fallback != "" {
		return s.Profile.Config.Challenge.Fallback
	}
	for _, propose := range s.Profile.Config.Propose {
		if propose.Provider != "" {
			return propose.Provider
		}
	}
	return s.Profile.Config.Synthesize.Provider
}

func findingOutFromGroup(group Group) FindingOut {
	side := string(group.Side)
	if side == "" {
		side = string(SideNew)
	}
	return FindingOut{
		Path:       group.Path,
		Line:       group.Line,
		Side:       side,
		Severity:   group.Severity,
		Category:   group.Category,
		Title:      group.Title,
		Body:       group.Body,
		Suggestion: group.Suggestion,
		Evidence:   group.Evidence,
		Confidence: group.Confidence,
	}
}
