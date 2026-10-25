package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

const defaultSameModelWeight = 0.5

// Side is the diff side of a finding line.
type Side string

const (
	SideNew Side = "new"
	SideOld Side = "old"
)

// Action is a finding decision action. Judge is an intermediate adjudication
// result that synthesize must resolve.
type Action string

const (
	ActionPublish   Action = "publish"
	ActionDrop      Action = "drop"
	ActionMark      Action = "mark"
	ActionDowngrade Action = "downgrade"
	ActionJudge     Action = "judge"
)

// ChallengeStatus is the normalized challenger result.
type ChallengeStatus string

const (
	ChallengePending   ChallengeStatus = "pending"
	ChallengeConfirmed ChallengeStatus = "confirmed"
	ChallengeRefuted   ChallengeStatus = "refuted"
	ChallengeUncertain ChallengeStatus = "uncertain"
	ChallengeFailed    ChallengeStatus = "failed"
	ChallengeSkipped   ChallengeStatus = "skipped"
)

// Origin identifies where a finding came from.
type Origin struct {
	Node     string
	Index    int
	Attempt  int
	Provider string
	Model    string
	Main     bool
}

// Finding is the unified, code-owned finding record.
type Finding struct {
	WorkflowID string
	ID         string
	Origin     Origin
	Severity   Severity
	Category   string
	Path       string
	Line       int
	Side       Side
	Title      string
	Body       string
	Suggestion string
	Evidence   string
	Confidence Confidence
	Decision   *FindingDecision
	PublishKey string
}

// FindingDecision is the rule, gate, judge, or fallback decision for a group.
type FindingDecision struct {
	Action   Action
	Reason   string
	Severity Severity
	Source   string
}

// Challenge is one verification result attached to a group.
type Challenge struct {
	Status            ChallengeStatus
	Node              string
	Provider          string
	Model             string
	Reason            string
	Evidence          []CodeRef
	SuggestedSeverity Severity
	SameModel         bool
}

// Group is one deduplicated finding and all of its sources.
type Group struct {
	ID         string
	Origin     Origin
	Severity   Severity
	Category   string
	Path       string
	Line       int
	Side       Side
	Title      string
	Body       string
	Suggestion string
	Evidence   string
	Confidence Confidence
	Sources    []Finding
	Votes      float64
	Consensus  bool
	Challenge  *Challenge
	Decision   *FindingDecision
	PositionOK bool
	PublishKey string
}

// EffectiveSeverity applies a downgrade decision without changing the source.
func (g Group) EffectiveSeverity() Severity {
	if g.Decision != nil && g.Decision.Action == ActionDowngrade && g.Decision.Severity.Valid() {
		return g.Decision.Severity
	}
	return g.Severity
}

// Published reports whether the group is intended for the merge request.
// Downgrade publishes at the lower severity, so it is also final.
func (g Group) Published() bool {
	return g.Decision != nil && (g.Decision.Action == ActionPublish || g.Decision.Action == ActionMark || g.Decision.Action == ActionDowngrade)
}

// Proposal is one proposer node's qualified findings. Verdict, EvidenceStatus,
// and EvidenceGaps keep the review channel's explicit evidence state so gather
// can distinguish an explained BLOCKING from unresolved missing evidence.
type Proposal struct {
	Node           string
	Kind           string
	Provider       string
	Model          string
	Attempt        int
	Verdict        string
	EvidenceStatus string
	EvidenceGaps   []string
	Findings       []FindingOut
}

// DroppedFinding is a review.gate rebuttal. The finding is recorded but not
// grouped or challenged.
type DroppedFinding struct {
	Ref    Ref
	Reason string
}

// GatherInput is the pure input to Gather.
type GatherInput struct {
	WorkflowID string
	Proposals  []Proposal
	Dropped    []DroppedFinding
	// Unresolved carries review-level signals that no finding explains, such
	// as a BLOCKING gate with no accepted finding.
	Unresolved []UnresolvedSignal
}

