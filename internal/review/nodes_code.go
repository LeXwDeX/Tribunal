package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

// GatherOut is the code node output for gather.
type GatherOut struct {
	Groups      []Group            `json:"groups"`
	Dropped     []Finding          `json:"dropped,omitempty"`
	ToChallenge []ChallengeTarget  `json:"to_challenge"`
	Warnings    []string           `json:"warnings,omitempty"`
	Unresolved  []UnresolvedSignal `json:"unresolved,omitempty"`
}

// Validate checks the gather contract invariants used by the expander.
func (g GatherOut) Validate() error {
	if g.Groups == nil {
		return invalid("groups", "is required")
	}
	if g.ToChallenge == nil {
		return invalid("to_challenge", "is required")
	}
	if err := validateUnresolved(g.Unresolved); err != nil {
		return wrapField("unresolved", err)
	}
	byID := make(map[string]Group, len(g.Groups))
	for i, group := range g.Groups {
		if err := validateGroup(group); err != nil {
			return wrapField(fmt.Sprintf("groups[%d]", i), err)
		}
		if _, ok := byID[group.ID]; ok {
			return invalid(fmt.Sprintf("groups[%d].id", i), "duplicate group id")
		}
		byID[group.ID] = group
	}
	seenSeq := make(map[int]struct{}, len(g.ToChallenge))
	for i, target := range g.ToChallenge {
		if _, ok := byID[target.GroupID]; !ok {
			return invalid(fmt.Sprintf("to_challenge[%d].group_id", i), "does not map to a group")
		}
		if target.Sequence <= 0 {
			return invalid(fmt.Sprintf("to_challenge[%d].sequence", i), "must be positive")
		}
		if _, ok := seenSeq[target.Sequence]; ok {
			return invalid(fmt.Sprintf("to_challenge[%d].sequence", i), "duplicate sequence")
		}
		seenSeq[target.Sequence] = struct{}{}
		if strings.TrimSpace(target.Provider) == "" {
			return invalid(fmt.Sprintf("to_challenge[%d].provider", i), "must not be empty")
		}
	}
	return nil
}

// AdjudicateOut is the code node output for adjudicate.
type AdjudicateOut struct {
	Groups     []Group            `json:"groups"`
	Warnings   []string           `json:"warnings,omitempty"`
	Unresolved []UnresolvedSignal `json:"unresolved,omitempty"`
}

// Validate checks the adjudicate contract invariants.
func (a AdjudicateOut) Validate() error {
	if a.Groups == nil {
		return invalid("groups", "is required")
	}
	if err := validateUnresolved(a.Unresolved); err != nil {
		return wrapField("unresolved", err)
	}
	seen := make(map[string]struct{}, len(a.Groups))
	for i, group := range a.Groups {
		if err := validateGroup(group); err != nil {
			return wrapField(fmt.Sprintf("groups[%d]", i), err)
		}
		if _, ok := seen[group.ID]; ok {
			return invalid(fmt.Sprintf("groups[%d].id", i), "duplicate group id")
		}
		seen[group.ID] = struct{}{}
		if group.Decision == nil {
			return invalid(fmt.Sprintf("groups[%d].decision", i), "rule decision is required")
		}
	}
	return nil
}

// VerifyResult is one verify node output envelope.
type VerifyResult struct {
	GroupID   string    `json:"group_id"`
	Provider  string    `json:"provider"`
	Model     string    `json:"model,omitempty"`
	SameModel bool      `json:"same_model,omitempty"`
	Result    VerifyOut `json:"result"`
}

// Validate checks the verify envelope and the embedded contract.
func (v VerifyResult) Validate() error {
	if strings.TrimSpace(v.GroupID) == "" {
		return invalid("group_id", "must not be empty")
	}
	if strings.TrimSpace(v.Provider) == "" {
		return invalid("provider", "must not be empty")
	}
	return v.Result.Validate()
}

