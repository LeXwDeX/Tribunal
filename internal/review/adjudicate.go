package review

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	VerdictPass       = "PASS"
	VerdictBlocking   = "BLOCKING"
	VerdictIncomplete = "INCOMPLETE"
)

// OnUncertain is the project policy for an unverified or uncertain finding.
type OnUncertain string

const (
	OnUncertainDrop  OnUncertain = "drop"
	OnUncertainMark  OnUncertain = "mark"
	OnUncertainJudge OnUncertain = "judge"
)

// LocateFunc reports whether (path, line, side) is inside a diff hunk. The
// runtime wraps gitlab.DiffIndex.Locate here so this package stays independent
// of the GitLab client.
type LocateFunc func(path string, line int, side Side) bool

// EvidenceValidator checks one CodeRef against a fixed head/base snapshot.
// A non-nil error means the reference is not proven: callers must not treat
// the refutation as successful. A nil validator keeps the legacy syntax-only
// behavior for pure callers; production executors always supply one.
type EvidenceValidator func(CodeRef) error

// AdjudicateInput is the profile and diff context for rule adjudication.
type AdjudicateInput struct {
	MinVotes      float64
	ChallengeOnly []Severity
	OnUncertain   OnUncertain
	Locator       LocateFunc
	CodeEvidence  EvidenceValidator
	Warnings      []string
	Unresolved    []UnresolvedSignal
}

// AdjudicateResult is deterministic rule output.
type AdjudicateResult struct {
	Groups     []Group
	Warnings   []string
	Unresolved []UnresolvedSignal
}

// Adjudicate applies the ARCH.md decision table. It never calls an LLM.
func Adjudicate(groups []Group, in AdjudicateInput) (AdjudicateResult, error) {
	challengeOnly, err := normalizeChallengeOnly(in.ChallengeOnly)
	if err != nil {
		return AdjudicateResult{}, err
	}
	onUncertain := in.OnUncertain
	if onUncertain == "" {
		onUncertain = OnUncertainMark
	}
	switch onUncertain {
	case OnUncertainDrop, OnUncertainMark, OnUncertainJudge:
	default:
		return AdjudicateResult{}, invalid("on_uncertain", "must be drop, mark, or judge")
	}
	if in.MinVotes < 0 {
		return AdjudicateResult{}, invalid("min_votes", "must be zero or positive")
	}
	unresolved, err := normalizeUnresolved(in.Unresolved)
	if err != nil {
		return AdjudicateResult{}, err
	}

	out := AdjudicateResult{Groups: cloneGroups(groups), Unresolved: unresolved}
	if in.Locator == nil {
		for _, group := range out.Groups {
			if group.Line > 0 && group.Path != "" {
				out.Warnings = appendUnique(out.Warnings, "diff locator unavailable: positioned findings fall back to the summary")
				break
			}
		}
	}
	for _, warning := range in.Warnings {
		if strings.TrimSpace(warning) != "" {
			out.Warnings = appendUnique(out.Warnings, warning)
		}
	}
	for i := range out.Groups {
		group := &out.Groups[i]
		side := group.Side
		if side != SideOld {
			side = SideNew
		}
		if group.Line > 0 && group.Path != "" && in.Locator != nil {
			group.PositionOK = in.Locator(group.Path, group.Line, side)
		} else {
			group.PositionOK = false
		}
		if group.Decision != nil && group.Decision.Action == ActionDrop {
			continue
		}
		consensus := group.Consensus || meetsVotes(group.Votes, in.MinVotes)
		switch {
		case consensus:
			group.Decision = &FindingDecision{Action: ActionPublish, Reason: "met consensus threshold", Source: "rules"}
		case !severityIn(group.Severity, challengeOnly):
			group.Decision = &FindingDecision{Action: ActionPublish, Reason: "severity is outside challenge_only", Source: "rules"}
		default:
			action, reason := adjudicateChallenge(group, onUncertain, in.CodeEvidence)
			group.Decision = &FindingDecision{Action: action, Reason: reason, Source: "rules"}
		}
	}
	return out, nil
}