// GatherOptions is the profile context for dedupe and challenge fan-out.
type GatherOptions struct {
	ChallengeOnly     []Severity
	MinVotes          float64
	SameModelWeight   float64
	ChallengeProvider string
	FallbackProvider  string
	AllowedProviders  []string
	// Identities maps a provider name to a stable endpoint+model identity.
	// nil keeps the legacy provider/model vote key; a non-nil map is
	// conservative: missing or empty metadata is treated as possibly the same
	// model and cannot prove an independent vote.
	Identities      map[string]string
	BudgetExhausted bool
}

// ChallengeTarget asks the engine to expand one verify node.
type ChallengeTarget struct {
	GroupID   string
	Sequence  int // 1-based, matching verify.<sequence>
	Provider  string
	SameModel bool
}

// GatherResult is deterministic and contains no store access.
type GatherResult struct {
	Groups      []Group
	Dropped     []Finding
	ToChallenge []ChallengeTarget
	Warnings    []string
	Unresolved  []UnresolvedSignal
}

// ProposalFromCodeReview converts a strict code-review output.
func ProposalFromCodeReview(node, provider, model string, attempt int, out CodeReviewOut) Proposal {
	return Proposal{Node: node, Kind: ContractCodeReview, Provider: provider, Model: model, Attempt: attempt, Findings: out.Findings}
}

// ProposalFromReview converts a standards or intent output. kind must identify
// the channel for dedupe and voting.
func ProposalFromReview(node, kind, provider, model string, attempt int, out ReviewOut) Proposal {
	if kind == "" {
		kind = ContractReview
	}
	return Proposal{
		Node: node, Kind: kind, Provider: provider, Model: model, Attempt: attempt,
		Verdict: out.Verdict, EvidenceStatus: out.EvidenceStatus,
		EvidenceGaps: append([]string(nil), out.EvidenceGaps...),
		Findings:     out.Findings,
	}
}

// ProposalFromReasoner normalizes the reasoner severity names and fields.
func ProposalFromReasoner(node, provider, model string, attempt int, out ReasonerOut) Proposal {
	findings := make([]FindingOut, 0, len(out.Findings))
	for _, finding := range out.Findings {
		severity := reasonerSeverity(finding.Severity)
		category := slugCategory(finding.Dimension)
		title := oneLine(finding.Finding, MaxTitleRunes)
		if title == "" {
			title = "Reasoner finding"
		}
		body := finding.Why
		if finding.Where != "" {
			body = body + "\n\nLocation: " + finding.Where
		}
		findings = append(findings, FindingOut{
			Path:       finding.Path,
			Line:       finding.Line,
			Side:       "new",
			Severity:   severity,
			Category:   category,
			Title:      title,
			Body:       body,
			Evidence:   finding.Where,
			Confidence: finding.Confidence,
		})
	}
	return Proposal{Node: node, Kind: ContractReasoner, Provider: provider, Model: model, Attempt: attempt, Findings: findings}
}

func reasonerSeverity(value string) Severity {
	switch value {
	case "critical":
		return SeverityP1
	case "notable":
		return SeverityP2
	case "informational":
		return SeverityInfo
	default:
		return SeverityInfo
	}
}

func slugCategory(value string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(value) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case !lastDash && b.Len() > 0:
			b.WriteByte('_')
			lastDash = true
		}
		if b.Len() >= 32 {
			break
		}
	}
	result := strings.Trim(b.String(), "_")
	if result == "" || result[0] < 'a' || result[0] > 'z' {
		return "logic"
	}
	return result
}

func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}