// SynthesisResult is the synthesize node output. Output preserves the model
// contract; Groups is the code-owned final state used by rendering/publishing.
type SynthesisResult struct {
	Verdict    string             `json:"verdict"`
	Summary    string             `json:"summary"`
	Groups     []Group            `json:"groups"`
	Warnings   []string           `json:"warnings,omitempty"`
	Unresolved []UnresolvedSignal `json:"unresolved,omitempty"`
	Output     SynthesizeOut      `json:"output"`
	Fallback   bool               `json:"fallback,omitempty"`
}

// Validate checks the final synthesis envelope.
func (s SynthesisResult) Validate() error {
	switch s.Verdict {
	case VerdictPass, VerdictBlocking, VerdictIncomplete:
	default:
		return invalid("verdict", "must be PASS, BLOCKING, or INCOMPLETE")
	}
	if err := checkText("summary", s.Summary, MaxSummaryRunes, true); err != nil {
		return err
	}
	if s.Groups == nil {
		return invalid("groups", "is required")
	}
	for i, group := range s.Groups {
		if err := validateGroup(group); err != nil {
			return wrapField(fmt.Sprintf("groups[%d]", i), err)
		}
	}
	if err := validateUnresolved(s.Unresolved); err != nil {
		return wrapField("unresolved", err)
	}
	if s.Verdict == VerdictIncomplete {
		if len(s.Unresolved) == 0 {
			return invalid("unresolved", "is required when verdict is INCOMPLETE")
		}
	} else if len(s.Unresolved) != 0 {
		return invalid("unresolved", "must be empty unless verdict is INCOMPLETE")
	}
	if err := s.Output.Validate(); err != nil {
		return wrapField("output", err)
	}
	if s.Output.Verdict != s.Verdict && s.Verdict != VerdictIncomplete {
		return invalid("verdict", "must match output.verdict")
	}
	return nil
}

func validateGroup(group Group) error {
	if strings.TrimSpace(group.ID) == "" {
		return invalid("id", "must not be empty")
	}
	if !group.Severity.Valid() {
		return invalid("severity", "must be P0, P1, P2, or info")
	}
	if err := checkRepoPath("path", group.Path, false); err != nil {
		return err
	}
	if group.Line < 0 {
		return invalid("line", "must be zero or positive")
	}
	if err := checkText("title", group.Title, MaxTitleRunes, true); err != nil {
		return err
	}
	if err := checkText("body", group.Body, MaxBodyRunes, true); err != nil {
		return err
	}
	if group.Side != "" && group.Side != SideNew && group.Side != SideOld {
		return invalid("side", "must be new or old")
	}
	if group.Challenge != nil {
		switch group.Challenge.Status {
		case ChallengePending, ChallengeConfirmed, ChallengeRefuted, ChallengeUncertain, ChallengeFailed, ChallengeSkipped:
		default:
			return invalid("challenge.status", "unknown challenge status")
		}
	}
	if group.Decision != nil && group.Decision.Action == "" {
		return invalid("decision.action", "must not be empty")
	}
	return nil
}