func adjudicateChallenge(group *Group, onUncertain OnUncertain, evidence EvidenceValidator) (Action, string) {
	challenge := group.Challenge
	if challenge == nil {
		reason := "no challenge result"
		group.Challenge = &Challenge{Status: ChallengeUncertain, Reason: reason}
		return uncertainAction(onUncertain, reason)
	}
	switch challenge.Status {
	case ChallengeConfirmed:
		return ActionPublish, "challenge confirmed"
	case ChallengeRefuted:
		if ValidCodeEvidence(challenge.Evidence, evidence) {
			return ActionDrop, "challenge refuted with a code location"
		}
		reason := "refutation has no verifiable code location in the fixed snapshot"
		challenge.Status = ChallengeUncertain
		challenge.Reason = reason
		return uncertainAction(onUncertain, reason)
	case ChallengeUncertain:
		return uncertainAction(onUncertain, "challenge uncertain")
	case ChallengePending:
		reason := "challenge not completed"
		challenge.Status = ChallengeUncertain
		challenge.Reason = reason
		return uncertainAction(onUncertain, reason)
	case ChallengeSkipped:
		reason := "challenge skipped"
		if challenge.Reason != "" {
			reason += ": " + challenge.Reason
		}
		challenge.Status = ChallengeUncertain
		challenge.Reason = reason
		return uncertainAction(onUncertain, reason)
	case ChallengeFailed:
		reason := "challenge failed"
		if challenge.Reason != "" {
			reason += ": " + challenge.Reason
		}
		challenge.Status = ChallengeUncertain
		challenge.Reason = reason
		return uncertainAction(onUncertain, reason)
	default:
		challenge.Status = ChallengeUncertain
		challenge.Reason = "unknown challenge status"
		return uncertainAction(onUncertain, "unknown challenge status")
	}
}

// ValidCodeEvidence reports whether at least one reference is valid and, when
// an EvidenceValidator is supplied, present in the fixed snapshot. A failed
// read never justifies a refutation.
func ValidCodeEvidence(refs []CodeRef, validator EvidenceValidator) bool {
	for _, ref := range refs {
		if ref.Validate() != nil {
			continue
		}
		if validator == nil || validator(ref) == nil {
			return true
		}
	}
	return false
}

func uncertainAction(policy OnUncertain, reason string) (Action, string) {
	switch policy {
	case OnUncertainDrop:
		return ActionDrop, reason + "; on_uncertain=drop"
	case OnUncertainJudge:
		return ActionJudge, reason + "; on_uncertain=judge"
	default:
		return ActionMark, reason + "; on_uncertain=mark"
	}
}

func cloneGroups(groups []Group) []Group {
	out := make([]Group, len(groups))
	for i, group := range groups {
		copy := group
		copy.Sources = append([]Finding(nil), group.Sources...)
		for j := range copy.Sources {
			if group.Sources[j].Decision != nil {
				decision := *group.Sources[j].Decision
				copy.Sources[j].Decision = &decision
			}
		}
		if group.Challenge != nil {
			challenge := *group.Challenge
			challenge.Evidence = append([]CodeRef(nil), group.Challenge.Evidence...)
			copy.Challenge = &challenge
		}
		if group.Decision != nil {
			decision := *group.Decision
			copy.Decision = &decision
		}
		out[i] = copy
	}
	return out
}

// SynthesisContext supplies code-owned ids and extension budget.
type SynthesisContext struct {
	WorkflowID   string
	NodeID       string
	Attempt      int
	Groups       []Group
	MaxExtends   int
	ExtendsUsed  int
	AllowedFiles []string
}

