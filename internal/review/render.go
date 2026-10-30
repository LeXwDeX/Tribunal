package review

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const SummaryMarkerPrefix = "<!-- gitlab-ce-review:summary"
const FindingMarkerPrefix = "<!-- gitlab-ce-review:finding"

var markerFingerprintRE = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
var publishKeyRE = regexp.MustCompile(`^pk_[0-9a-f]{32}$`)
var wwwAutolinkRE = regexp.MustCompile(`(?i)www\.`)
var commitReferenceRE = regexp.MustCompile(`[0-9a-fA-F]{7,}`)
var numberedReferenceRE = regexp.MustCompile(`&#[0-9]+;|[#!$][0-9]`)
var gitLabIssueReferenceRE = regexp.MustCompile(`GL-([0-9])`)
var urlSchemeRE = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://`)
var tableDelimiterRE = regexp.MustCompile(`^[ \t]*\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$`)
var tableInterruptRE = regexp.MustCompile(`^[ \t]*(?:#{1,6}[ \t]+|(?:[-+*]|[0-9]{1,9}[.)])[ \t]+)`)

const (
	summaryItemBodyRunes       = 2000
	summaryItemSuggestionRunes = 1000
	truncationNotice           = "\n\n…（内容已截断）"
)

// SummaryInput is structured data for the summary note.
type SummaryInput struct {
	Verdict              string
	Summary              string
	Groups               []Group
	Fingerprint          string
	HeadSHA              string
	Profile              string
	Duration             time.Duration
	DetailURL            string
	ShowModelAttribution bool
	Warnings             []string
	IncompleteReason     string
}

// FindingRenderOptions controls optional attribution in an inline discussion.
type FindingRenderOptions struct {
	ShowModelAttribution bool
}

// SummaryMarkerLine returns the hidden first line of the summary note.
func SummaryMarkerLine(fingerprint string) (string, error) {
	if !markerFingerprintRE.MatchString(fingerprint) {
		return "", invalid("fingerprint", "must be a safe token of at most 128 characters")
	}
	return fmt.Sprintf("%s v1 fp=%s -->", SummaryMarkerPrefix, fingerprint), nil
}

