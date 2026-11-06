package review

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// coverageUnresolved turns material and prompt truncation into hard evidence
// gaps. The app layer owns collection and sets DiffSummary.Truncated for
// capacity, too-large, or collapsed diffs. Prompt truncation is deterministic
// here: every review node is rebuilt through BuildPrompt with an explicit diff
// budget, so a diff that the real prompt omits or cuts is a gap even when the
// raw diff was rendered. Both code and agent synthesis cannot clear these
// signals. A truncated MR description or a bounded context section is
// disclosed as a warning; the budget keeps the diff complete.
func (e *Executor) coverageUnresolved(wf dag.WorkflowView) ([]UnresolvedSignal, []string) {
	if e == nil || e.deps.Dossier == nil {
		return nil, nil
	}
	d := e.deps.Dossier
	var signals []UnresolvedSignal
	var warnings []string
	if message := collectionCoverageGap(d); message != "" {
		signals = append(signals, UnresolvedSignal{
			Node: "coverage", Kind: UnresolvedEvidence, Message: message,
		})
	}
	if warning := contentFreeWarning(d); warning != "" {
		warnings = append(warnings, warning)
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
		case KindCodeReview, KindReviewStd, KindReviewInt, KindReview:
		default:
			continue
		}
		params, err := decodeParams[reviewNodeParams](node.Spec.Params)
		if err != nil {
			continue
		}
		gaps, locallyGapped := promptDiffGaps(d, params.Files)
		builtGaps, builtWarnings := e.builtPromptDiffGaps(d, node.Spec.Kind, params, locallyGapped, node.Output)
		gaps = append(gaps, builtGaps...)
		warnings = mergeWarnings(warnings, builtWarnings)
		for _, gap := range gaps {
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
	return signals, warnings
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

// isContentFree reports an added, deleted or renamed file whose diff is empty:
// there is no textual content to review (an empty file or a pure rename).
// GitLab reports binary files with a non-empty "Binary files ... differ" diff,
// so they never reach this rule. A modified file with an empty diff stays a
// coverage gap: the client does not read a_mode/b_mode, so a mode-only change
// cannot be told apart from a withheld diff.
func isContentFree(file FileChange) bool {
	if strings.TrimSpace(file.Diff) != "" {
		return false
	}
	switch file.Status {
	case "added", "deleted", "renamed":
		return true
	}
	return false
}

// contentFreeWarning lists content-free files so the omission stays visible.
func contentFreeWarning(d *Dossier) string {
	if d == nil {
		return ""
	}
	var paths []string
	for _, file := range d.Files {
		if isContentFree(file) {
			paths = append(paths, file.Path)
		}
	}
	if len(paths) == 0 {
		return ""
	}
	sort.Strings(paths)
	return "no textual changes to review: " + boundedPathList(paths, 12)
}

// promptDiffGaps reports materials that cannot be reviewed at all: a file
// missing from the snapshot or a file with no diff content. Prompt inline
// truncation is no longer a gap by itself; builtPromptDiffGaps verifies that
// every hunk of an omitted file was read through read_diff.
func promptDiffGaps(d *Dossier, files []string) ([]string, map[string]struct{}) {
	if d == nil || len(files) == 0 {
		return nil, nil
	}
	byPath := make(map[string]FileChange, len(d.Files))
	for _, file := range d.Files {
		byPath[file.Path] = file
	}
	var gaps []string
	gapped := make(map[string]struct{})
	for _, filePath := range files {
		file, ok := byPath[filePath]
		if !ok {
			gaps = append(gaps, fmt.Sprintf(
				"file %s is not in the materials snapshot; it was not reviewed", filePath))
			gapped[filePath] = struct{}{}
			continue
		}
		if isContentFree(file) {
			continue
		}
		if strings.TrimSpace(file.Diff) == "" {
			gaps = append(gaps, fmt.Sprintf(
				"file %s has no diff content in the materials snapshot; it was not reviewed", filePath))
			gapped[filePath] = struct{}{}
		}
	}
	return gaps, gapped
}

// promptCoverageNonce makes the rebuilt prompt deterministic. BuildPrompt
// returns the same nonce, so neutralization is reproduced exactly.
const promptCoverageNonce = "prompt_coverage_nonce"

// builtPromptDiffGaps rebuilds the node task and judges coverage from where the
// renderer placed each complete diff block, not from a search over untrusted
// text. A block counts only when its recorded end lies inside the task that
// survives MaxTaskRunes truncation. A file omitted from the inline budget
// counts only when the node output records every hunk as read through
// read_diff. The whole user prompt is also checked against the deterministic
// rebuild, so a change in prompt framing fails closed. Files already reported
// by promptDiffGaps are skipped to avoid duplicate signals.
func (e *Executor) builtPromptDiffGaps(d *Dossier, kind string, params reviewNodeParams, locallyGapped map[string]struct{}, output json.RawMessage) ([]string, []string) {
	if d == nil || len(params.Files) == 0 {
		return nil, nil
	}
	role, ok := roleForKind(kind)
	if !ok {
		return nil, nil
	}
	layout, ok := reviewTaskLayoutFor(kind, d, params)
	if !ok {
		return nil, nil
	}
	built, err := BuildPrompt(PromptInput{
		Bundle: e.prompts, Role: role, Task: layout.task, Nonce: promptCoverageNonce,
	})
	if err != nil {
		return []string{fmt.Sprintf("could not verify the built review prompt: %v", err)}, nil
	}
	if built.User != renderTaskSection(layout.task, built.Nonce) {
		return []string{"could not verify the built review prompt: deterministic rebuild differs"}, nil
	}
	visible := utf8.RuneCountInString(layout.task)
	if visible > MaxTaskRunes {
		visible = MaxTaskRunes
	}
	covered := make(map[string]struct{}, len(layout.spans))
	for _, span := range layout.spans {
		if span.end <= visible {
			covered[span.path] = struct{}{}
		}
	}
	var diffReads map[string][]int
	if len(output) > 0 {
		var record struct {
			DiffReads map[string][]int `json:"diff_reads"`
		}
		_ = json.Unmarshal(output, &record)
		diffReads = record.DiffReads
	}
	byPath := make(map[string]FileChange, len(d.Files))
	for _, file := range d.Files {
		byPath[file.Path] = file
	}
	var missing []string
	var unread []string
	reported := make(map[string]struct{}, len(params.Files))
	for _, filePath := range params.Files {
		if _, ok := locallyGapped[filePath]; ok {
			continue
		}
		if _, ok := covered[filePath]; ok {
			continue
		}
		if _, ok := reported[filePath]; ok {
			continue
		}
		reported[filePath] = struct{}{}
		file := byPath[filePath]
		if isContentFree(file) {
			continue
		}
		total := countFileHunks(file.Diff)
		if total > 0 && hunksCovered(total, diffReads[filePath]) {
			continue
		}
		missing = append(missing, filePath)
		if total > 0 {
			unread = append(unread, fmt.Sprintf("%s=%s", filePath, unreadHunks(total, diffReads[filePath])))
		}
	}
	if len(missing) == 0 {
		return nil, layout.warnings
	}
	message := fmt.Sprintf(
		"built prompt did not include the complete diff for %d of %d file(s): %s",
		len(missing), len(params.Files), boundedPathList(missing, 8))
	if len(unread) > 0 {
		message += "; unread hunks: " + strings.Join(unread, ", ")
	}
	return []string{message}, layout.warnings
}

// hunksCovered reports whether every 1-based hunk index appears in reads.
func hunksCovered(total int, reads []int) bool {
	if total == 0 {
		return true
	}
	seen := make(map[int]struct{}, len(reads))
	for _, hunk := range reads {
		seen[hunk] = struct{}{}
	}
	for hunk := 1; hunk <= total; hunk++ {
		if _, ok := seen[hunk]; !ok {
			return false
		}
	}
	return true
}

func unreadHunks(total int, reads []int) string {
	seen := make(map[int]struct{}, len(reads))
	for _, hunk := range reads {
		seen[hunk] = struct{}{}
	}
	var missing []string
	for hunk := 1; hunk <= total; hunk++ {
		if _, ok := seen[hunk]; !ok {
			missing = append(missing, fmt.Sprintf("%d", hunk))
		}
	}
	return strings.Join(missing, ",")
}
