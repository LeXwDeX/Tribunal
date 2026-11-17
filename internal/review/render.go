package review

import (
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
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
	// platformLimitNotice is the only publishing-time clip notice. It is added
	// when a whole note would exceed the GitLab note size limit.
	platformLimitNotice = "\n\n…（已达 GitLab 注记上限，余下内容见控制台）"
)

// briefBody renders a published finding body. The renderer never clips content
// by size; limitPublishedNote bounds the whole note at the GitLab platform
// limit and points the reader at the console.
func briefBody(value string) string {
	value = strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
	if value == "" {
		return ""
	}
	return renderMarkdown(value, maxContentRunes)
}

// SummaryInput is structured data for the summary note.
type SummaryInput struct {
	Verdict string
	Summary string
	// RuleSummary marks Summary as the deterministic rule-mode text, whose
	// published count is rendered as a verdict/inline/summary-only breakdown.
	// Model-written synthesis text is never rewritten.
	RuleSummary          bool
	Groups               []Group
	Fingerprint          string
	HeadSHA              string
	Profile              string
	Duration             time.Duration
	DetailURL            string
	ShowModelAttribution bool
	Warnings             []string
	IncompleteReason     string
	// Scope discloses the structured review coverage of the workflow result.
	Scope *ReviewScope
	// Generated is the number of generated changed paths skipped before
	// review. GeneratedPaths names up to MaxGeneratedPaths of them.
	Generated      int
	GeneratedPaths []string
}