// RenderSummary renders a summary note using structured fields. Model text is
// treated as untrusted Markdown: structure survives, while raw HTML, link
// syntax and hidden markers are neutralized. The first line is always the
// official hidden marker.
func RenderSummary(in SummaryInput) (string, error) {
	switch in.Verdict {
	case VerdictPass, VerdictBlocking, VerdictIncomplete:
	default:
		return "", invalid("verdict", "must be PASS, BLOCKING, or INCOMPLETE")
	}
	marker, err := SummaryMarkerLine(in.Fingerprint)
	if err != nil {
		return "", err
	}
	summary := strings.TrimSpace(strings.ReplaceAll(in.Summary, "\r\n", "\n"))
	if in.Verdict != VerdictIncomplete && summary == "" {
		return "", invalid("summary", "must not be empty")
	}

	counts := map[Severity]int{SeverityP0: 0, SeverityP1: 0, SeverityP2: 0, SeverityInfo: 0}
	positioned := make([]Group, 0)
	marked := make([]Group, 0)
	unpositioned := make([]Group, 0)
	for _, group := range in.Groups {
		if !group.Published() {
			continue
		}
		severity := group.EffectiveSeverity()
		if _, ok := counts[severity]; ok {
			counts[severity]++
		}
		if group.Decision.Action == ActionMark {
			marked = append(marked, group)
			continue
		}
		if !group.PositionOK {
			unpositioned = append(unpositioned, group)
			continue
		}
		positioned = append(positioned, group)
	}

	var b strings.Builder
	b.WriteString(marker)
	b.WriteByte('\n')
	b.WriteString("### AI 评审：")
	b.WriteString(verdictLabel(in.Verdict))
	b.WriteString("\n\n")
	if line := severityCountLine(counts); line != "" {
		b.WriteString(line)
		b.WriteString("\n\n")
	}
	if summary != "" {
		b.WriteString(renderMarkdown(summary, MaxSummaryRunes))
		b.WriteString("\n\n")
	}
	if len(positioned) > 0 {
		fmt.Fprintf(&b, "\n### 行内问题（%d）\n\n", len(positioned))
		for _, group := range positioned {
			b.WriteString("- **[")
			b.WriteString(string(group.EffectiveSeverity()))
			b.WriteString("]** ")
			b.WriteString(escapeOneLine(group.Title, MaxTitleRunes))
			if group.Path != "" {
				b.WriteString(" · ")
				b.WriteString(inlineCode(group.Path))
				if group.Line > 0 {
					fmt.Fprintf(&b, ":%d", group.Line)
				}
			}
			b.WriteByte('\n')
		}
	}
	if len(unpositioned) > 0 {
		fmt.Fprintf(&b, "\n### 无法定位到行的问题（%d）\n\n", len(unpositioned))
		for i, group := range unpositioned {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(renderSummaryItem(group, false))
		}
	}
	if len(marked) > 0 {
		fmt.Fprintf(&b, "\n### 存疑条目（%d）\n\n", len(marked))
		for i, group := range marked {
			if i > 0 {
				b.WriteByte('\n')
			}
			b.WriteString(renderSummaryItem(group, true))
		}
	}
	if len(in.Warnings) > 0 {
		fmt.Fprintf(&b, "\n### 评审提示（%d）\n\n", len(in.Warnings))
		for _, warning := range in.Warnings {
			b.WriteString("- ")
			b.WriteString(escapeOneLine(warning, 500))
			b.WriteByte('\n')
		}
	}
	if in.Verdict == VerdictIncomplete && strings.TrimSpace(in.IncompleteReason) != "" {
		b.WriteString("\n> 评审未完成：")
		b.WriteString(escapeOneLine(in.IncompleteReason, 500))
		b.WriteByte('\n')
	}

	footer := make([]string, 0, 4)
	if in.HeadSHA != "" && markerFingerprintRE.MatchString(in.HeadSHA) {
		footer = append(footer, "head "+escapeOneLine(in.HeadSHA, 128))
	}
	if in.Profile != "" && markerFingerprintRE.MatchString(in.Profile) {
		footer = append(footer, "profile "+escapeOneLine(in.Profile, 128))
	}
	if in.Duration > 0 {
		footer = append(footer, "用时 "+in.Duration.Round(time.Second).String())
	}
	if link := safeDetailURL(in.DetailURL); link != "" {
		footer = append(footer, fmt.Sprintf(`<a href="%s">详情</a>`, link))
	}
	if len(footer) > 0 {
		b.WriteString("\n<sub>")
		b.WriteString(strings.Join(footer, " · "))
		b.WriteString("</sub>\n")
	}
	return b.String(), nil
}

func verdictLabel(verdict string) string {
	switch verdict {
	case VerdictPass:
		return "🟢 PASS"
	case VerdictBlocking:
		return "🔴 BLOCKING"
	default:
		return "⚪ 评审未完成"
	}
}

// severityCountLine renders non-zero counts on one line, for example
// "**P1** × 2 · **P2** × 1". It is empty when nothing was published.
func severityCountLine(counts map[Severity]int) string {
	parts := make([]string, 0, 4)
	for _, severity := range []Severity{SeverityP0, SeverityP1, SeverityP2, SeverityInfo} {
		if counts[severity] > 0 {
			parts = append(parts, fmt.Sprintf("**%s** × %d", severity, counts[severity]))
		}
	}
	return strings.Join(parts, " · ")
}

