package review

import (
	"fmt"
	"strings"
)

// UnresolvedSignal records upstream evidence that findings cannot resolve.
// It is code-owned data: model prose never decides whether a review can pass.
type UnresolvedSignal struct {
	Node    string `json:"node"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

const (
	// UnresolvedEvidence marks an explicit evidence gap, for example a
	// missing pipeline log. A missing CI result is a gap, not a code defect.
	UnresolvedEvidence = "evidence_gap"
	// UnresolvedBlocking marks review-level blocking that no finding explains,
	// such as a gate verdict with no accepted finding.
	UnresolvedBlocking = "review_blocking"

	// 2000 covers the default six-package fan-out with 100 gaps per channel;
	// the cap keeps a malformed output from growing a workflow snapshot.
	maxUnresolvedSignals      = 2000
	maxUnresolvedSummaryItems = 8
	maxUnresolvedNode         = 128
	maxUnresolvedMessage      = 2000
	unresolvedSummaryPrefix   = "评审未完成：上游评审存在未解决的证据不足或阻断，不能给出 PASS。具体缺口："
)

// Validate checks one unresolved signal.
func (s UnresolvedSignal) Validate() error {
	if err := checkOneLine("node", s.Node, maxUnresolvedNode, true); err != nil {
		return err
	}
	switch s.Kind {
	case UnresolvedEvidence, UnresolvedBlocking:
	default:
		return invalid("kind", "must be %s or %s", UnresolvedEvidence, UnresolvedBlocking)
	}
	return checkOneLine("message", s.Message, maxUnresolvedMessage, true)
}

// normalizeUnresolved trims, validates, and deduplicates signals while keeping
// their first-seen order. No signal means a nil slice, which keeps omitempty
// output compatible with older snapshots.
func normalizeUnresolved(values []UnresolvedSignal) ([]UnresolvedSignal, error) {
	if len(values) == 0 {
		return nil, nil
	}
	out := make([]UnresolvedSignal, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		value.Node = strings.TrimSpace(value.Node)
		value.Message = oneLine(value.Message, maxUnresolvedMessage)
		if err := value.Validate(); err != nil {
			return nil, wrapField(fmt.Sprintf("unresolved[%d]", i), err)
		}
		key := unresolvedKey(value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	if len(out) > maxUnresolvedSignals {
		return nil, invalid("unresolved", "must contain at most %d entries", maxUnresolvedSignals)
	}
	return out, nil
}

// validateUnresolved checks a persisted list without changing it.
func validateUnresolved(values []UnresolvedSignal) error {
	if len(values) == 0 {
		return nil
	}
	if len(values) > maxUnresolvedSignals {
		return invalid("unresolved", "must contain at most %d entries", maxUnresolvedSignals)
	}
	seen := make(map[string]struct{}, len(values))
	for i, value := range values {
		if err := value.Validate(); err != nil {
			return wrapField(fmt.Sprintf("unresolved[%d]", i), err)
		}
		key := unresolvedKey(value)
		if _, ok := seen[key]; ok {
			return invalid(fmt.Sprintf("unresolved[%d]", i), "duplicate signal")
		}
		seen[key] = struct{}{}
	}
	return nil
}

func unresolvedKey(signal UnresolvedSignal) string {
	return signal.Node + "\x00" + signal.Kind + "\x00" + signal.Message
}

// mergeUnresolved concatenates already-normalized signals and drops duplicates.
func mergeUnresolved(values ...[]UnresolvedSignal) []UnresolvedSignal {
	out := make([]UnresolvedSignal, 0)
	seen := make(map[string]struct{})
	for _, list := range values {
		for _, signal := range list {
			key := unresolvedKey(signal)
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, signal)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// proposalUnresolved derives signals that the proposal's findings cannot
// explain. A BLOCKING verdict with findings is explained by those findings and
// is therefore resolved later by gate, adjudication, and synthesis.
func proposalUnresolved(p Proposal) []UnresolvedSignal {
	var out []UnresolvedSignal
	if p.EvidenceStatus == "insufficient" {
		gaps := make([]string, 0, len(p.EvidenceGaps))
		for _, gap := range p.EvidenceGaps {
			if strings.TrimSpace(gap) != "" {
				gaps = append(gaps, oneLine(gap, maxUnresolvedMessage))
			}
		}
		if len(gaps) == 0 {
			out = append(out, UnresolvedSignal{
				Node: p.Node, Kind: UnresolvedEvidence, Message: "evidence is insufficient",
			})
		}
		for _, gap := range gaps {
			out = append(out, UnresolvedSignal{Node: p.Node, Kind: UnresolvedEvidence, Message: gap})
		}
	}
	if p.Verdict == "BLOCKING" && len(p.Findings) == 0 {
		out = append(out, UnresolvedSignal{
			Node: p.Node, Kind: UnresolvedBlocking, Message: "review channel reported BLOCKING with no findings",
		})
	}
	return out
}

// gateUnresolved reports a gate-level blocking that has no accepted finding to
// explain it. Accepted findings already flow through gather and adjudication.
func gateUnresolved(node string, out ReviewGateOut) []UnresolvedSignal {
	if out.Verdict == "BLOCKING" && len(out.Accepted) == 0 {
		return []UnresolvedSignal{{
			Node: node, Kind: UnresolvedBlocking, Message: "review gate reported BLOCKING with no accepted findings",
		}}
	}
	return nil
}

// incompleteSummary states the actual gaps so the published note is useful.
func incompleteSummary(unresolved []UnresolvedSignal) string {
	var b strings.Builder
	b.WriteString(unresolvedSummaryPrefix)
	shown := unresolved
	extra := 0
	if len(shown) > maxUnresolvedSummaryItems {
		shown = shown[:maxUnresolvedSummaryItems]
		extra = len(unresolved) - maxUnresolvedSummaryItems
	}
	for i, signal := range shown {
		if i > 0 {
			b.WriteString("；")
		}
		b.WriteString(oneLine(signal.Node, 64))
		b.WriteString(": ")
		b.WriteString(oneLine(signal.Message, 200))
	}
	if extra > 0 {
		fmt.Fprintf(&b, "；另有 %d 项", extra)
	}
	return truncateRunes(b.String(), MaxSummaryRunes)
}

// applyUnresolved makes unresolved evidence a hard INCOMPLETE result. The raw
// model or rule verdict stays in Output; a PASS summary cannot mask the gaps.
func applyUnresolved(result *SynthesisResult, unresolved []UnresolvedSignal) {
	if result == nil || len(unresolved) == 0 {
		return
	}
	result.Unresolved = append([]UnresolvedSignal(nil), unresolved...)
	result.Verdict = VerdictIncomplete
	result.Summary = incompleteSummary(unresolved)
	result.Warnings = appendUnique(result.Warnings, "unresolved evidence or review-level blocking cannot be settled as PASS")
}
