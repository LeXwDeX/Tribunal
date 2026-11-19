package review

import (
	"fmt"
	"slices"
	"strings"
)

// The console shows unresolved signals and the incomplete summary as Chinese
// text. These helpers add that reading next to the stored English signal; the
// stored text is never rewritten.

// UnresolvedCategoryTool is the console category of a gap caused by an
// unavailable MCP tool. Every other signal uses its kind as the category.
const UnresolvedCategoryTool = "tool_unavailable"

// UnresolvedCategory classifies a signal for the console. A tool gap is a
// missing capability of the review run, not missing evidence about the change,
// so it is reported apart from the evidence gaps that share its kind. Only an
// evidence gap with the shape the executor writes is a tool gap: a model gap
// that mentions the phrase keeps its kind. Any other signal keeps its kind, so
// an unknown kind is passed through, never invented.
func UnresolvedCategory(signal UnresolvedSignal) string {
	if signal.Kind == UnresolvedEvidence && isToolGap(signal.Message) {
		return UnresolvedCategoryTool
	}
	return signal.Kind
}

// UnresolvedText renders one signal as Chinese for the console. A message that
// no template maps keeps its original text, so the console never replaces the
// only explanation of a gap with a generic sentence.
func UnresolvedText(signal UnresolvedSignal) string {
	if text, ok := localizeMachineMessage(signal.Message); ok {
		return text
	}
	return signal.Message
}

// IncompleteSummaryText renders the stored incomplete summary as one plain
// Chinese paragraph for the console: the stored lead and the distinct causes,
// without Markdown, node ids or the published note's next-step line. It reports
// false for any other summary.
func IncompleteSummaryText(summary string) (string, bool) {
	reasons, extra, ok := parseIncompleteSummary(summary)
	if !ok {
		return "", false
	}
	return renderIncompleteSummaryText(reasons, extra), true
}

// IncompleteSummaryTextFor is IncompleteSummaryText with the structured
// signals of the same workflow. When signals exist, the causes and the
// overflow count come from them, grouped like the console rows (category plus
// Chinese text), instead of from the stored summary: a summary stored before
// #603 counted every per-tool gap, so its "另有 N 项" overstated the distinct
// causes. Without signals the stored summary is parsed as before.
func IncompleteSummaryTextFor(summary string, unresolved []UnresolvedSignal) (string, bool) {
	if len(unresolved) == 0 {
		return IncompleteSummaryText(summary)
	}
	if _, _, ok := parseIncompleteSummary(summary); !ok {
		return "", false
	}
	type cause struct{ category, text string }
	seen := make(map[cause]struct{}, len(unresolved))
	reasons := make([]string, 0, maxUnresolvedSummaryItems)
	groups := 0
	for _, signal := range unresolved {
		key := cause{UnresolvedCategory(signal), UnresolvedText(signal)}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		groups++
		if len(reasons) < maxUnresolvedSummaryItems {
			reasons = append(reasons, key.text)
		}
	}
	return renderIncompleteSummaryText(reasons, groups-len(reasons)), true
}

// renderIncompleteSummaryText joins the stored lead, the causes and the
// overflow count into one plain paragraph.
func renderIncompleteSummaryText(reasons []string, extra int) string {
	var b strings.Builder
	b.WriteString(unresolvedSummaryPrefix)
	for i, reason := range reasons {
		if i > 0 {
			b.WriteString("；")
		}
		b.WriteString(strings.TrimSuffix(reason, "。"))
	}
	b.WriteString("。")
	if extra > 0 {
		fmt.Fprintf(&b, "另有 %d 项检查未完成。", extra)
	}
	return b.String()
}

// signalPseudoNodes are the node names code-owned signals use for checks that
// are not DAG nodes, so a warning that names one still reads as a node.
var signalPseudoNodes = []string{"coverage", "provenance", "budget"}

// ConsoleWarnings renders the synthesize warnings as Chinese lines for the
// console. applyToolGaps and applyUnresolved put "<node>: " in front of a
// warning; the prefix is read as a node only when it names a node of the
// workflow (nodeIDs), a node of an unresolved signal or a code-owned check, so
// any other word before a colon stays in the text. The message is worded like an
// unresolved signal: one without a template keeps its original text. Warnings
// with the same text become one line that lists their nodes,
// "<text>（<node> · <node>）". A warning is dropped when an unresolved signal
// already shows its text for its node, so the panel states a cause once. The
// result is never nil: an empty one means every warning repeated an unresolved
// signal.
func ConsoleWarnings(warnings []string, unresolved []UnresolvedSignal, nodeIDs []string) []string {
	known := make(map[string]struct{}, len(nodeIDs)+len(signalPseudoNodes)+len(unresolved))
	for _, id := range nodeIDs {
		known[id] = struct{}{}
	}
	for _, id := range signalPseudoNodes {
		known[id] = struct{}{}
	}
	// shown maps the text of each unresolved group to the nodes it lists.
	shown := make(map[string]map[string]struct{}, len(unresolved))
	for _, signal := range unresolved {
		if signal.Node == "" {
			continue
		}
		known[signal.Node] = struct{}{}
		text := UnresolvedText(signal)
		if shown[text] == nil {
			shown[text] = make(map[string]struct{})
		}
		shown[text][signal.Node] = struct{}{}
	}
	type line struct {
		text  string
		nodes []string
	}
	lines := make([]*line, 0, len(warnings))
	byText := make(map[string]*line, len(warnings))
	for _, warning := range warnings {
		node, message := "", strings.TrimSpace(warning)
		if prefix, rest, ok := strings.Cut(message, ": "); ok && strings.TrimSpace(rest) != "" {
			if _, isNode := known[prefix]; isNode {
				node, message = prefix, strings.TrimSpace(rest)
			}
		}
		if message == "" {
			continue
		}
		text, mapped := localizeMachineMessage(message)
		if !mapped {
			text = message
		}
		if _, repeated := shown[text][node]; node != "" && repeated {
			continue
		}
		entry, ok := byText[text]
		if !ok {
			entry = &line{text: text}
			byText[text] = entry
			lines = append(lines, entry)
		}
		if node != "" && !slices.Contains(entry.nodes, node) {
			entry.nodes = append(entry.nodes, node)
		}
	}
	out := make([]string, 0, len(lines))
	for _, entry := range lines {
		if len(entry.nodes) == 0 {
			out = append(out, entry.text)
			continue
		}
		out = append(out, entry.text+"（"+strings.Join(entry.nodes, " · ")+"）")
	}
	return out
}
