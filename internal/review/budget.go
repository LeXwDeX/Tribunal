package review

import (
	"errors"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

var errSynthesisBudgetExhausted = errors.New("review: workflow token budget exhausted during synthesis")

const (
	challengeLimitReason = "max_nodes limit reached; finding was not verified"
	budgetLimitWarning   = "max_nodes limit reached: some findings were not verified"
	budgetSettlementNote = "预算不足，部分条目未核验；以下结论仅来自既有规则裁决。"
)

// challengeLimit returns the number of additional nodes that still fit in
// the graph. A negative result means no limit is known or configured.
func challengeLimit(wf dag.WorkflowView) int {
	maxNodes := wf.Spec.Budget.MaxNodes
	if maxNodes <= 0 {
		return -1
	}
	remaining := maxNodes - len(wf.Spec.Nodes)
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// boundedChallengeTargets selects the most severe targets that fit and marks
// the rest as skipped. It preserves each target's original sequence so node
// IDs stay stable. limit < 0 disables limiting.
func boundedChallengeTargets(groups []Group, targets []ChallengeTarget, limit int) ([]Group, []ChallengeTarget, []string) {
	if limit < 0 || len(targets) <= limit {
		return groups, targets, nil
	}
	byID := make(map[string]Group, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	selected, kept := limitChallengeTargets(byID, targets, limit)
	for i, target := range targets {
		if _, ok := kept[i]; ok {
			continue
		}
		group, ok := byID[target.GroupID]
		if !ok || group.Challenge == nil || group.Challenge.Status != ChallengePending {
			continue
		}
		group.Challenge = &Challenge{Status: ChallengeSkipped, Reason: challengeLimitReason}
		byID[group.ID] = group
	}
	updated := make([]Group, len(groups))
	for i, group := range groups {
		if replacement, ok := byID[group.ID]; ok {
			updated[i] = replacement
		} else {
			updated[i] = group
		}
	}
	return updated, selected, []string{budgetLimitWarning}
}

// limitChallengeTargets keeps the most severe targets and returns the kept
// original indexes. The returned slice preserves the original target order.
func limitChallengeTargets(byID map[string]Group, targets []ChallengeTarget, limit int) ([]ChallengeTarget, map[int]struct{}) {
	if limit < 0 || len(targets) <= limit {
		return targets, nil
	}
	order := make([]int, len(targets))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(i, j int) bool {
		left, right := byID[targets[order[i]].GroupID], byID[targets[order[j]].GroupID]
		return severityRank[left.Severity] < severityRank[right.Severity]
	})
	kept := make(map[int]struct{}, limit)
	for _, index := range order[:limit] {
		kept[index] = struct{}{}
	}
	selected := make([]ChallengeTarget, 0, limit)
	for i, target := range targets {
		if _, ok := kept[i]; ok {
			selected = append(selected, target)
		}
	}
	return selected, kept
}

// applyChallengeLimit bounds gather output in place.
func applyChallengeLimit(out *GatherOut, wf dag.WorkflowView) {
	groups, targets, warnings := boundedChallengeTargets(out.Groups, out.ToChallenge, challengeLimit(wf))
	out.Groups = groups
	out.ToChallenge = targets
	out.Warnings = mergeWarnings(out.Warnings, warnings)
}

func verificationLimited(warnings []string) bool {
	for _, warning := range warnings {
		lower := strings.ToLower(warning)
		if strings.Contains(lower, "budget") || strings.Contains(lower, "max_nodes") {
			return true
		}
	}
	return false
}

// withVerificationCaveat keeps the first summary paragraph visible while
// stating that some findings were not model-verified. The caveat is inserted
// before long trailing text so truncation cannot remove it.
func withVerificationCaveat(summary string, warnings []string) string {
	if !verificationLimited(warnings) {
		return summary
	}
	note := budgetSettlementNote
	summary = strings.TrimSpace(summary)
	if summary == "" {
		return truncateRunes(note, MaxSummaryRunes)
	}
	head, rest := summary, ""
	if i := strings.Index(summary, "\n\n"); i >= 0 {
		head = strings.TrimSpace(summary[:i])
		rest = summary[i:]
	}
	available := MaxSummaryRunes - utf8.RuneCountInString(note) - 1
	if available < 0 {
		return truncateRunes(note, MaxSummaryRunes)
	}
	head = truncateRunes(head, available)
	combined := strings.TrimSpace(head+" "+note) + rest
	return truncateRunes(combined, MaxSummaryRunes)
}
