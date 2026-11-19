package review

import (
	"fmt"
	"regexp"
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
// so it is reported apart from the evidence gaps that share its kind. Any other
// signal keeps its kind, so an unknown kind is passed through, never invented.
func UnresolvedCategory(signal UnresolvedSignal) string {
	if strings.Contains(signal.Message, toolGapMarker) {
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
	return b.String(), true
}

// warningNodeRE matches the "<node>: " prefix that applyToolGaps and
// applyUnresolved put before a warning. A node id starts with a letter or a
// digit and holds no space, so the words before a colon and a file name such as
// ".gitlab-review.yml" are not read as nodes.
var warningNodeRE = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]{0,127}): (.+)$`)

// ConsoleWarnings renders the synthesize warnings as Chinese lines for the
// console. A warning with a node prefix is read as a node and a message; the
// message is worded like an unresolved signal, so one without a template keeps
// its original text. Warnings with the same text become one line that lists
// their nodes, "<text>（<node> · <node>）". A text that an unresolved signal
// already shows is dropped, so the panel states a cause once. The result is
// never nil: an empty one means every warning repeated an unresolved signal.
func ConsoleWarnings(warnings []string, unresolved []UnresolvedSignal) []string {
	shown := make(map[string]struct{}, len(unresolved))
	for _, signal := range unresolved {
		shown[UnresolvedText(signal)] = struct{}{}
	}
	type line struct {
		text  string
		nodes []string
	}
	lines := make([]*line, 0, len(warnings))
	byText := make(map[string]*line, len(warnings))
	for _, warning := range warnings {
		node, message := "", strings.TrimSpace(warning)
		if match := warningNodeRE.FindStringSubmatch(message); match != nil {
			node, message = match[1], match[2]
		}
		if message == "" {
			continue
		}
		text := UnresolvedText(UnresolvedSignal{Message: message})
		if _, repeated := shown[text]; repeated {
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