func renderSummaryItem(group Group, marked bool) string {
	severity := group.EffectiveSeverity()
	display := string(severity)
	if marked {
		display += "?"
	}
	var b strings.Builder
	b.WriteString("#### [")
	b.WriteString(display)
	b.WriteString("] ")
	b.WriteString(escapeOneLine(group.Title, MaxTitleRunes))
	b.WriteString("\n\n")
	if group.Path != "" {
		b.WriteString(inlineCode(group.Path))
		if group.Line > 0 {
			fmt.Fprintf(&b, ":%d", group.Line)
		}
		b.WriteString("\n\n")
	}
	if marked {
		reason := "挑战结果：UNCERTAIN"
		if group.Challenge != nil && group.Challenge.Reason != "" {
			reason = "挑战结果：" + oneLine(group.Challenge.Reason, 300)
		}
		if group.Decision != nil && group.Decision.Reason != "" {
			reason += "；" + oneLine(group.Decision.Reason, 300)
		}
		b.WriteString("**标记原因**：")
		b.WriteString(escapeOneLine(reason, 600))
		b.WriteString("\n\n")
	}
	if body := renderMarkdown(group.Body, summaryItemBodyRunes); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	if suggestion := renderSuggestion(group.Suggestion, summaryItemSuggestionRunes); suggestion != "" {
		b.WriteString(suggestion)
		b.WriteString("\n\n")
	}
	// A reused inline discussion may have been published before this was marked.
	if marked && strings.TrimSpace(group.Evidence) != "" {
		b.WriteString("<details><summary>证据</summary>\n\n")
		b.WriteString(codeFence(group.Evidence))
		b.WriteString("\n\n</details>\n\n")
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

// renderSuggestion shows a fenced block, such as a GitLab suggestion, as is.
// A prose suggestion gets a short label on the same line.
func renderSuggestion(value string, max int) string {
	rendered := renderMarkdown(value, max)
	if rendered == "" {
		return ""
	}
	if _, _, ok := opensFence(strings.SplitN(rendered, "\n", 2)[0]); ok {
		return rendered
	}
	return "**建议**：" + rendered
}

// RenderFinding renders one inline discussion. The first line is a stable
// hidden key derived from path, normalized title, and snippet.
func RenderFinding(group Group, opts FindingRenderOptions) (string, error) {
	if !group.Published() {
		return "", invalid("decision", "only publish or mark groups can be rendered inline")
	}
	if !group.PositionOK {
		return "", invalid("position_ok", "finding has no diff hunk and must be published in the summary only")
	}
	key := group.PublishKey
	if key == "" {
		key = PublishKeyFor(group.Path, group.Title, group.Evidence)
	}
	if !publishKeyRE.MatchString(key) {
		return "", invalid("publish_key", "must be a stable pk_ key")
	}
	severity := group.EffectiveSeverity()

	var b strings.Builder
	fmt.Fprintf(&b, "%s key=%s -->\n", FindingMarkerPrefix, key)
	b.WriteString("**[")
	b.WriteString(string(severity))
	if group.Decision.Action == ActionMark {
		b.WriteString(" · 存疑")
	}
	b.WriteString("] ")
	b.WriteString(escapeOneLine(group.Title, MaxTitleRunes))
	b.WriteString("**\n")
	// The discussion is anchored on the line, so the location is not repeated.
	if body := renderMarkdown(group.Body, MaxBodyRunes); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteByte('\n')
	}
	if group.Decision.Action == ActionMark && strings.TrimSpace(group.Decision.Reason) != "" {
		b.WriteString("\n**存疑原因**：")
		b.WriteString(escapeOneLine(group.Decision.Reason, 500))
		b.WriteByte('\n')
	}
	if suggestion := renderSuggestion(group.Suggestion, MaxSuggestionRunes); suggestion != "" {
		b.WriteString("\n")
		b.WriteString(suggestion)
		b.WriteByte('\n')
	}
	// Evidence stays only on uncertain findings, where the reader must judge.
	if group.Decision.Action == ActionMark && strings.TrimSpace(group.Evidence) != "" {
		b.WriteString("\n<details><summary>证据</summary>\n\n")
		b.WriteString(codeFence(group.Evidence))
		b.WriteString("\n\n</details>\n")
	}
	if opts.ShowModelAttribution {
		if attribution := modelAttribution(group); attribution != "" {
			b.WriteString("\n<sub>来源：")
			b.WriteString(attribution)
			b.WriteString("</sub>\n")
		}
	}
	return b.String(), nil
}

func modelAttribution(group Group) string {
	seen := make(map[string]struct{})
	values := make([]string, 0, 3)
	for _, source := range group.Sources {
		name := strings.TrimSpace(source.Origin.Provider)
		if source.Origin.Model != "" {
			if name != "" {
				name += "/" + source.Origin.Model
			} else {
				name = source.Origin.Model
			}
		}
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		values = append(values, escapeOneLine(name, 128))
	}
	return strings.Join(values, "、")
}

func oneLine(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	return truncateRunes(value, max)
}

func escapeOneLine(value string, max int) string {
	return escapeText(oneLine(value, max), max)
}

// escapeText renders untrusted single-line text literally. Backslashes are
// doubled before escaping punctuation, and empty spans break references that
// GitLab resolves after decoding Markdown entities.
func escapeText(value string, max int) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = truncateRunes(value, max)
	value = strings.NewReplacer(
		`\`, `\\`,
		`@`, `\@`,
		`#`, `\#`,
		`!`, `\!`,
		`:`, `\:`,
		`$`, `\$`,
		`%`, `\%`,
		`~`, `\~`,
		`*`, `\*`,
	).Replace(value)
	runes := []rune(value)
	var b strings.Builder
	for i, r := range runes {
		if r == '_' && (i == 0 || i == len(runes)-1 || !wordRune(runes[i-1]) || !wordRune(runes[i+1])) {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	value = b.String()
	// Only the dot that starts GFM's www autolink needs protection. Escaping
	// every dot would change ordinary file names in raw Markdown.
	value = wwwAutolinkRE.ReplaceAllStringFunc(value, func(match string) string {
		return match[:len(match)-1] + `\.`
	})
	value = html.EscapeString(value)
	// GitLab resolves commit and project references after Markdown entities are
	// decoded. Empty spans split text nodes without changing their visible text.
	value = breakCommitReferences(value)
	value = gitLabIssueReferenceRE.ReplaceAllString(value, "GL-<span></span>$1")
	return strings.NewReplacer(
		"&amp;", `\&`,
		"&gt;", "<span></span>&gt;",
		"[", "&#91;",
		"]", "&#93;",
		"`", "&#96;",
	).Replace(value)
}

func wordRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

func breakCommitReferences(value string) string {
	return commitReferenceRE.ReplaceAllStringFunc(value, func(match string) string {
		var b strings.Builder
		for len(match) > 6 {
			b.WriteString(match[:6])
			b.WriteString("<span></span>")
			match = match[6:]
		}
		b.WriteString(match)
		return b.String()
	})
}

// renderMarkdown preserves paragraphs, headings and code while neutralizing
// references and raw HTML outside code. Indented fences move to the root so a
// surrounding list cannot close them before untrusted code has ended.
func renderMarkdown(value string, max int) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r", "\n"))
	if value == "" {
		return ""
	}
	truncated := false
	if utf8.RuneCountInString(value) > max {
		value = truncateRunes(value, max)
		truncated = true
	}

	var b strings.Builder
	var fenceChar byte
	fenceLen := 0
	fenceIndent := 0
	var outputFence string
	inTable := false
	lines := strings.Split(value, "\n")
	for lineIndex, line := range lines {
		if fenceChar != 0 {
			if closesFence(line, fenceChar, fenceLen) {
				if outputFence != "" {
					b.WriteString(outputFence)
				} else {
					b.WriteString(line)
				}
				fenceChar = 0
				fenceLen = 0
				outputFence = ""
			} else {
				for i := 0; i < fenceIndent && strings.HasPrefix(line, " "); i++ {
					line = line[1:]
				}
				b.WriteString(line)
			}
			b.WriteByte('\n')
			continue
		}
		if ch, n, ok := opensFence(line); ok {
			inTable = false
			fenceChar, fenceLen = ch, n
			fenceIndent = len(line) - len(strings.TrimLeft(line, " "))
			if fenceIndent > 0 {
				// A list can end an indented fence early. Emit it at the root
				// with a delimiter absent from the untrusted fence content.
				longest := n
				for _, content := range lines[lineIndex+1:] {
					if closesFence(content, ch, n) {
						break
					}
					run := 0
					for i := 0; i < len(content); i++ {
						if content[i] == ch {
							run++
							if run > longest {
								longest = run
							}
						} else {
							run = 0
						}
					}
				}
				outputFence = strings.Repeat(string(ch), longest+1)
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(outputFence)
				b.WriteString(line[fenceIndent+n:])
			} else {
				b.WriteString(line)
			}
			b.WriteByte('\n')
			continue
		}
		// Escaping a commit-like list number can stop it interrupting a table.
		if strings.TrimSpace(line) == "" || (inTable && !tableDelimiterRE.MatchString(line) && tableInterruptRE.MatchString(renderMarkdownLine(line))) {
			inTable = false
		}
		if lineIndex+1 < len(lines) && strings.ContainsRune(lines[lineIndex+1], '|') && tableDelimiterRE.MatchString(lines[lineIndex+1]) {
			inTable = true
		}
		if inTable {
			b.WriteString(renderMarkdownTableLine(line))
		} else {
			b.WriteString(renderMarkdownLine(line))
		}
		b.WriteByte('\n')
	}
	if fenceChar != 0 {
		if outputFence != "" {
			b.WriteString(outputFence)
		} else {
			b.WriteString(strings.Repeat(string(fenceChar), fenceLen))
		}
		b.WriteByte('\n')
	}
	out := strings.TrimRight(b.String(), "\n")
	if truncated {
		out += truncationNotice
	}
	return out
}

// GitLab splits table cells before parsing code spans. Escaped pipes stay in
// their cell, even when another backslash precedes the escape.
func renderMarkdownTableLine(line string) string {
	var b strings.Builder
	start := 0
	for i := 0; i < len(line); i++ {
		if line[i] != '|' || (i > 0 && line[i-1] == '\\') {
			continue
		}
		b.WriteString(renderMarkdownLine(line[start:i]))
		b.WriteByte('|')
		start = i + 1
	}
	b.WriteString(renderMarkdownLine(line[start:]))
	return b.String()
}

func renderMarkdownLine(line string) string {
	var b strings.Builder
	for i := 0; i < len(line); {
		if line[i] != '`' {
			next := strings.IndexByte(line[i:], '`')
			if next < 0 {
				next = len(line) - i
			}
			b.WriteString(escapeMarkdownText(line[i : i+next]))
			i += next
			continue
		}
		backslashes := 0
		for j := i - 1; j >= 0 && line[j] == '\\'; j-- {
			backslashes++
		}
		if backslashes%2 != 0 {
			b.WriteByte('`')
			i++
			continue
		}
		n := 0
		for i+n < len(line) && line[i+n] == '`' {
			n++
		}
		closing := -1
		for j := i + n; j < len(line); {
			if line[j] != '`' {
				j++
				continue
			}
			m := 0
			for j+m < len(line) && line[j+m] == '`' {
				m++
			}
			if m == n {
				closing = j
				break
			}
			j += m
		}
		if closing < 0 {
			b.WriteString("&#96;")
			i++
			continue
		}
		b.WriteString(line[i : i+n])
		b.WriteString(line[i+n : closing])
		b.WriteString(line[closing : closing+n])
		i = closing + n
	}
	return b.String()
}

// escapeMarkdownText keeps Markdown structure but neutralizes references and
// raw HTML outside code spans.
func escapeMarkdownText(value string) string {
	value = html.EscapeString(value)
	value = breakCommitReferences(value)
	value = gitLabIssueReferenceRE.ReplaceAllString(value, "GL-<span></span>$1")
	value = numberedReferenceRE.ReplaceAllStringFunc(value, func(match string) string {
		if match[0] == '&' {
			return match
		}
		return match[:1] + "<span></span>" + match[1:]
	})
	value = urlSchemeRE.ReplaceAllStringFunc(value, func(match string) string {
		return match[:len(match)-2] + "<span></span>//"
	})
	value = wwwAutolinkRE.ReplaceAllStringFunc(value, func(match string) string {
		return match[:len(match)-1] + `\.`
	})
	value = strings.NewReplacer(
		"&amp;", "&amp;<span></span>",
		"&gt;", "<span></span>&gt;",
		"@", "@<span></span>",
		"%", "%<span></span>",
		"[", "&#91;",
		"]", "&#93;",
	).Replace(value)
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		b.WriteByte(value[i])
		// A trailing tilde in any run still starts a label; separate it from
		// the following text. Strikethrough delimiters remain valid around it.
		if value[i] == '~' && (i+1 == len(value) || value[i+1] != '~') {
			b.WriteString("<span></span>")
		}
	}
	return b.String()
}