func decodeStrictJSON[T any](raw []byte, validate func(*T) error) (T, error) {
	var zero T
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return zero, errors.New("review: contract must be a JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var out T
	if err := dec.Decode(&out); err != nil {
		return zero, fmt.Errorf("review: decode contract: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return zero, errors.New("review: trailing JSON value")
		}
		return zero, fmt.Errorf("review: decode trailing JSON: %w", err)
	}
	if validate != nil {
		if err := validate(&out); err != nil {
			return zero, err
		}
	}
	return out, nil
}

func decodeGatherOut(raw []byte) (GatherOut, error) {
	out, err := decodeStrictJSON(raw, func(out *GatherOut) error { return out.Validate() })
	if err != nil {
		return GatherOut{}, err
	}
	if out.Groups == nil {
		out.Groups = []Group{}
	}
	if out.ToChallenge == nil {
		out.ToChallenge = []ChallengeTarget{}
	}
	return out, nil
}

func decodeAdjudicateOut(raw []byte) (AdjudicateOut, error) {
	out, err := decodeStrictJSON(raw, func(out *AdjudicateOut) error { return out.Validate() })
	if err != nil {
		return AdjudicateOut{}, err
	}
	if out.Groups == nil {
		out.Groups = []Group{}
	}
	return out, nil
}

func decodeSynthesisResult(raw []byte) (SynthesisResult, error) {
	return decodeStrictJSON(raw, func(out *SynthesisResult) error { return out.Validate() })
}

func decodeVerifyResult(raw []byte) (VerifyResult, error) {
	return decodeStrictJSON(raw, func(out *VerifyResult) error { return out.Validate() })
}

func decodeAdjudicateNodeOutput(wf dag.WorkflowView, nodeID string) (AdjudicateOut, error) {
	node, ok := wf.Nodes[nodeID]
	if !ok {
		return AdjudicateOut{}, fmt.Errorf("review: node %q is missing", nodeID)
	}
	if node.Status != dag.NodeCompleted {
		return AdjudicateOut{}, fmt.Errorf("review: node %q is %s", nodeID, node.Status)
	}
	return decodeAdjudicateOut(node.Output)
}

// marshalNodeOutput validates JSON and converts it into a dag result.
func marshalNodeOutput(value any, usage dag.Usage) (dag.NodeOutput, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return dag.NodeOutput{}, fmt.Errorf("review: marshal node output: %w", err)
	}
	return dag.NodeOutput{Data: raw, Usage: usage}, nil
}

func (e *Executor) snapshot(ctx context.Context, workflowID string) (dag.WorkflowView, error) {
	if e.deps.Snapshot == nil {
		return dag.WorkflowView{}, errors.New("review: workflow snapshot is not configured")
	}
	return e.deps.Snapshot(ctx, workflowID)
}

// providerIdentities resolves endpoint+model identity for every provider the
// gather can vote on or choose as a challenger. A client lookup failure keeps
// the provider in the map with an empty identity, which makes voting and
// cross-model selection conservative instead of inventing independence.
func (e *Executor) providerIdentities(proposals []Proposal, params gatherParams) map[string]string {
	if e == nil || e.deps.Clients == nil {
		return nil
	}
	names := make(map[string]struct{})
	for _, proposal := range proposals {
		if proposal.Provider != "" {
			names[proposal.Provider] = struct{}{}
		}
	}
	for _, name := range params.AllowedProviders {
		if name != "" {
			names[name] = struct{}{}
		}
	}
	if params.FallbackProvider != "" {
		names[params.FallbackProvider] = struct{}{}
	}
	if len(names) == 0 {
		return nil
	}
	out := make(map[string]string, len(names))
	for name := range names {
		client, err := e.deps.Clients.Client(name)
		if err != nil {
			out[name] = ""
			continue
		}
		out[name] = client.Info().ModelIdentity()
	}
	return out
}