// Gather normalizes proposals, drops gate rebuttals, deduplicates, counts
// weighted votes, and selects deterministic challenge targets.
func Gather(in GatherInput, opts GatherOptions) (GatherResult, error) {
	if strings.TrimSpace(in.WorkflowID) == "" {
		return GatherResult{}, invalid("workflow_id", "must not be empty")
	}
	challengeOnly, err := normalizeChallengeOnly(opts.ChallengeOnly)
	if err != nil {
		return GatherResult{}, err
	}
	if opts.MinVotes < 0 {
		return GatherResult{}, invalid("min_votes", "must be zero or positive")
	}
	weight := opts.SameModelWeight
	if weight == 0 {
		weight = defaultSameModelWeight
	}
	if weight < 0 || weight > 1 {
		return GatherResult{}, invalid("same_model_weight", "must be between 0 and 1")
	}
	if opts.FallbackProvider != "" && len(opts.AllowedProviders) > 0 {
		allowed := stringSet(opts.AllowedProviders)
		if _, ok := allowed[opts.FallbackProvider]; !ok {
			return GatherResult{}, invalid("fallback_provider", "must be in allowed providers")
		}
	}

	findings, byRef, err := prepareFindings(in)
	if err != nil {
		return GatherResult{}, err
	}
	dropped, err := prepareDropped(in, byRef)
	if err != nil {
		return GatherResult{}, err
	}
	derived := make([]UnresolvedSignal, 0)
	for _, proposal := range in.Proposals {
		derived = append(derived, proposalUnresolved(proposal)...)
	}
	unresolved, err := normalizeUnresolved(append(derived, in.Unresolved...))
	if err != nil {
		return GatherResult{}, err
	}
	droppedRefs := make(map[Ref]struct{}, len(dropped))
	for _, finding := range dropped {
		droppedRefs[Ref{Node: finding.Origin.Node, Index: finding.Origin.Index}] = struct{}{}
	}
	kept := findings[:0]
	for _, finding := range findings {
		if _, isDropped := droppedRefs[Ref{Node: finding.Origin.Node, Index: finding.Origin.Index}]; !isDropped {
			kept = append(kept, finding)
		}
	}
	groups := Dedupe(kept)

	result := GatherResult{Groups: groups, Dropped: dropped, Unresolved: unresolved}
	budgetWarning := false
	for i := range result.Groups {
		group := &result.Groups[i]
		group.Votes = countVotes(group.Sources, weight, opts.Identities)
		group.Consensus = meetsVotes(group.Votes, opts.MinVotes)
		if !severityIn(group.Severity, challengeOnly) || group.Consensus {
			continue
		}
		if opts.BudgetExhausted {
			group.Challenge = &Challenge{Status: ChallengeSkipped, Reason: "budget exhausted"}
			if !budgetWarning {
				result.Warnings = appendUnique(result.Warnings, "budget exhausted: some findings were not verified")
				budgetWarning = true
			}
			continue
		}
		provider, sameModel, warning, err := selectChallenger(*group, opts)
		if err != nil {
			return GatherResult{}, err
		}
		if warning != "" {
			result.Warnings = appendUnique(result.Warnings, warning)
		}
		group.Challenge = &Challenge{Status: ChallengePending, Provider: provider, SameModel: sameModel}
		result.ToChallenge = append(result.ToChallenge, ChallengeTarget{
			GroupID:   group.ID,
			Sequence:  len(result.ToChallenge) + 1,
			Provider:  provider,
			SameModel: sameModel,
		})
	}
	return result, nil
}