func opensFence(line string) (byte, int, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 {
		return 0, 0, false
	}
	ch := trimmed[0]
	if ch != '`' && ch != '~' {
		return 0, 0, false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	if n < 3 {
		return 0, 0, false
	}
	if ch == '`' && strings.ContainsRune(trimmed[n:], '`') {
		return 0, 0, false
	}
	return ch, n, true
}

func closesFence(line string, ch byte, min int) bool {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return false
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == ch {
		n++
	}
	return n >= min && strings.Trim(trimmed[n:], " \t") == ""
}

func inlineCode(value string) string {
	value = truncateRunes(strings.NewReplacer("\r", " ", "\n", " ").Replace(value), MaxPathBytes)
	fence := "`"
	for strings.Contains(value, fence) {
		fence += "`"
	}
	if len(fence) > 1 || (strings.HasPrefix(value, " ") && strings.HasSuffix(value, " ") && strings.Trim(value, " ") != "") {
		return fence + " " + value + " " + fence
	}
	return fence + value + fence
}

func codeFence(value string) string {
	value = truncateRunes(value, MaxEvidenceRunes)
	fence := "```"
	for strings.Contains(value, fence) {
		fence += "`"
	}
	return fence + "\n" + value + "\n" + fence
}

func safeDetailURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 2048 || strings.ContainsAny(raw, "\r\n\x00") {
		return ""
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return ""
	}
	return html.EscapeString(raw)
}