// executeGather collects completed proposals, applies gate rebuttals, and
// deduplicates findings. It never calls an LLM.
func (e *Executor) executeGather(ctx context.Context, in dag.NodeInput) (dag.NodeOutput, error) {
	params, err := decodeParams[gatherParams](in.Node.Params)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	wf, err := e.snapshot(ctx, in.WorkflowID)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	proposals, dropped, signals, warnings, err := e.collectProposals(wf)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	signals = append(signals, e.coverageUnresolved(wf)...)
	if scope := e.incrementalFiles(); scope != nil {
		filtered := make([]Proposal, 0, len(proposals))
		for _, proposal := range proposals {
			findings := make([]FindingOut, 0, len(proposal.Findings))
			for _, finding := range proposal.Findings {
				if _, ok := scope[finding.Path]; ok {
					findings = append(findings, finding)
				}
			}
			if len(findings) == 0 {
				continue
			}
			proposal.Findings = findings
			filtered = append(filtered, proposal)
		}
		if len(filtered) != len(proposals) {
			warnings = appendUnique(warnings, "incremental review: findings outside the incremental file set were ignored")
		}
		proposals = filtered
		allowed := make(map[Ref]struct{})
		for _, proposal := range proposals {
			for i := range proposal.Findings {
				allowed[Ref{Node: proposal.Node, Index: i}] = struct{}{}
			}
		}
		filteredDropped := dropped[:0]
		for _, drop := range dropped {
			if _, ok := allowed[drop.Ref]; ok {
				filteredDropped = append(filteredDropped, drop)
			}
		}
		dropped = filteredDropped
	}
	budgetExhausted := false
	if max := wf.Spec.Budget.MaxTokens; max > 0 {
		used := wf.Usage.InputTokens + wf.Usage.OutputTokens
		budgetExhausted = used >= max
	}
	gathered, err := Gather(GatherInput{
		WorkflowID: in.WorkflowID,
		Proposals:  proposals,
		Dropped:    dropped,
		Unresolved: signals,
	}, GatherOptions{
		ChallengeOnly:     params.ChallengeOnly,
		MinVotes:          params.MinVotes,
		SameModelWeight:   params.SameModelWeight,
		ChallengeProvider: params.ChallengeProvider,
		FallbackProvider:  params.FallbackProvider,
		AllowedProviders:  params.AllowedProviders,
		Identities:        e.providerIdentities(proposals, params),
		BudgetExhausted:   budgetExhausted,
	})
	if err != nil {
		return dag.NodeOutput{}, err
	}
	warnings = mergeWarnings(warnings, gathered.Warnings)
	out := GatherOut{
		Groups:      gathered.Groups,
		Dropped:     gathered.Dropped,
		ToChallenge: gathered.ToChallenge,
		Warnings:    warnings,
		Unresolved:  gathered.Unresolved,
	}
	applyChallengeLimit(&out, wf)
	if out.Groups == nil {
		out.Groups = []Group{}
	}
	if out.ToChallenge == nil {
		out.ToChallenge = []ChallengeTarget{}
	}
	if err := out.Validate(); err != nil {
		return dag.NodeOutput{}, err
	}
	return marshalNodeOutput(out, dag.Usage{})
}

func (e *Executor) collectProposals(wf dag.WorkflowView) ([]Proposal, []DroppedFinding, []UnresolvedSignal, []string, error) {
	order := append([]string(nil), wf.Order...)
	if len(order) == 0 {
		for id := range wf.Nodes {
			order = append(order, id)
		}
		sort.Strings(order)
	}
	proposals := make([]Proposal, 0)
	dropped := make([]DroppedFinding, 0)
	signals := make([]UnresolvedSignal, 0)
	warnings := make([]string, 0)
	for _, id := range order {
		node, ok := wf.Nodes[id]
		if !ok {
			continue
		}
		provider := nodeProvider(node)
		model := node.Model
		switch node.Spec.Kind {
		case KindCodeReview, KindReviewStd, KindReviewInt, KindReasoner, KindReviewGate:
		default:
			continue
		}
		if node.Status != dag.NodeCompleted {
			if node.Status == dag.NodeFailed || node.Status == dag.NodeAborted || node.Status == dag.NodeSkipped {
				warnings = appendUnique(warnings, fmt.Sprintf("node %s (%s) was %s and its findings are unavailable", node.ID, node.Spec.Kind, node.Status))
			}
			continue
		}
		switch node.Spec.Kind {
		case KindCodeReview:
			out, err := DecodeStrict[*CodeReviewOut](node.Output)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("review: decode %s: %w", node.ID, err)
			}
			proposals = append(proposals, ProposalFromCodeReview(node.ID, provider, model, node.Attempt, *out))
		case KindReviewStd, KindReviewInt:
			out, err := DecodeStrict[*ReviewOut](node.Output)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("review: decode %s: %w", node.ID, err)
			}
			proposal := ProposalFromReview(node.ID, node.Spec.Kind, provider, model, node.Attempt, *out)
			proposals = append(proposals, proposal)
			// Collect evidence signals before incremental filtering: a filtered
			// finding must not hide a missing-evidence declaration.
			signals = append(signals, proposalUnresolved(proposal)...)
		case KindReasoner:
			out, err := DecodeStrict[*ReasonerOut](node.Output)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("review: decode %s: %w", node.ID, err)
			}
			proposals = append(proposals, ProposalFromReasoner(node.ID, provider, model, node.Attempt, *out))
		case KindReviewGate:
			out, err := DecodeStrict[*ReviewGateOut](node.Output)
			if err != nil {
				return nil, nil, nil, nil, fmt.Errorf("review: decode %s: %w", node.ID, err)
			}
			for _, rebut := range out.Rebutted {
				dropped = append(dropped, DroppedFinding{Ref: rebut.Ref, Reason: rebut.Reason})
			}
			signals = append(signals, gateUnresolved(node.ID, *out)...)
		}
	}
	return proposals, dropped, signals, warnings, nil
}

