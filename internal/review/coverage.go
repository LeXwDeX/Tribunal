package review

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
)

// coverageUnresolved turns material and prompt truncation into hard evidence
// gaps. The app layer owns collection and sets DiffSummary.Truncated for
// capacity, too-large, or collapsed diffs. Prompt truncation is deterministic
// here. Neither code nor agent synthesis can clear these signals.
func (e *Executor) coverageUnresolved(wf dag.WorkflowView) []UnresolvedSignal {
	if e == nil || e.deps.Dossier == nil {
		return nil
	}
	d := e.deps.Dossier
	var signals []UnresolvedSignal
	if message := collectionCoverageGap(d); message != "" {
		signals = append(signals, UnresolvedSignal{
			Node: "coverage", Kind: UnresolvedEvidence, Message: message,
		})
	}
	if len(d.Files) > MaxFindings {
		signals = append(signals, UnresolvedSignal{
			Node: "plan",
			Kind: UnresolvedEvidence,
			Message: fmt.Sprintf(
				"plan prompt lists at most %d files; %d changed files were omitted before planning",
				MaxFindings, len(d.Files)-MaxFindings),
		})
	}
	for _, id := range wf.Order {
		node, ok := wf.Nodes[id]
		if !ok {
			continue
		}
		switch node.Spec.Kind {
		case KindCodeReview, KindReviewStd, KindReviewInt:
		default:
			continue
		}
		params, err := decodeParams[reviewNodeParams](node.Spec.Params)
		if err != nil {
			continue
		}
		for _, gap := range promptDiffGaps(d, params.Files) {
			signals = append(signals, UnresolvedSignal{
				Node: node.ID, Kind: UnresolvedEvidence, Message: gap,
			})
		}
	}
	if plan, ok := completedPlan(wf); ok {
		assigned := make(map[string]struct{})
		for _, pkg := range plan.Packages {
			for _, filePath := range pkg.Files {
				assigned[filePath] = struct{}{}
			}
		}
		if message := unassignedCoverageGap(d, assigned); message != "" {
			signals = append(signals, UnresolvedSignal{
				Node: "plan", Kind: UnresolvedEvidence, Message: message,
			})
		}
	}
	return signals
}

// completedPlan returns the planner's declared file assignment. Without a
// completed plan this check is skipped: evidence-only unit workflows do not
// model planning.
func completedPlan(wf dag.WorkflowView) (PlanOut, bool) {
	node, ok := wf.Nodes[NodePlan]
	if !ok || node.Status != dag.NodeCompleted {
		return PlanOut{}, false
	}
	plan, err := DecodeStrict[*PlanOut](node.Output)
	if err != nil || plan == nil {
		return PlanOut{}, false
	}
	return *plan, true
}

// collectionCoverageGap reports omitted collection scope with exact counts.
// Older snapshots only carry truncated=true; their omitted paths stay unknown
// and therefore fail closed.
func collectionCoverageGap(d *Dossier) string {
	if d == nil {
		return ""
	}
	summary := d.DiffSummary
	if !summary.Truncated && len(summary.Omitted) == 0 {
		return ""
	}
	total := summary.Total
	if total == 0 {
		total = summary.Files + len(d.Ignored) + len(summary.Omitted)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "diff collection omitted %d of %d changed file(s): %d included in the materials snapshot, %d ignored by rules",
		len(summary.Omitted), total, summary.Files, len(d.Ignored))
	if len(summary.Omitted) == 0 {
		b.Reset()
		fmt.Fprintf(&b, "diff collection is truncated: %d changed file(s), %d included in the materials snapshot, %d ignored by rules; omitted paths were not recorded in this older snapshot, so the unreviewed scope is not fully known",
			total, summary.Files, len(d.Ignored))
		return b.String()
	}
	reasons := make(map[string]int, len(summary.Omitted))
	paths := make([]string, 0, len(summary.Omitted))
	for _, omitted := range summary.Omitted {
		reasons[omitted.Reason]++
		paths = append(paths, omitted.Path)
	}
	names := make([]string, 0, len(reasons))
	for reason := range reasons {
		names = append(names, reason)
	}
	sort.Strings(names)
	counts := make([]string, 0, len(names))
	for _, reason := range names {
		counts = append(counts, fmt.Sprintf("%s=%d", reason, reasons[reason]))
	}
	b.WriteString("; reasons: ")
	b.WriteString(strings.Join(counts, ", "))
	b.WriteString("; omitted paths: ")
	b.WriteString(boundedPathList(paths, 12))
	return b.String()
}

// unassignedCoverageGap reports included files that the planner never assigned
// to a review node. Generated files are not part of the review plan input.
func unassignedCoverageGap(d *Dossier, assigned map[string]struct{}) string {
	if d == nil || len(assigned) == 0 {
		return ""
	}
	reviewable := 0
	missing := make([]string, 0)
	for _, file := range d.Files {
		if file.Generated {
			continue
		}
		reviewable++
		if _, ok := assigned[file.Path]; !ok {
			missing = append(missing, file.Path)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	sort.Strings(missing)
	return fmt.Sprintf(
		"planner assigned %d of %d reviewable included file(s) to review nodes; %d included file(s) were never assigned: %s",
		reviewable-len(missing), reviewable, len(missing), boundedPathList(missing, 12))
}

func boundedPathList(paths []string, max int) string {
	if len(paths) == 0 {
		return ""
	}
	if max <= 0 || max > len(paths) {
		max = len(paths)
	}
	shown := make([]string, 0, max)
	for _, filePath := range paths[:max] {
		shown = append(shown, truncateRunes(filePath, 200))
	}
	out := strings.Join(shown, ", ")
	if len(paths) > max {
		out += fmt.Sprintf(", ... (+%d more)", len(paths)-max)
	}
	return out
}

// promptDiffGaps mirrors renderDiffs coverage: a missing file, a per-file
// truncation, an empty diff, or a package total truncation means the model did
// not see the full changed scope. Ignore rules are not gaps.
func promptDiffGaps(d *Dossier, files []string) []string {
	if d == nil || len(files) == 0 {
		return nil
	}
	byPath := make(map[string]FileChange, len(d.Files))
	for _, file := range d.Files {
		byPath[file.Path] = file
	}
	var gaps []string
	total := 0
	for i, filePath := range files {
		file, ok := byPath[filePath]
		if !ok {
			gaps = append(gaps, fmt.Sprintf(
				"file %s is not in the materials snapshot; it was not reviewed", filePath))
			continue
		}
		diff := file.Diff
		if utf8.RuneCountInString(diff) > maxFileDiffRunes {
			gaps = append(gaps, fmt.Sprintf(
				"diff for %s was truncated at %d characters before review", filePath, maxFileDiffRunes))
		}
		diff = truncateRunes(diff, maxFileDiffRunes)
		if strings.TrimSpace(diff) == "" {
			gaps = append(gaps, fmt.Sprintf(
				"file %s has no diff content in the materials snapshot; it was not reviewed", filePath))
			continue
		}
		if total+len(diff) > maxTaskFileRunes {
			omitted := files[i:]
			gaps = append(gaps, fmt.Sprintf(
				"package diff reached the %d-byte prompt limit at %s; %d file(s) were omitted: %s",
				maxTaskFileRunes, filePath, len(omitted), boundedPathList(omitted, 20)))
			break
		}
		total += len(diff)
	}
	return gaps
}