func normalizeChallengeOnly(values []Severity) ([]Severity, error) {
	if values == nil {
		return []Severity{SeverityP0, SeverityP1}, nil
	}
	if len(values) == 0 || len(values) > 4 {
		return nil, invalid("challenge_only", "must contain 1 to 4 severities")
	}
	seen := make(map[Severity]struct{}, len(values))
	out := make([]Severity, 0, len(values))
	for _, value := range values {
		if !value.Valid() {
			return nil, invalid("challenge_only", "invalid severity %q", value)
		}
		if _, ok := seen[value]; ok {
			return nil, invalid("challenge_only", "duplicate severity")
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

func severityIn(value Severity, values []Severity) bool {
	for _, candidate := range values {
		if value == candidate {
			return true
		}
	}
	return false
}

func prepareFindings(in GatherInput) ([]Finding, map[Ref]Finding, error) {
	findings := make([]Finding, 0)
	byRef := make(map[Ref]Finding)
	for p, proposal := range in.Proposals {
		if strings.TrimSpace(proposal.Node) == "" {
			return nil, nil, invalid(fmt.Sprintf("proposals[%d].node", p), "must not be empty")
		}
		if proposal.Attempt < 0 {
			return nil, nil, invalid(fmt.Sprintf("proposals[%d].attempt", p), "must be zero or positive")
		}
		for i, contractFinding := range proposal.Findings {
			if err := contractFinding.Validate(); err != nil {
				return nil, nil, wrapField(fmt.Sprintf("proposals[%d].findings[%d]", p, i), err)
			}
			side := Side(contractFinding.Side)
			if side == "" {
				side = SideNew
			}
			finding := Finding{
				WorkflowID: in.WorkflowID,
				ID:         StableFindingID(in.WorkflowID, proposal.Node, proposal.Attempt, i),
				Origin: Origin{
					Node:     proposal.Node,
					Index:    i,
					Attempt:  proposal.Attempt,
					Provider: proposal.Provider,
					Model:    proposal.Model,
				},
				Severity:   contractFinding.Severity,
				Category:   contractFinding.Category,
				Path:       contractFinding.Path,
				Line:       contractFinding.Line,
				Side:       side,
				Title:      contractFinding.Title,
				Body:       contractFinding.Body,
				Suggestion: contractFinding.Suggestion,
				Evidence:   contractFinding.Evidence,
				Confidence: contractFinding.Confidence,
			}
			finding.PublishKey = PublishKeyFor(finding.Path, finding.Title, snippetFor(finding))
			findings = append(findings, finding)
			ref := Ref{Node: proposal.Node, Index: i}
			if _, exists := byRef[ref]; exists {
				return nil, nil, invalid(fmt.Sprintf("proposals[%d].findings[%d]", p, i), "duplicate node/index ref")
			}
			byRef[ref] = finding
		}
	}
	return findings, byRef, nil
}

func prepareDropped(in GatherInput, byRef map[Ref]Finding) ([]Finding, error) {
	out := make([]Finding, 0, len(in.Dropped))
	seen := make(map[Ref]struct{}, len(in.Dropped))
	for i, dropped := range in.Dropped {
		ref := dropped.Ref
		if err := ref.Validate(); err != nil {
			return nil, wrapField(fmt.Sprintf("dropped[%d].ref", i), err)
		}
		if _, ok := seen[ref]; ok {
			return nil, invalid(fmt.Sprintf("dropped[%d].ref", i), "duplicate rebuttal ref")
		}
		seen[ref] = struct{}{}
		source, ok := byRef[ref]
		if !ok {
			return nil, invalid(fmt.Sprintf("dropped[%d].ref", i), "rebuttal ref does not exist")
		}
		reason := strings.TrimSpace(dropped.Reason)
		if reason == "" {
			reason = "rebutted by review gate"
		}
		copy := source
		copy.Decision = &FindingDecision{Action: ActionDrop, Reason: reason, Source: "review.gate"}
		out = append(out, copy)
	}
	return out, nil
}

// Dedupe merges findings by path, category, and line proximity. Findings
// without a line merge only when their normalized titles match.
func Dedupe(findings []Finding) []Group {
	groups := make([]Group, 0, len(findings))
	for _, finding := range findings {
		merged := false
		for i := range groups {
			if canMerge(groups[i], finding) {
				mergeInto(&groups[i], finding)
				merged = true
				break
			}
		}
		if !merged {
			groups = append(groups, newGroup(finding))
		}
	}
	for i := range groups {
		groups[i].PublishKey = PublishKeyFor(groups[i].Path, groups[i].Title, snippetFor(Finding{Evidence: groups[i].Evidence, Body: groups[i].Body}))
	}
	return groups
}

func canMerge(group Group, finding Finding) bool {
	if group.Path != finding.Path || group.Category != finding.Category {
		return false
	}
	if group.Line == 0 || finding.Line == 0 {
		return group.Line == 0 && finding.Line == 0 && NormalizeTitle(group.Title) == NormalizeTitle(finding.Title)
	}
	difference := group.Line - finding.Line
	return difference >= -3 && difference <= 3
}

func newGroup(finding Finding) Group {
	return Group{
		ID:         StableGroupID(finding.WorkflowID, finding.Origin.Node, finding.Origin.Attempt, finding.Origin.Index),
		Origin:     finding.Origin,
		Severity:   finding.Severity,
		Category:   finding.Category,
		Path:       finding.Path,
		Line:       finding.Line,
		Side:       finding.Side,
		Title:      finding.Title,
		Body:       finding.Body,
		Suggestion: finding.Suggestion,
		Evidence:   finding.Evidence,
		Confidence: finding.Confidence,
		Sources:    []Finding{finding},
		PublishKey: finding.PublishKey,
	}
}

func mergeInto(group *Group, finding Finding) {
	group.Sources = append(group.Sources, finding)
	if finding.Severity.Higher(group.Severity) {
		group.Severity = finding.Severity
	}
	if richer(finding, groupDisplay(*group)) {
		group.Title = finding.Title
		group.Body = finding.Body
		group.Suggestion = finding.Suggestion
		group.Evidence = finding.Evidence
		group.Confidence = finding.Confidence
		group.Line = finding.Line
		group.Side = finding.Side
		group.Category = finding.Category
		group.Path = finding.Path
	}
}

func groupDisplay(group Group) Finding {
	return Finding{
		Title:      group.Title,
		Body:       group.Body,
		Suggestion: group.Suggestion,
		Evidence:   group.Evidence,
		Confidence: group.Confidence,
	}
}

func richer(candidate, current Finding) bool {
	candidateEvidence := utf8.RuneCountInString(candidate.Evidence)
	currentEvidence := utf8.RuneCountInString(current.Evidence)
	if candidateEvidence != currentEvidence {
		return candidateEvidence > currentEvidence
	}
	candidateBody := utf8.RuneCountInString(candidate.Body)
	currentBody := utf8.RuneCountInString(current.Body)
	if candidateBody != currentBody {
		return candidateBody > currentBody
	}
	return confidenceRank(candidate.Confidence) > confidenceRank(current.Confidence)
}

func confidenceRank(value Confidence) int {
	switch value {
	case ConfidenceHigh:
		return 3
	case ConfidenceMedium:
		return 2
	case ConfidenceLow:
		return 1
	default:
		return 0
	}
}

// CountVotes counts one full vote per provider/model pair and applies the
// configured weight to extra findings from the same pair. Standards and intent
// channels from the same model therefore do not become independent votes.
// Callers that know endpoint/model identities should use GatherOptions.
func CountVotes(sources []Finding, sameModelWeight float64) float64 {
	return countVotes(sources, sameModelWeight, nil)
}

func countVotes(sources []Finding, sameModelWeight float64, identities map[string]string) float64 {
	weight := sameModelWeight
	if weight == 0 {
		weight = defaultSameModelWeight
	}
	if weight < 0 || weight > 1 {
		return 0
	}
	seen := make(map[string]struct{}, len(sources))
	total := 0.0
	for _, source := range sources {
		key := ""
		if identities == nil {
			key = source.Origin.Provider + "\x1f" + source.Origin.Model
			if source.Origin.Provider == "" && source.Origin.Model == "" {
				key = "\x00anonymous"
			}
		} else {
			key = identities[source.Origin.Provider]
			if key == "" {
				key = "\x00unknown-model"
			}
		}
		if _, ok := seen[key]; ok {
			total += weight
		} else {
			total++
			seen[key] = struct{}{}
		}
	}
	return total
}

func meetsVotes(votes, minVotes float64) bool {
	return minVotes > 0 && votes+1e-9 >= minVotes
}

func selectChallenger(group Group, opts GatherOptions) (string, bool, string, error) {
	allowed := stringSet(opts.AllowedProviders)
	switch opts.ChallengeProvider {
	case "":
		return "", false, "", invalid("challenge_provider", "must be configured when a finding needs challenge")
	case "cross":
		if opts.Identities == nil {
			return selectCrossByProvider(group, opts)
		}
		sourceIdentities, sourceUnknown := groupSourceIdentities(group, opts.Identities)
		for _, candidate := range opts.AllowedProviders {
			if candidate == "" {
				continue
			}
			identity := opts.Identities[candidate]
			if identity == "" || sourceUnknown {
				continue
			}
			if _, used := sourceIdentities[identity]; !used {
				return candidate, false, "", nil
			}
		}
		if opts.FallbackProvider == "" {
			return "", false, "", invalid("fallback_provider", "cross challenge has no independent model and no fallback")
		}
		fallbackIdentity := opts.Identities[opts.FallbackProvider]
		_, used := sourceIdentities[fallbackIdentity]
		sameModel := sourceUnknown || fallbackIdentity == "" || used
		if sameModel {
			return opts.FallbackProvider, true, "cross challenge cannot use an independent model for group " + group.ID + "; fallback " + opts.FallbackProvider + " is not cross-model confirmation", nil
		}
		return opts.FallbackProvider, false, "", nil
	default:
		if len(allowed) > 0 {
			if _, ok := allowed[opts.ChallengeProvider]; !ok {
				return "", false, "", invalid("challenge_provider", "provider %q is not allowed", opts.ChallengeProvider)
			}
		}
		if opts.Identities == nil {
			_, sameModel := sourceProviders(group)[opts.ChallengeProvider]
			return opts.ChallengeProvider, sameModel, "", nil
		}
		sourceIdentities, sourceUnknown := groupSourceIdentities(group, opts.Identities)
		identity := opts.Identities[opts.ChallengeProvider]
		_, used := sourceIdentities[identity]
		return opts.ChallengeProvider, sourceUnknown || identity == "" || used, "", nil
	}
}

func selectCrossByProvider(group Group, opts GatherOptions) (string, bool, string, error) {
	sourceProviders := sourceProviders(group)
	for _, candidate := range opts.AllowedProviders {
		if candidate == "" {
			continue
		}
		if _, used := sourceProviders[candidate]; !used {
			return candidate, false, "", nil
		}
	}
	if opts.FallbackProvider == "" {
		return "", false, "", invalid("fallback_provider", "cross challenge has no unused provider and no fallback")
	}
	_, sameModel := sourceProviders[opts.FallbackProvider]
	warning := ""
	if sameModel {
		warning = "cross challenge fallback uses a proposer provider for group " + group.ID
	}
	return opts.FallbackProvider, sameModel, warning, nil
}

func sourceProviders(group Group) map[string]struct{} {
	out := make(map[string]struct{}, len(group.Sources))
	for _, source := range group.Sources {
		if source.Origin.Provider != "" {
			out[source.Origin.Provider] = struct{}{}
		}
	}
	return out
}

func groupSourceIdentities(group Group, identities map[string]string) (map[string]struct{}, bool) {
	out := make(map[string]struct{}, len(group.Sources))
	unknown := false
	for _, source := range group.Sources {
		if source.Origin.Provider == "" {
			unknown = true
			continue
		}
		identity := identities[source.Origin.Provider]
		if identity == "" {
			unknown = true
			continue
		}
		out[identity] = struct{}{}
	}
	return out, unknown
}

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// StableFindingID returns the code-owned id from workflow, node, attempt, and
// the finding's index in the node output.
func StableFindingID(workflowID, node string, attempt, index int) string {
	return stableID("f_", workflowID, node, attempt, index)
}

// StableGroupID returns the code-owned id for a deduplicated group.
func StableGroupID(workflowID, node string, attempt, index int) string {
	return stableID("g_", workflowID, node, attempt, index)
}

func stableID(prefix, workflowID, node string, attempt, index int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", workflowID, node, attempt, index)))
	return prefix + hex.EncodeToString(sum[:12])
}

// NormalizeTitle gives line-less dedupe and publish keys a stable comparison.
func NormalizeTitle(title string) string {
	return strings.ToLower(strings.Join(strings.Fields(title), " "))
}

// PublishKeyFor creates a stable publication key from path, normalized title,
// and a short snippet. The workflow fingerprint and line number are
// deliberately excluded so reruns update the same discussion.
func PublishKeyFor(path, title, snippet string) string {
	normalizedSnippet := strings.Join(strings.Fields(snippet), " ")
	normalizedSnippet = truncateRunes(normalizedSnippet, 512)
	value := strings.TrimSpace(path) + "\x00" + NormalizeTitle(title) + "\x00" + normalizedSnippet
	sum := sha256.Sum256([]byte(value))
	return "pk_" + hex.EncodeToString(sum[:16])
}

func snippetFor(finding Finding) string {
	if strings.TrimSpace(finding.Evidence) != "" {
		return finding.Evidence
	}
	return finding.Body
}