func nodeProvider(node dag.NodeView) string {
	if node.Provider != "" {
		return node.Provider
	}
	var params struct {
		Provider string `json:"provider"`
	}
	if len(node.Spec.Params) > 0 {
		_ = json.Unmarshal(node.Spec.Params, &params)
	}
	return params.Provider
}

func (e *Executor) incrementalFiles() map[string]struct{} {
	if e.deps.Dossier == nil || e.deps.Dossier.Incremental == nil || e.deps.Dossier.Incremental.Full {
		return nil
	}
	files := e.deps.Dossier.Incremental.Files
	if files == nil {
		return nil
	}
	out := make(map[string]struct{}, len(files))
	for _, file := range files {
		out[file] = struct{}{}
	}
	return out
}

// executeAdjudicate applies the deterministic decision table. It reads verify
// node outputs from the snapshot and never consults a model transcript.
func (e *Executor) executeAdjudicate(ctx context.Context, in dag.NodeInput) (dag.NodeOutput, error) {
	params, err := decodeParams[adjudicateParams](in.Node.Params)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	gathered, err := e.gatherInput(ctx, in)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	wf, err := e.snapshot(ctx, in.WorkflowID)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	results, failed, err := collectVerifyResults(wf)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	groups := cloneGroups(gathered.Groups)
	warnings := append([]string(nil), gathered.Warnings...)
	for i := range groups {
		group := &groups[i]
		if group.Challenge == nil || group.Challenge.Status != ChallengePending {
			continue
		}
		if result, ok := results[group.ID]; ok {
			challenge := &Challenge{
				Status:            verifyStatus(result.Result.Status),
				Node:              result.Node,
				Provider:          result.Provider,
				Model:             result.Model,
				Reason:            result.Result.Reason,
				Evidence:          append([]CodeRef(nil), result.Result.Evidence...),
				SuggestedSeverity: result.Result.SuggestedSeverity,
				SameModel:         result.SameModel,
			}
			group.Challenge = challenge
			continue
		}
		if reason, ok := failed[group.ID]; ok {
			group.Challenge = &Challenge{Status: ChallengeFailed, Reason: reason}
			warnings = appendUnique(warnings, "verify failed for group "+group.ID+": "+reason)
			continue
		}
		group.Challenge = &Challenge{Status: ChallengeFailed, Reason: "verify result missing"}
		warnings = appendUnique(warnings, "verify result missing for group "+group.ID)
	}
	result, err := Adjudicate(groups, AdjudicateInput{
		MinVotes:      params.MinVotes,
		ChallengeOnly: params.ChallengeOnly,
		OnUncertain:   params.OnUncertain,
		Locator:       e.deps.Locate,
		CodeEvidence:  e.validateCodeRef,
		Warnings:      warnings,
		Unresolved:    gathered.Unresolved,
	})
	if err != nil {
		return dag.NodeOutput{}, err
	}
	out := AdjudicateOut{Groups: result.Groups, Warnings: result.Warnings, Unresolved: result.Unresolved}
	if out.Groups == nil {
		out.Groups = []Group{}
	}
	if err := out.Validate(); err != nil {
		return dag.NodeOutput{}, err
	}
	return marshalNodeOutput(out, dag.Usage{})
}