// FindingRenderOptions controls optional attribution in an inline discussion.
type FindingRenderOptions struct {
	ShowModelAttribution bool
	ProviderLabels       map[string]string
	// Generation, when positive, adds the review generation to a position
	// discussion so readers can tell an in-place update from a new comment.
	Generation int64
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
	// Unpositioned near-duplicates of a listed group are referenced under it
	// instead of listed (and counted) again. This is a summary-only view: the
	// inline discussions and stored findings are untouched.
	fold := foldUnpositionedForSummary(in.Groups)
	for _, group := range in.Groups {
		if !group.Published() || fold.folded[group.ID] {
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
			if related := fold.related[group.ID]; len(related) > 0 {
				group.Related = SortRelated(append(append([]RelatedGroup(nil), group.Related...), related...))
			}
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
		if localized, ok := localizeIncompleteSummary(summary, len(positioned), len(unpositioned)+len(marked)); ok && in.Verdict == VerdictIncomplete {
			summary = localized
		} else if in.RuleSummary {
			summary = alignRuleSummaryCounts(summary, len(positioned), len(unpositioned)+len(marked))
		}
		b.WriteString(renderMarkdown(summary, maxContentRunes))
		b.WriteString("\n\n")
	}
	if section := reviewScopeSection(in.Scope, in.Generated, in.GeneratedPaths); section != "" {
		b.WriteString(section)
	}

	// The footer is fixed-shape and small; render it up front so the
	// incremental budget can reserve its bytes instead of cutting it.
	var footer strings.Builder
	footerParts := make([]string, 0, 4)
	if in.HeadSHA != "" && markerFingerprintRE.MatchString(in.HeadSHA) {
		footerParts = append(footerParts, "head "+escapeOneLine(in.HeadSHA, 128))
	}
	if in.Profile != "" && markerFingerprintRE.MatchString(in.Profile) {
		footerParts = append(footerParts, "profile "+escapeOneLine(in.Profile, 128))
	}
	if in.Duration > 0 {
		footerParts = append(footerParts, "用时 "+in.Duration.Round(time.Second).String())
	}
	if link := safeDetailURL(in.DetailURL); link != "" {
		footerParts = append(footerParts, fmt.Sprintf(`<a href="%s">详情</a>`, link))
	}
	if len(footerParts) > 0 {
		footer.WriteString("\n<sub>")
		footer.WriteString(strings.Join(footerParts, " · "))
		footer.WriteString("</sub>\n")
	}

	budget := newNoteBudget(footer.Len())
	budget.b.WriteString(b.String())

	if len(positioned) > 0 {
		budget.writeSection(fmt.Sprintf("\n### 行内问题（%d）\n\n", len(positioned)), len(positioned), func(i int) string {
			group := positioned[i]
			var item strings.Builder
			item.WriteString("- **[")
			item.WriteString(string(group.EffectiveSeverity()))
			item.WriteString("]")
			item.WriteString(escapeOneLine(group.Title, MaxTitleRunes))
			item.WriteString("**")
			if group.Path != "" {
				item.WriteString(" · ")
				item.WriteString(inlineCode(group.Path))
				if group.Line > 0 {
					fmt.Fprintf(&item, ":%d", group.Line)
				}
			}
			item.WriteByte('\n')
			item.WriteString(inlineRelatedLines(fold.related[group.ID]))
			return item.String()
		})
	}
	if len(unpositioned) > 0 {
		budget.writeSection(fmt.Sprintf("\n### 无法定位到行的问题（%d）\n\n", len(unpositioned)), len(unpositioned), func(i int) string {
			item := renderSummaryItem(unpositioned[i], false)
			if i > 0 {
				item = "\n" + item
			}
			return item
		})
	}
	if len(marked) > 0 {
		budget.writeSection(fmt.Sprintf("\n### 存疑条目（%d）\n\n", len(marked)), len(marked), func(i int) string {
			item := renderSummaryItem(marked[i], true)
			if i > 0 {
				item = "\n" + item
			}
			return item
		})
	}
	if len(in.Warnings) > 0 {
		rendered := make([]string, 0, len(in.Warnings))
		seen := make(map[string]bool, len(in.Warnings))
		for _, warning := range in.Warnings {
			localized := escapeOneLine(localizeWarning(warning), 500)
			if localized == "" || seen[localized] {
				continue
			}
			seen[localized] = true
			rendered = append(rendered, localized)
		}
		if len(rendered) > 0 {
			budget.writeSection(fmt.Sprintf("\n### 评审提示（%d）\n\n", len(rendered)), len(rendered), func(i int) string {
				return "- " + rendered[i] + "\n"
			})
		}
	}
	if in.Verdict == VerdictIncomplete && strings.TrimSpace(in.IncompleteReason) != "" {
		budget.write("\n> 评审未完成：" + escapeOneLine(localizeIncompleteReason(in.IncompleteReason), 500) + "\n")
	}

	return limitPublishedNote(gitlab.NeutralizeQuickActions(budget.finish(footer.String()))), nil
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

// reviewScopeSection renders the structured review coverage and the skipped
// generated file count. It is empty when the result has neither.
func reviewScopeSection(scope *ReviewScope, generated int, generatedPaths []string) string {
	if scope == nil && generated <= 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n### 评审范围\n\n")
	if scope != nil {
		if scope.Full {
			b.WriteString("- 全量评审：覆盖本次变更的全部文件。\n")
		} else {
			total := scope.FileCount
			if total <= 0 {
				total = len(scope.Files)
			}
			fmt.Fprintf(&b, "- 增量评审：上次 head %s → 本次 head %s，覆盖 %d 个文件；结论仅覆盖本次增量变更的文件。\n",
				scopeHeadLabel(scope.LastHead), scopeHeadLabel(scope.Head), total)
			if scope.FileCount > maxScopeFiles {
				fmt.Fprintf(&b, "- 范围文件列表超过 %d 条，摘要只保留计数，完整列表见控制台。\n", maxScopeFiles)
			}
		}
		if scope.Unlocated > 0 {
			fmt.Fprintf(&b, "- 其中 %d 条问题无法定位到具体文件，已列入「无法定位到行的问题」。\n", scope.Unlocated)
		}
	}
	if generated > 0 {
		if len(generatedPaths) > 0 {
			fmt.Fprintf(&b, "- 生成文件：跳过 %d 个，未进入评审：%s。\n", generated, generatedPathList(generatedPaths))
		} else {
			fmt.Fprintf(&b, "- 生成文件：跳过 %d 个，未进入评审（路径见评审提示）。\n", generated)
		}
	}
	b.WriteByte('\n')
	return b.String()
}

// generatedPathList renders a bounded line of skipped generated paths.
func generatedPathList(paths []string) string {
	const maxShown = 12
	shown, extra := paths, 0
	if len(shown) > maxShown {
		extra = len(shown) - maxShown
		shown = shown[:maxShown]
	}
	parts := make([]string, 0, len(shown))
	for _, filePath := range shown {
		parts = append(parts, escapeOneLine(filePath, 200))
	}
	out := strings.Join(parts, "、")
	if extra > 0 {
		out += fmt.Sprintf(" 等 %d 个", len(paths))
	}
	return out
}

// scopeHeadLabel renders a scope head only when it looks like an object ID or
// ref token; unexpected values stay unnamed.
func scopeHeadLabel(head string) string {
	head = strings.TrimSpace(head)
	if head == "" || !markerFingerprintRE.MatchString(head) {
		return "未知"
	}
	return escapeOneLine(head, 128)
}

// inlineRelatedLines renders unpositioned groups folded into an inline group
// as indented sub-bullets of its overview row, capped at maxRelatedGroups.
func inlineRelatedLines(related []RelatedGroup) string {
	var b strings.Builder
	for i, r := range related {
		if i == maxRelatedGroups {
			fmt.Fprintf(&b, "  - 相关描述：另有 %d 条\n", len(related)-maxRelatedGroups)
			break
		}
		b.WriteString("  - 相关描述：**[")
		b.WriteString(string(r.Severity))
		b.WriteString("]")
		b.WriteString(escapeOneLine(r.Title, MaxTitleRunes))
		b.WriteString("**")
		b.WriteByte('\n')
	}
	return b.String()
}

var ruleSummaryCountsRE = regexp.MustCompile(`^(规则模式汇总：共 )([0-9]+)( 条问题，已发布 )([0-9]+)( 条。)`)

// alignRuleSummaryCounts replaces the rule-mode published count with an
// explicit breakdown: the verdict published count, how many entries are inline,
// how many are summary-only, and how many published findings were folded into
// 「相关描述」. The synthesis text counts every published group; folding happens
// afterwards, in the publish view, and never changes the stored text or
// findings. Model-written synthesis text is never rewritten.
func alignRuleSummaryCounts(summary string, inline, summaryOnly int) string {
	match := ruleSummaryCountsRE.FindStringSubmatchIndex(summary)
	if match == nil {
		return summary
	}
	published, err := strconv.Atoi(summary[match[8]:match[9]])
	if err != nil {
		return summary
	}
	listed := inline + summaryOnly
	if published < listed {
		published = listed
	}
	folded := published - listed
	var b strings.Builder
	b.WriteString("规则模式汇总：共 ")
	b.WriteString(summary[match[4]:match[5]])
	b.WriteString(" 条问题，裁决发布 ")
	b.WriteString(strconv.Itoa(published))
	b.WriteString(" 条（行内 ")
	b.WriteString(strconv.Itoa(inline))
	b.WriteString(" 条，仅汇总 ")
	b.WriteString(strconv.Itoa(summaryOnly))
	b.WriteString(" 条")
	if folded > 0 {
		b.WriteString("，另有 ")
		b.WriteString(strconv.Itoa(folded))
		b.WriteString(" 条并入相关描述")
	}
	b.WriteString("）。")
	b.WriteString(summary[match[11]:])
	return b.String()
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
	b.WriteString("]")
	b.WriteString(escapeOneLine(group.Title, MaxTitleRunes))
	b.WriteString("\n\n")
	if group.Path != "" {
		b.WriteString(inlineCode(group.Path))
		if group.Line > 0 {
			fmt.Fprintf(&b, ":%d", group.Line)
		}
		switch group.Side {
		case SideOld:
			b.WriteString("（旧侧）")
		case SideNew:
			b.WriteString("（新侧）")
		}
		b.WriteString("\n\n")
	}
	if marked {
		reason := "挑战结果：UNCERTAIN"
		if group.Challenge != nil && group.Challenge.Reason != "" {
			reason = "挑战结果：" + oneLine(group.Challenge.Reason, maxContentRunes)
		}
		if group.Decision != nil && group.Decision.Reason != "" {
			reason += "；" + oneLine(group.Decision.Reason, maxContentRunes)
		}
		b.WriteString("**标记原因**：")
		b.WriteString(escapeOneLine(reason, maxContentRunes))
		b.WriteString("\n\n")
	}
	if body := briefBody(group.Body); body != "" {
		b.WriteString(body)
		b.WriteString("\n\n")
	}
	if suggestion := renderSuggestion(group.Suggestion, maxContentRunes); suggestion != "" {
		b.WriteString(suggestion)
		b.WriteString("\n\n")
	}
	// A reused inline discussion may have been published before this was marked.
	if marked && strings.TrimSpace(group.Evidence) != "" {
		b.WriteString("<details><summary>证据</summary>\n\n")
		b.WriteString(codeFence(group.Evidence))
		b.WriteString("\n\n</details>\n\n")
	}
	if related := relatedDescriptions(group); related != "" {
		b.WriteString(related)
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

// NormalizeAnchorPosition maps a located diff anchor to the path, line and
// side that own its discussion. A context line carries both an old and a new
// line number, so a finding placed on either side of that unchanged line is
// physically the same anchor: both collapse onto the new side, its line number
// and (for a rename) its new path. Added and deleted lines carry a single
// number and keep their side. This is the single identity rule shared by
// position collection, rendering and lifecycle reconciliation.
func NormalizeAnchorPosition(path string, line int, side Side, anchor gitlab.Position) (string, int, Side) {
	if anchor.OldLine > 0 && anchor.NewLine > 0 {
		if anchor.NewPath != "" {
			path = anchor.NewPath
		}
		return path, anchor.NewLine, SideNew
	}
	if normalizedSide(side) == SideOld {
		return path, line, SideOld
	}
	return path, line, SideNew
}

// PositionAnchorKey returns the discussion key for a located diff anchor. It
// is the canonical identity used to collect, render and reconcile one inline
// thread, so context-line findings reported on different sides cannot claim
// two keys for the same physical line.
func PositionAnchorKey(path string, line int, side Side, anchor gitlab.Position) string {
	path, line, side = NormalizeAnchorPosition(path, line, side, anchor)
	return PositionPublishKey(path, line, side)
}

// RenderPositionThread renders the single inline discussion owned by one diff
// position. Wording changes between review generations update this body; a
// second distinct problem at the same anchor becomes a "### " section instead
// of another discussion. The hidden first line carries the position key so
// lifecycle reconciliation can keep the thread current.
func RenderPositionThread(groups []Group, anchor gitlab.Position, opts FindingRenderOptions) (string, error) {
	if len(groups) == 0 {
		return "", invalid("groups", "at least one finding is required")
	}
	key := PositionAnchorKey(groups[0].Path, groups[0].Line, groups[0].Side, anchor)
	if key == "" {
		return "", invalid("position", "finding has no diff anchor")
	}
	for _, group := range groups {
		if !group.Published() {
			return "", invalid("decision", "only publish or mark groups can be rendered inline")
		}
		if !group.PositionOK {
			return "", invalid("position_ok", "finding has no diff hunk and must be published in the summary only")
		}
		if PositionAnchorKey(group.Path, group.Line, group.Side, anchor) != key {
			return "", invalid("position", "all findings in one thread must share a position")
		}
	}
	ordered := append([]Group(nil), groups...)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if ra, rb := severityRank[a.EffectiveSeverity()], severityRank[b.EffectiveSeverity()]; ra != rb {
			return ra < rb
		}
		if at, bt := NormalizeTitle(a.Title), NormalizeTitle(b.Title); at != bt {
			return at < bt
		}
		return a.ID < b.ID
	})

	var head strings.Builder
	fmt.Fprintf(&head, "%s key=%s -->\n", FindingMarkerPrefix, key)
	if opts.Generation > 0 {
		fmt.Fprintf(&head, "<sub>代次 %d</sub>\n", opts.Generation)
	}
	budget := newNoteBudget(0)
	budget.write(head.String())
	for _, group := range ordered {
		writeFindingSection(budget, group, opts, len(ordered) == 1)
	}
	return limitPublishedNote(gitlab.NeutralizeQuickActions(budget.finish(""))), nil
}

// writeFindingSection appends one finding inside a rendered note. A flat
// section keeps the single-finding title line; a sectioned one uses "### " so
// several problems at one position stay visibly separate and ordered.
func writeFindingSection(budget *noteBudget, group Group, opts FindingRenderOptions, flat bool) {
	severity := group.EffectiveSeverity()

	var head strings.Builder
	if flat {
		head.WriteString("**[")
		head.WriteString(string(severity))
		if group.Decision.Action == ActionMark {
			head.WriteString(" · 存疑")
		}
		head.WriteString("]")
		head.WriteString(escapeOneLine(group.Title, MaxTitleRunes))
		head.WriteString("**\n")
	} else {
		head.WriteString("\n### [")
		head.WriteString(string(severity))
		if group.Decision.Action == ActionMark {
			head.WriteString(" · 存疑")
		}
		head.WriteString("]")
		head.WriteString(escapeOneLine(group.Title, MaxTitleRunes))
		head.WriteString("\n")
	}
	budget.write(head.String())
	// The discussion is anchored on the line, so the location is not repeated.
	// The body is published in full; only the whole-note platform budget can
	// omit its tail, and it says so.
	if body := briefBody(group.Body); body != "" {
		budget.write("\n" + body + "\n")
	}
	if group.Decision.Action == ActionMark && strings.TrimSpace(group.Decision.Reason) != "" {
		budget.write("\n**存疑原因**：" + escapeOneLine(group.Decision.Reason, maxContentRunes) + "\n")
	}
	if suggestion := renderSuggestion(group.Suggestion, maxContentRunes); suggestion != "" {
		budget.write("\n" + suggestion + "\n")
	}
	// Evidence stays only on uncertain findings, where the reader must judge.
	if group.Decision.Action == ActionMark && strings.TrimSpace(group.Evidence) != "" {
		budget.write("\n<details><summary>证据</summary>\n\n" + codeFence(group.Evidence) + "\n\n</details>\n")
	}
	if related := relatedDescriptions(group); related != "" {
		budget.write("\n" + related)
	}
	if opts.ShowModelAttribution {
		if attribution := modelAttribution(group, opts.ProviderLabels); attribution != "" {
			budget.write("\n<sub>来源：" + attribution + "</sub>\n")
		}
	}
}

// relatedDescriptions lists the groups folded into this discussion by
// MergePresentation (severity, category, title, short body excerpt). The
// list is already ordered by severity then position; at most maxRelatedGroups
// are shown and the rest is counted.
func relatedDescriptions(group Group) string {
	if len(group.Related) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("**相关描述**：\n")
	for i, related := range group.Related {
		if i == maxRelatedGroups {
			fmt.Fprintf(&b, "- 另有 %d 条\n", len(group.Related)-maxRelatedGroups)
			break
		}
		label := string(related.Severity)
		if related.Category != "" {
			label += " · " + related.Category
		}
		b.WriteString("- [" + escapeOneLine(label, 64) + "] " + escapeOneLine(related.Title, MaxTitleRunes))
		if excerpt := escapeOneLine(related.Excerpt, relatedExcerptRunes); excerpt != "" {
			b.WriteString("：" + excerpt)
			if related.ExcerptTruncated && related.ID != "" {
				b.WriteString("（编号 " + escapeOneLine(related.ID, 64) + "，完整描述见控制台）")
			}
		}
		b.WriteByte('\n')
	}
	return b.String()
}

func modelAttribution(group Group, labels map[string]string) string {
	seen := make(map[string]struct{})
	values := make([]string, 0, 3)
	for _, source := range group.Sources {
		name := strings.TrimSpace(source.Origin.Provider)
		if label := labels[name]; label != "" {
			name = label
		}
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
	return clipRunes(value, max)
}

// clipRunes bounds one logical line. A clipped value ends at the last sentence
// boundary (or, failing that, the last word boundary) before the limit and
// carries an ellipsis, so a published title or excerpt never stops mid-clause.
func clipRunes(value string, max int) string {
	if max <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	limit := max - 1
	cut := limit
	for i := limit - 1; i >= 0; i-- {
		if sentenceEnd(runes, i) {
			cut = i + 1
			break
		}
	}
	if cut == limit {
		for i := limit - 1; i >= 0; i-- {
			if unicode.IsSpace(runes[i]) {
				cut = i
				break
			}
		}
	}
	if cut <= 0 {
		cut = limit
	}
	return strings.TrimRight(string(runes[:cut]), " ") + "…"
}

// sentenceEnd reports a sentence boundary. An ASCII period only counts when it
// ends a sentence, so an abbreviation or a dotted file name is not a boundary.
func sentenceEnd(runes []rune, i int) bool {
	switch runes[i] {
	case '。', '！', '？', '；', '!', '?', ';':
		return true
	case '.':
		return i+1 >= len(runes) || unicode.IsSpace(runes[i+1]) || runes[i+1] == '"' || runes[i+1] == '”'
	}
	return false
}

// clipMarkdown bounds a multi-line Markdown value at a line or sentence
// boundary. The caller appends the explicit truncation notice.
func clipMarkdown(value string, max int) string {
	runes := []rune(value)
	if len(runes) <= max || max <= 0 {
		return value
	}
	cut := max
	for i := max - 1; i >= 0 && i >= max/2; i-- {
		if runes[i] == '\n' {
			cut = i
			break
		}
	}
	if cut == max {
		for i := max - 1; i >= 0; i-- {
			if sentenceEnd(runes, i) {
				cut = i + 1
				break
			}
		}
	}
	if cut <= 0 {
		cut = max
	}
	return strings.TrimRight(string(runes[:cut]), " \n")
}

func escapeOneLine(value string, max int) string {
	return escapeText(oneLine(value, max), max)
}

// escapeText renders untrusted single-line text literally. Backslashes are
// doubled before escaping punctuation, and empty spans break references that
// GitLab resolves after decoding Markdown entities.
func escapeText(value string, max int) string {
	value = strings.ReplaceAll(value, "\r\n", "\n")
	value = clipRunes(value, max)
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
	if utf8.RuneCountInString(value) > max {
		value = clipMarkdown(value, max)
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
	return out
}

// noteBudget renders a note block by block under the platform byte limit.
// Once a block no longer fits, rendering stops for good: the remaining blocks
// are omitted whole and finish appends platformLimitNotice. A block is never
// cut, so fences and <details> blocks stay balanced. reserve is the size of a
// fixed-shape tail (the summary footer) that finish appends after the budget.
type noteBudget struct {
	b       strings.Builder
	limit   int
	limited bool
}

func newNoteBudget(reserve int) *noteBudget {
	limit := MaxPublishedNoteBytes - len(platformLimitNotice) - reserve
	if limit < 0 {
		limit = 0
	}
	return &noteBudget{limit: limit}
}

func (n *noteBudget) fits(block string) bool {
	return n.b.Len()+len(block) <= n.limit
}

// write appends block when it still fits and reports whether it did.
func (n *noteBudget) write(block string) bool {
	if n.limited {
		return false
	}
	if !n.fits(block) {
		n.limited = true
		return false
	}
	n.b.WriteString(block)
	return true
}

// writeSection appends a section header followed by whole items, rendering one
// item at a time so an over-budget tail is never rendered or appended. The
// header is written only when the first item fits; the first item that does
// not fit stops the whole note.
func (n *noteBudget) writeSection(header string, count int, render func(int) string) {
	if n.limited || count == 0 {
		return
	}
	first := render(0)
	if !n.fits(header + first) {
		n.limited = true
		return
	}
	n.b.WriteString(header)
	n.b.WriteString(first)
	for i := 1; i < count; i++ {
		item := render(i)
		if !n.fits(item) {
			n.limited = true
			return
		}
		n.b.WriteString(item)
	}
}

// finish returns the note with its reserved tail and, when the budget was
// exhausted, the platform notice. The notice is the last line so the reader
// always sees why content ends.
func (n *noteBudget) finish(tail string) string {
	var b strings.Builder
	b.WriteString(n.b.String())
	b.WriteString(tail)
	if n.limited {
		b.WriteString(platformLimitNotice)
	}
	return b.String()
}

// limitPublishedNote applies the only publishing-time bound: GitLab validates
// note size in bytes (description_and_note_max_size, default 1 MiB) and rejects
// longer notes with HTTP 400. A longer note is cut at the last complete block
// boundary that fits and ends with platformLimitNotice, so the kept text never
// stops inside a code fence, a <details> block, or mid-rune/mid-item.
func limitPublishedNote(note string) string {
	if len(note) <= MaxPublishedNoteBytes {
		return note
	}
	budget := MaxPublishedNoteBytes - len(platformLimitNotice)
	return strings.TrimRight(note[:completeBlockCut(note, budget)], "\n") + platformLimitNotice
}

// completeBlockCut returns the byte offset of the last blank line that ends a
// block outside code fences and <details>, at or before budget bytes. Cutting
// at a line boundary keeps the result valid UTF-8. It returns zero when no
// such boundary fits, which publishes only the notice rather than a broken
// block.
func completeBlockCut(note string, budget int) int {
	var (
		cut       int
		bytes     int
		fenceChar byte
		fenceLen  int
		details   int
		lineStart int
	)
	for lineStart < len(note) {
		lineEnd := strings.IndexByte(note[lineStart:], '\n')
		if lineEnd < 0 {
			break
		}
		line := note[lineStart : lineStart+lineEnd]
		next := lineStart + lineEnd + 1
		trimmed := strings.TrimSpace(line)
		if fenceChar != 0 {
			if closesFence(line, fenceChar, fenceLen) {
				fenceChar, fenceLen = 0, 0
			}
		} else if ch, n, ok := opensFence(line); ok {
			fenceChar, fenceLen = ch, n
		} else if strings.HasPrefix(trimmed, "<details") {
			details++
		} else if strings.HasPrefix(trimmed, "</details>") && details > 0 {
			details--
		}
		bytes += next - lineStart
		if trimmed == "" && fenceChar == 0 && details == 0 && bytes <= budget {
			cut = next
		}
		lineStart = next
	}
	return cut
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

// codeFence wraps value in a fence longer than any backtick run in it. One
// linear scan keeps hostile backtick runs from making the fence search
// quadratic. A fence is always at least three backticks.
func codeFence(value string) string {
	longest, run := 0, 0
	for i := 0; i < len(value); i++ {
		if value[i] != '`' {
			run = 0
			continue
		}
		run++
		if run > longest {
			longest = run
		}
	}
	if longest < 2 {
		longest = 2
	}
	fence := strings.Repeat("`", longest+1)
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