// ValidateSynthesis enforces I4, I5, I6, explicit judge decisions, exact
// group mapping, extension budget, and synthesize.2's no-extend rule.
func ValidateSynthesis(out SynthesizeOut, ctx SynthesisContext) error {
	if err := out.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(ctx.WorkflowID) == "" {
		return invalid("context.workflow_id", "must not be empty")
	}
	if strings.TrimSpace(ctx.NodeID) == "" {
		return invalid("context.node_id", "must not be empty")
	}
	if ctx.Attempt < 0 {
		return invalid("context.attempt", "must be zero or positive")
	}
	if ctx.MaxExtends < 0 || ctx.MaxExtends > HardMaxExtends {
		return invalid("context.max_extends", "must be between 0 and %d", HardMaxExtends)
	}
	if ctx.ExtendsUsed < 0 || ctx.ExtendsUsed > ctx.MaxExtends {
		return invalid("context.extends_used", "must be between 0 and max_extends")
	}

	groupByID := make(map[string]Group, len(ctx.Groups))
	newIDs := make(map[string]int, len(out.NewFindings))
	for i, group := range ctx.Groups {
		if group.ID == "" {
			return invalid(fmt.Sprintf("context.groups[%d].id", i), "must not be empty")
		}
		if _, exists := groupByID[group.ID]; exists {
			return invalid(fmt.Sprintf("context.groups[%d].id", i), "duplicate group id")
		}
		if len(group.Sources) == 0 {
			return invalid(fmt.Sprintf("context.groups[%d].sources", i), "must contain an origin finding")
		}
		if group.Decision == nil {
			return invalid(fmt.Sprintf("context.groups[%d].decision", i), "rule decision is required before synthesis")
		}
		groupByID[group.ID] = group
	}
	for i := range out.NewFindings {
		id := StableFindingID(ctx.WorkflowID, ctx.NodeID, ctx.Attempt, i)
		if _, exists := groupByID[id]; exists {
			return invalid(fmt.Sprintf("new_findings[%d]", i), "id collides with an existing group")
		}
		newIDs[id] = i
	}

	decisions := make(map[string]Decision, len(out.Decisions))
	for i, decision := range out.Decisions {
		if _, exists := decisions[decision.FindingID]; exists {
			return invalid(fmt.Sprintf("decisions[%d].finding_id", i), "duplicate decision")
		}
		if _, isNew := newIDs[decision.FindingID]; isNew {
			return invalid(fmt.Sprintf("decisions[%d].finding_id", i), "a new finding cannot be decided before verification")
		}
		group, exists := groupByID[decision.FindingID]
		if !exists {
			return invalid(fmt.Sprintf("decisions[%d].finding_id", i), "does not map to an existing group")
		}
		if decision.Action == "mark" {
			if _, isNew := newIDs[decision.FindingID]; isNew {
				return invalid(fmt.Sprintf("decisions[%d].action", i), "a new finding cannot be auto-marked")
			}
		}
		if decision.Action == "downgrade" {
			if !decision.Severity.Valid() || !group.Severity.Higher(decision.Severity) {
				return invalid(fmt.Sprintf("decisions[%d].severity", i), "must be a lower severity than the group")
			}
		}
		decisions[decision.FindingID] = decision
	}

	for _, group := range ctx.Groups {
		if group.Decision.Action != ActionJudge {
			continue
		}
		if _, ok := decisions[group.ID]; !ok {
			return invalid("decisions", "judge group %s needs an explicit decision", group.ID)
		}
	}

	for i, decision := range out.Decisions {
		group, ok := groupByID[decision.FindingID]
		if !ok {
			continue
		}
		if (group.Severity == SeverityP0 || group.Severity == SeverityP1) &&
			group.Challenge != nil && group.Challenge.Status == ChallengeConfirmed &&
			group.Decision.Action == ActionPublish {
			if decision.Action == "drop" || decision.Action == "downgrade" {
				if utf8.RuneCountInString(strings.TrimSpace(decision.Reason)) < 20 {
					return invalid(fmt.Sprintf("decisions[%d].reason", i), "must be at least 20 characters for a confirmed severe finding")
				}
				if len(group.Sources) == 0 {
					return invalid(fmt.Sprintf("decisions[%d].finding_id", i), "cannot map back to an origin ref")
				}
			}
		}
	}

	remaining := ctx.MaxExtends - ctx.ExtendsUsed
	if len(out.ExtendRequests) > remaining {
		return invalid("extend_requests", "exceeds remaining extension budget %d", remaining)
	}
	if ctx.NodeID == "synthesize.2" {
		if len(out.ExtendRequests) != 0 {
			return invalid("extend_requests", "synthesize.2 cannot extend again")
		}
		if len(out.NewFindings) != 0 {
			return invalid("new_findings", "synthesize.2 cannot add unverified findings")
		}
	}

	allowedFiles := stringSet(ctx.AllowedFiles)
	verifyByGroup := make(map[string]int)
	newVerify := make(map[int]int, len(out.NewFindings))
	for i, request := range out.ExtendRequests {
		switch request.Kind {
		case "verify":
			if request.NewFindingIndex != nil {
				index := *request.NewFindingIndex
				if index < 0 || index >= len(out.NewFindings) {
					return invalid(fmt.Sprintf("extend_requests[%d].new_finding_index", i), "does not select a new finding")
				}
				newVerify[index]++
				if newVerify[index] > 1 {
					return invalid(fmt.Sprintf("extend_requests[%d].new_finding_index", i), "duplicate verify request for a new finding")
				}
				continue
			}
			if _, ok := groupByID[request.FindingID]; !ok {
				return invalid(fmt.Sprintf("extend_requests[%d].finding_id", i), "does not map to an existing group")
			}
			verifyByGroup[request.FindingID]++
			if verifyByGroup[request.FindingID] > 1 {
				return invalid(fmt.Sprintf("extend_requests[%d].finding_id", i), "duplicate verify request")
			}
		case "review":
			if request.Files != nil {
				for j, file := range request.Files {
					if _, ok := allowedFiles[file]; !ok {
						return invalid(fmt.Sprintf("extend_requests[%d].files[%d]", i, j), "file is not in the changed set")
					}
				}
			}
		}
	}
	for i := range out.NewFindings {
		if newVerify[i] != 1 {
			return invalid(fmt.Sprintf("new_findings[%d]", i), "must have exactly one verify extension request")
		}
	}

	published := make(map[string]Severity, len(groupByID))
	for _, group := range ctx.Groups {
		decision := group.Decision
		if override, ok := decisions[group.ID]; ok {
			decision = &FindingDecision{Action: Action(override.Action), Reason: override.Reason, Severity: override.Severity, Source: "synthesize"}
		}
		if decision == nil {
			return invalid("decisions", "group %s has no final decision", group.ID)
		}
		final := group.Severity
		if decision.Action == ActionDowngrade && decision.Severity.Valid() {
			final = decision.Severity
		}
		if decision.Action == ActionPublish || decision.Action == ActionMark || decision.Action == ActionDowngrade {
			published[group.ID] = final
		}
		if decision.Action == ActionJudge {
			return invalid("decisions", "judge action remained unresolved for group %s", group.ID)
		}
	}
	if out.Verdict == VerdictPass {
		for id, severity := range published {
			if severity == SeverityP0 || severity == SeverityP1 {
				return invalid("verdict", "PASS is forbidden while group %s publishes %s", id, severity)
			}
		}
	}
	return nil
}