func (e *Executor) gatherInput(ctx context.Context, in dag.NodeInput) (GatherOut, error) {
	if raw, ok := in.Inputs["gather"]; ok && !isUnavailable(raw) {
		return decodeGatherOut(raw)
	}
	wf, err := e.snapshot(ctx, in.WorkflowID)
	if err != nil {
		return GatherOut{}, err
	}
	node, ok := wf.Nodes[NodeGather]
	if !ok || node.Status != dag.NodeCompleted {
		return GatherOut{}, errors.New("review: gather output is unavailable")
	}
	return decodeGatherOut(node.Output)
}

func isUnavailable(raw json.RawMessage) bool {
	var probe struct {
		Unavailable bool `json:"_unavailable"`
	}
	return json.Unmarshal(raw, &probe) == nil && probe.Unavailable
}

type verifyApplication struct {
	Node      string
	Provider  string
	Model     string
	SameModel bool
	Result    VerifyOut
}

func collectVerifyResults(wf dag.WorkflowView) (map[string]verifyApplication, map[string]string, error) {
	order := append([]string(nil), wf.Order...)
	if len(order) == 0 {
		for id := range wf.Nodes {
			order = append(order, id)
		}
		sort.Strings(order)
	}
	results := make(map[string]verifyApplication)
	failed := make(map[string]string)
	for _, id := range order {
		node, ok := wf.Nodes[id]
		if !ok || node.Spec.Kind != KindVerify {
			continue
		}
		params, _ := decodeParams[verifyParams](node.Spec.Params)
		switch node.Status {
		case dag.NodeCompleted:
			decoded, err := decodeVerifyResult(node.Output)
			if err != nil {
				return nil, nil, fmt.Errorf("review: decode %s: %w", id, err)
			}
			current, exists := results[decoded.GroupID]
			if !exists || verifyPriority(decoded.Result.Status) > verifyPriority(current.Result.Status) {
				results[decoded.GroupID] = verifyApplication{
					Node:      id,
					Provider:  decoded.Provider,
					Model:     decoded.Model,
					SameModel: decoded.SameModel,
					Result:    NormalizeVerify(decoded.Result),
				}
			}
		case dag.NodeFailed, dag.NodeAborted, dag.NodeSkipped:
			reason := node.Error
			if reason == "" {
				reason = node.Reason
			}
			if reason == "" {
				reason = string(node.Status)
			}
			failed[params.GroupID] = reason
		}
	}
	return results, failed, nil
}

func verifyPriority(status string) int {
	switch status {
	case "CONFIRMED":
		return 4
	case "REFUTED":
		return 3
	case "UNCERTAIN":
		return 2
	default:
		return 0
	}
}

func verifyStatus(status string) ChallengeStatus {
	switch status {
	case "CONFIRMED":
		return ChallengeConfirmed
	case "REFUTED":
		return ChallengeRefuted
	default:
		return ChallengeUncertain
	}
}

func mergeWarnings(groups ...[]string) []string {
	out := make([]string, 0)
	for _, values := range groups {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				out = appendUnique(out, value)
			}
		}
	}
	return out
}

// executePlanCode builds a deterministic PlanOut without an LLM.
func (e *Executor) executePlanCode(ctx context.Context, in dag.NodeInput) (dag.NodeOutput, error) {
	params, err := decodeParams[planParams](in.Node.Params)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	if e.deps.Dossier == nil {
		return dag.NodeOutput{}, errors.New("review: dossier is required for planning")
	}
	plan, err := buildCodePlan(params, e.deps.Dossier)
	if err != nil {
		return dag.NodeOutput{}, err
	}
	policy := PlanPolicy{
		AllowedBlocks:      planAllowedBlocks(params.Propose),
		AllowedProviders:   planAllowedProviders(params.Propose),
		ChangedFiles:       dossierPaths(e.deps.Dossier),
		MaxPackages:        params.MaxPackages,
		MaxFilesPerPackage: DefaultMaxFiles,
		AllowReasoner:      params.AllowReasoner,
	}
	if err := ValidatePlan(plan, policy); err != nil {
		return dag.NodeOutput{}, err
	}
	return marshalNodeOutput(plan, dag.Usage{})
}

func buildCodePlan(params planParams, dossier *Dossier) (PlanOut, error) {
	files := make([]FileChange, 0, len(dossier.Files))
	for _, file := range dossier.Files {
		if file.Generated {
			continue
		}
		files = append(files, file)
	}
	if len(files) == 0 {
		return PlanOut{}, errors.New("review: plan has no reviewable changed files")
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	maxPackages := params.MaxPackages
	if maxPackages <= 0 {
		maxPackages = DefaultMaxPackages
	}
	packageTokens := params.PackageTokens
	if packageTokens <= 0 {
		packageTokens = DefaultPackageTokens
	}
	blocks := planAllowedBlocks(params.Propose)
	if len(blocks) == 0 {
		return PlanOut{}, errors.New("review: plan has no authorized review blocks")
	}
	type bucket struct {
		files  []FileChange
		dirs   map[string]struct{}
		tokens int
	}
	buckets := make([]bucket, 0, maxPackages)
	for _, file := range files {
		tokens := estimateFileTokens(file)
		if len(buckets) == 0 {
			buckets = append(buckets, bucket{dirs: map[string]struct{}{}})
		}
		current := &buckets[len(buckets)-1]
		if current.tokens > 0 && current.tokens+tokens > packageTokens && len(buckets) < maxPackages {
			buckets = append(buckets, bucket{dirs: map[string]struct{}{}})
			current = &buckets[len(buckets)-1]
		}
		current.files = append(current.files, file)
		current.dirs[path.Dir(file.Path)] = struct{}{}
		current.tokens += tokens
	}
	packages := make([]Package, 0, len(buckets))
	for i, bucket := range buckets {
		dirs := make([]string, 0, len(bucket.dirs))
		for dir := range bucket.dirs {
			dirs = append(dirs, dir)
		}
		sort.Strings(dirs)
		paths := make([]string, 0, len(bucket.files))
		for _, file := range bucket.files {
			paths = append(paths, file.Path)
		}
		focus := fmt.Sprintf("Review %d file(s) in %s", len(paths), strings.Join(dirs, ", "))
		focus = truncateRunes(focus, 2000)
		packages = append(packages, Package{
			ID:     fmt.Sprintf("p%d", i+1),
			Blocks: append([]string(nil), blocks...),
			Files:  paths,
			Focus:  focus,
		})
	}
	additions, deletions := 0, 0
	for _, file := range files {
		additions += file.Additions
		deletions += file.Deletions
	}
	risk := "low"
	reasons := []string{}
	switch {
	case len(files) > 20 || additions+deletions > 400:
		risk = "high"
		reasons = append(reasons, fmt.Sprintf("%d files and %d changed lines", len(files), additions+deletions))
	case len(files) > 5 || additions+deletions > 100:
		risk = "medium"
		reasons = append(reasons, fmt.Sprintf("%d files and %d changed lines", len(files), additions+deletions))
	default:
		reasons = append(reasons, "small change set")
	}
	if dossier.Pipeline != nil && dossier.Pipeline.Status == "failed" {
		reasons = append(reasons, "the latest pipeline failed")
	}
	runReasoner := params.AllowReasoner && (params.ReasonerWhen == "always" || (params.ReasonerWhen == "auto" && risk != "low"))
	challengeOnly := params.ChallengeOnly
	if len(challengeOnly) == 0 {
		challengeOnly = []Severity{SeverityP0, SeverityP1}
	}
	return PlanOut{
		Risk:          risk,
		RiskReasons:   reasons,
		Packages:      packages,
		RunReasoner:   runReasoner,
		ChallengeOnly: challengeOnly,
	}, nil
}

func estimateFileTokens(file FileChange) int {
	runes := len([]rune(file.Diff))
	return runes/4 + 200
}