// ApplySynthesisDecisions returns a copy with explicit decisions applied. It
// does not validate; call ValidateSynthesis first.
func ApplySynthesisDecisions(groups []Group, out SynthesizeOut) []Group {
	applied := cloneGroups(groups)
	decisions := make(map[string]Decision, len(out.Decisions))
	for _, decision := range out.Decisions {
		decisions[decision.FindingID] = decision
	}
	for i := range applied {
		group := &applied[i]
		decision, ok := decisions[group.ID]
		if !ok {
			continue
		}
		group.Decision = &FindingDecision{
			Action:   Action(decision.Action),
			Reason:   decision.Reason,
			Severity: decision.Severity,
			Source:   "synthesize",
		}
	}
	return applied
}

// SynthesisFallback is an explicit deterministic result used when model
// synthesis fails validation. It is never labelled as a model PASS.
type SynthesisFallback struct {
	Groups   []Group
	Verdict  string
	Summary  string
	Warnings []string
}

// BuildRulesFallback marks unresolved judge groups as uncertain and reports
// that the model output was rejected. It never invents a clean model verdict.
func BuildRulesFallback(groups []Group, cause error) SynthesisFallback {
	out := SynthesisFallback{Groups: cloneGroups(groups)}
	warnings := []string{"synthesis output rejected; deterministic rules fallback used"}
	if cause != nil {
		warnings = append(warnings, truncateRunes(cause.Error(), 500))
	}
	out.Warnings = warnings
	for i := range out.Groups {
		group := &out.Groups[i]
		if group.Decision == nil {
			group.Decision = &FindingDecision{Action: ActionMark, Reason: "rule result unavailable; fallback marks the finding uncertain", Source: "rules_fallback"}
			continue
		}
		if group.Decision.Action == ActionJudge {
			group.Decision = &FindingDecision{Action: ActionMark, Reason: "synthesis invalid; unresolved judge group is published as uncertain", Source: "rules_fallback"}
		}
	}
	blocking := false
	for _, group := range out.Groups {
		if !group.Published() {
			continue
		}
		if severity := group.EffectiveSeverity(); severity == SeverityP0 || severity == SeverityP1 {
			blocking = true
			break
		}
	}
	if blocking {
		out.Verdict = VerdictBlocking
	} else {
		out.Verdict = VerdictPass
	}
	out.Summary = "规则兜底：模型 synthesis 输出未通过确定性校验，以下结论仅来自规则与挑战结果，不是模型结论。未决条目按存疑发布。"
	if cause != nil {
		out.Summary += " 校验原因：" + truncateRunes(cause.Error(), 500)
	}
	return out
}
