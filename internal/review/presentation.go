package review

import (
	"sort"
	"strings"
	"unicode"
)

// RelatedGroup records a group folded into another group's discussion by the
// presentation merge. It is display-only: the folded group keeps its own votes,
// challenge and rule decision in the stored findings.
type RelatedGroup struct {
	ID       string   `json:"id"`
	Severity Severity `json:"severity"`
	Category string   `json:"category,omitempty"`
	Line     int      `json:"line,omitempty"`
	Title    string   `json:"title"`
	Excerpt  string   `json:"excerpt,omitempty"`
}

// maxRelatedGroups bounds the 「相关描述」 list; the rest is counted.
const maxRelatedGroups = 5

// relatedExcerptRunes bounds each body excerpt in the related list.
const relatedExcerptRunes = 120

// MergePresentation folds near-duplicate groups that are already decided to be
// published into one discussion. It runs after adjudication and synthesis, so
// votes, consensus, challenges and decisions are never changed by it: each
// folded group keeps its stored state and is only marked dropped for display
// (Source "presentation"). Groups that are dropped, challenge-refuted or not
// published never take part. The result is deterministic: candidates are
// ordered by severity, then richness, then location and ID, the first member
// of each cluster is its representative, and later groups are compared with
// the representative only (no chaining).
func MergePresentation(groups []Group, dossier *Dossier) []Group {
	out := make([]Group, len(groups))
	copy(out, groups)
	var order []int
	for i, g := range out {
		if g.Published() && g.Line > 0 && foldablePath(dossier, g.Path) {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(x, y int) bool { return presentationLess(out[order[x]], out[order[y]]) })
	var reps []int
	folded := make(map[int][]int)
	for _, idx := range order {
		merged := false
		for _, rep := range reps {
			if presentationMatch(out[rep], out[idx]) {
				folded[rep] = append(folded[rep], idx)
				merged = true
				break
			}
		}
		if !merged {
			reps = append(reps, idx)
		}
	}
	for rep, members := range folded {
		display := &out[rep]
		display.Related = append([]RelatedGroup(nil), display.Related...)
		for _, idx := range members {
			member := &out[idx]
			display.Related = append(display.Related, RelatedGroup{
				ID: member.ID, Severity: member.EffectiveSeverity(), Category: member.Category,
				Line: member.Line, Title: member.Title, Excerpt: oneLine(member.Body, relatedExcerptRunes),
			})
			member.Decision = &FindingDecision{
				Action: ActionDrop, Reason: "merged into " + display.ID + " for presentation", Source: "presentation",
			}
		}
		sort.SliceStable(display.Related, func(x, y int) bool {
			a, b := display.Related[x], display.Related[y]
			if ra, rb := severityRank[a.Severity], severityRank[b.Severity]; ra != rb {
				return ra < rb
			}
			if a.Line != b.Line {
				return a.Line < b.Line
			}
			return a.ID < b.ID
		})
	}
	return out
}

// foldablePath reports whether a group's own path can take part in folding.
// Unlike publish-key provenance it is per path: an unrelated ignored or
// omitted file elsewhere in the MR must not disable folding. The path itself
// must not be ignored or omitted, and must resolve to one changed file.
func foldablePath(dossier *Dossier, path string) bool {
	if path == "" {
		return false
	}
	if dossier == nil {
		return true
	}
	if dossier.IgnoresPath(path) {
		return false
	}
	for _, omitted := range dossier.DiffSummary.Omitted {
		if omitted.Path == path {
			return false
		}
	}
	oldFile, oldCount := matchingFile(dossier.Files, path, SideOld)
	newFile, newCount := matchingFile(dossier.Files, path, SideNew)
	return !(oldCount > 1 || newCount > 1 || (oldCount == 1 && newCount == 1 && oldFile != newFile))
}

func presentationLess(a, b Group) bool {
	if ra, rb := severityRank[a.EffectiveSeverity()], severityRank[b.EffectiveSeverity()]; ra != rb {
		return ra < rb
	}
	if ca, cb := Finding(groupDisplay(a)), Finding(groupDisplay(b)); richer(ca, cb) != richer(cb, ca) {
		return richer(ca, cb)
	}
	if a.Path != b.Path {
		return a.Path < b.Path
	}
	if a.Line != b.Line {
		return a.Line < b.Line
	}
	return a.ID < b.ID
}

func presentationMatch(rep, g Group) bool {
	if rep.Path != g.Path || normalizedSide(rep.Side) != normalizedSide(g.Side) {
		return false
	}
	// An uncertain (存疑) group never hides inside a confident discussion.
	if (rep.Decision.Action == ActionMark) != (g.Decision.Action == ActionMark) {
		return false
	}
	d := severityRank[rep.EffectiveSeverity()] - severityRank[g.EffectiveSeverity()]
	if d >= nearDuplicateSeverityGap || -d >= nearDuplicateSeverityGap {
		return false
	}
	sameCategory := rep.Category == g.Category
	difference := absInt(rep.Line - g.Line)
	threshold := nearDuplicateCrossCategory
	if sameCategory {
		if difference > nearDuplicateLines {
			return false
		}
		threshold = nearDuplicateSameCategory
	} else if difference != 0 {
		return false
	}
	if !titleIdentityMatches(rep.Title, g.Title) || !titleContentMatches(rep.Title, g.Title) {
		return false
	}
	return textSimilar(textTokens(rep.Title, rep.Body), Finding{Title: g.Title, Body: g.Body}, threshold)
}

// titleContentMatches requires the titles to share content beyond identifiers
// (a function name alone never proves the same problem): at least two shared
// CJK bigrams, or at least two shared plain English words.
func titleContentMatches(a, b string) bool {
	return sharedCount(cjkBigrams(a), cjkBigrams(b)) >= nearDuplicateTitleContent ||
		sharedCount(englishContentWords(a), englishContentWords(b)) >= nearDuplicateTitleContent
}

func sharedCount(a, b map[string]struct{}) int {
	n := 0
	for token := range a {
		if _, ok := b[token]; ok {
			n++
		}
	}
	return n
}

// stopBigrams are CJK bigrams too common in titles to count as shared content.
var stopBigrams = map[string]bool{
	"存在": true, "问题": true, "导致": true, "可能": true, "没有": true, "不能": true, "需要": true, "应该": true,
	"配置": true, "文件": true, "用户": true, "输入": true, "校验": true, "参数": true, "数据": true, "处理": true, "检查": true,
}

func cjkBigrams(title string) map[string]struct{} {
	out := make(map[string]struct{})
	var run []rune
	flush := func() {
		for i := 0; i+1 < len(run); i++ {
			if bigram := string(run[i : i+2]); !stopBigrams[bigram] {
				out[bigram] = struct{}{}
			}
		}
		run = run[:0]
	}
	for _, r := range title {
		if unicode.Is(unicode.Han, r) {
			run = append(run, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// englishContentWords returns lowercase plain words of four or more letters;
// code identifiers (underscores, digits, camelCase) and generic words are out.
func englishContentWords(title string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, word := range strings.FieldsFunc(title, func(r rune) bool {
		return !(r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'))
	}) {
		if len(word) < 4 {
			continue
		}
		plain := true
		for i, r := range word {
			if !unicode.IsLetter(r) || (i > 0 && unicode.IsUpper(r)) {
				plain = false
				break
			}
		}
		lower := strings.ToLower(word)
		if plain && !genericIdentifiers[lower] && !nearDuplicateStopwords[lower] {
			out[lower] = struct{}{}
		}
	}
	return out
}

// summaryTitleContentMatches is the stricter title-content rule of the summary
// folding. A pathless candidate has no location to anchor it, so the same
// function name plus a similar-sounding title is not enough: the titles must
// share at least two content units that cover a minimum share of the second
// (candidate) title's CJK units, or of the union of English words. CJK units are
// bigrams built after removing stop words from the text, so a fragment that
// straddles a stop word (未处理 -> 未处/理文) never counts; English words
// exclude any part of a code identifier of either title, including the parts
// of a qualified name such as `Type.method`. Inline folding keeps the legacy
// titleContentMatches.
func summaryTitleContentMatches(a, b string) bool {
	ca, cb := cjkContentUnits(a), cjkContentUnits(b)
	if shared := sharedCount(ca, cb); shared >= nearDuplicateTitleContent && len(cb) > 0 &&
		float64(shared)/float64(len(cb)) >= nearDuplicateCJKCoverage {
		return true
	}
	exclude := identifierParts(a, b)
	ea, eb := englishContentUnits(a, exclude), englishContentUnits(b, exclude)
	shared := sharedCount(ea, eb)
	union := len(ea) + len(eb) - shared
	return shared >= nearDuplicateTitleContent && union > 0 && float64(shared)/float64(union) >= nearDuplicateTitleCoverage
}

// summaryStopWords are CJK words too common in titles to count as shared content.
// They are removed from the text before bigrams are built.
var summaryStopWords = []string{
	"存在", "问题", "导致", "可能", "没有", "不能", "需要", "应该",
	"配置", "文件", "用户", "输入", "校验", "参数", "数据", "处理", "检查",
}

func cjkContentUnits(title string) map[string]struct{} {
	out := make(map[string]struct{})
	var run []rune
	flush := func() {
		var segment []rune
		endSegment := func() {
			for i := 0; i+1 < len(segment); i++ {
				out[string(segment[i:i+2])] = struct{}{}
			}
			segment = segment[:0]
		}
		for i := 0; i < len(run); i++ {
			if i+1 < len(run) && isSummaryStopWord(string(run[i:i+2])) {
				endSegment()
				i++
				continue
			}
			segment = append(segment, run[i])
		}
		endSegment()
		run = run[:0]
	}
	for _, r := range title {
		if unicode.Is(unicode.Han, r) {
			run = append(run, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

func isSummaryStopWord(word string) bool {
	for _, stop := range summaryStopWords {
		if word == stop {
			return true
		}
	}
	return false
}

// identifierParts returns the lowercase parts of every code identifier in the
// titles: qualified names (`TokenBucket.allow`), snake_case and camelCase.
func identifierParts(titles ...string) map[string]struct{} {
	out := make(map[string]struct{})
	for _, title := range titles {
		for _, field := range strings.FieldsFunc(title, func(r rune) bool {
			return !(r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' || r == '.'))
		}) {
			field = strings.Trim(field, ".")
			if !strings.ContainsAny(field, "._") && !hasInnerUpper(field) {
				continue
			}
			for _, part := range strings.FieldsFunc(field, func(r rune) bool { return r == '.' || r == '_' }) {
				out[strings.ToLower(part)] = struct{}{}
			}
		}
	}
	return out
}

func hasInnerUpper(word string) bool {
	for i, r := range word {
		if i > 0 && unicode.IsUpper(r) {
			return true
		}
	}
	return false
}

// englishContentUnits returns lowercase plain words of three or more letters
// that are not parts of a code identifier (exclude); code identifiers
// (underscores, digits, camelCase) and generic words are out.
func englishContentUnits(title string, exclude map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{})
	for _, word := range strings.FieldsFunc(title, func(r rune) bool {
		return !(r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_'))
	}) {
		if len(word) < 3 {
			continue
		}
		plain := true
		for i, r := range word {
			if !unicode.IsLetter(r) || (i > 0 && unicode.IsUpper(r)) {
				plain = false
				break
			}
		}
		lower := strings.ToLower(word)
		if _, part := exclude[lower]; plain && !part && !genericIdentifiers[lower] && !nearDuplicateStopwords[lower] {
			out[lower] = struct{}{}
		}
	}
	return out
}

// Near-duplicate thresholds. Candidates are compared with the group anchor
// only. Same-category findings 4-6 lines apart and cross-category findings on
// the exact same line must share title identifiers (Jaccard over non-generic
// ASCII identifiers) and then pass a body text similarity check.
const (
	nearDuplicateLines         = 6
	nearDuplicateSameCategory  = 0.45
	nearDuplicateCrossCategory = 0.5
	nearDuplicateMinShared     = 4
	nearDuplicateTitleJaccard  = 0.5
	nearDuplicateTitleContent  = 2
	// Shared title content must also cover this share of the units compared.
	nearDuplicateTitleCoverage = 0.5
	nearDuplicateCJKCoverage   = 0.3
	// Groups whose highest severities are this many levels apart (P0 vs P2)
	// are never approximately merged.
	nearDuplicateSeverityGap = 2
)

// genericIdentifiers are ASCII words common to many unrelated findings; they
// never count as identity evidence in a title.
var genericIdentifiers = map[string]bool{
	"err": true, "nil": true, "ctx": true, "return": true, "error": true, "errors": true,
	"true": true, "false": true, "null": true, "none": true, "self": true, "this": true,
	"func": true, "def": true, "var": true, "let": true, "const": true, "resp": true,
	"req": true, "data": true, "value": true, "values": true, "result": true, "results": true,
	"handle": true, "handler": true, "get": true, "set": true, "new": true, "int": true,
	"str": true, "string": true, "list": true, "map": true, "bool": true, "type": true,
	"key": true, "name": true, "file": true, "code": true, "test": true, "main": true,
	"not": true, "missing": true, "check": true, "unchecked": true, "invalid": true,
}

func titleIdentifiers(title string) map[string]struct{} {
	out := make(map[string]struct{})
	for token := range textTokens(title) {
		if identifierToken(token) && !genericIdentifiers[token] {
			out[token] = struct{}{}
		}
	}
	return out
}

func titleIdentityMatches(a, b string) bool {
	ids, other := titleIdentifiers(a), titleIdentifiers(b)
	shared := 0
	for token := range ids {
		if _, ok := other[token]; ok {
			shared++
		}
	}
	union := len(ids) + len(other) - shared
	return shared >= 1 && float64(shared)/float64(union) >= nearDuplicateTitleJaccard
}

func textSimilar(tokens map[string]struct{}, finding Finding, threshold float64) bool {
	other := textTokens(finding.Title, finding.Body)
	if tokenJaccardAtLeast(tokens, other, threshold, nil) {
		return true
	}
	// CJK prose adds many incidental bigrams that dilute the score of two
	// paraphrases of one problem, while identifiers (function and exception
	// names, parameters) stay stable. Compare that identifier view too.
	return tokenJaccardAtLeast(tokens, other, threshold, identifierToken)
}

func identifierToken(token string) bool {
	return len(token) >= 3 && token[0] < 0x80
}

func tokenJaccardAtLeast(a, b map[string]struct{}, threshold float64, keep func(string) bool) bool {
	sizeA, sizeB, shared := 0, 0, 0
	for token := range a {
		if keep != nil && !keep(token) {
			continue
		}
		sizeA++
		if _, ok := b[token]; ok {
			shared++
		}
	}
	for token := range b {
		if keep == nil || keep(token) {
			sizeB++
		}
	}
	union := sizeA + sizeB - shared
	if union == 0 || shared < nearDuplicateMinShared {
		return false
	}
	return float64(shared)/float64(union) >= threshold
}

// textTokens lowercases title and body into a set of significant tokens:
// alphanumeric words of two or more characters outside a small stopword list,
// plus Han bigrams so Chinese text is comparable without a segmenter.
func textTokens(parts ...string) map[string]struct{} {
	tokens := make(map[string]struct{})
	var word []rune
	var han []rune
	flushWord := func() {
		if len(word) >= 2 {
			if value := string(word); !nearDuplicateStopwords[value] {
				tokens[value] = struct{}{}
			}
		}
		word = word[:0]
	}
	flushHan := func() {
		for i := 0; i+1 < len(han); i++ {
			tokens[string(han[i:i+2])] = struct{}{}
		}
		han = han[:0]
	}
	for _, part := range parts {
		for _, r := range strings.ToLower(part) {
			switch {
			case unicode.Is(unicode.Han, r):
				flushWord()
				han = append(han, r)
			case unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_':
				flushHan()
				word = append(word, r)
			default:
				flushWord()
				flushHan()
			}
		}
		flushWord()
		flushHan()
	}
	return tokens
}

var nearDuplicateStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "that": true, "this": true, "with": true, "are": true,
	"not": true, "can": true, "may": true, "will": true, "when": true, "from": true, "which": true,
	"does": true, "has": true, "have": true, "its": true, "but": true, "any": true, "all": true,
	"is": true, "in": true, "to": true, "of": true, "or": true, "be": true, "as": true, "on": true,
	"it": true, "by": true, "an": true, "at": true, "so": true, "if": true,
}

func absInt(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// summaryFold is the summary-only view of publish-decided groups after
// near-duplicate unpositioned groups were referenced from a group that is
// already listed. It never touches inline discussions (their bodies and
// hashes stay stable) or stored findings.
type summaryFold struct {
	// folded holds IDs of groups not listed separately.
	folded map[string]bool
	// related maps a listed group ID to the groups referenced under it.
	related map[string][]RelatedGroup
}

// foldUnpositionedForSummary decides which unpositioned (no path or position
// not OK), confident, publish-decided groups duplicate a group that is already
// published inline or already listed. Candidates are visited in a fixed order
// (severity, richness, location, ID) and matched against the representatives
// only: positioned groups first, then earlier kept unpositioned groups.
func foldUnpositionedForSummary(groups []Group) summaryFold {
	fold := summaryFold{folded: map[string]bool{}, related: map[string][]RelatedGroup{}}
	var reps, candidates []Group
	for _, g := range groups {
		if !g.Published() || g.Decision.Action == ActionMark {
			continue
		}
		if g.PositionOK {
			reps = append(reps, g)
		} else {
			candidates = append(candidates, g)
		}
	}
	sort.SliceStable(reps, func(x, y int) bool { return presentationLess(reps[x], reps[y]) })
	sort.SliceStable(candidates, func(x, y int) bool { return presentationLess(candidates[x], candidates[y]) })
	for _, g := range candidates {
		matched := false
		for _, rep := range reps {
			if !unpositionedMatch(rep, g) {
				continue
			}
			fold.folded[g.ID] = true
			// The folded group's own related items (from MergePresentation)
			// move along with it, so none of them disappears from the summary.
			added := append([]RelatedGroup{{
				ID: g.ID, Severity: g.EffectiveSeverity(), Category: g.Category,
				Line: g.Line, Title: g.Title, Excerpt: oneLine(g.Body, relatedExcerptRunes),
			}}, g.Related...)
			fold.related[rep.ID] = append(fold.related[rep.ID], added...)
			matched = true
			break
		}
		if !matched {
			reps = append(reps, g)
		}
	}
	for id, list := range fold.related {
		fold.related[id] = SortRelated(list)
	}
	return fold
}

// unpositionedMatch reports whether an unpositioned group g describes the
// same problem as the listed group rep. A reasoner finding often has no path
// and a differently worded title, so the evidence is the named identifiers
// (functions, parameters, exceptions) that one title shares with the other
// group's title or body, together with shared title content. A listed title
// without identifiers (a plain-prose title) only needs the first direction.
func unpositionedMatch(rep, g Group) bool {
	if rep.Path != "" && g.Path != "" && (rep.Path != g.Path || normalizedSide(rep.Side) != normalizedSide(g.Side)) {
		return false
	}
	// A more severe candidate would vanish from the header counts and the
	// listing, so it never folds under a less severe group; a candidate at
	// most one level less severe may.
	d := severityRank[g.EffectiveSeverity()] - severityRank[rep.EffectiveSeverity()]
	if d < 0 || d >= nearDuplicateSeverityGap {
		return false
	}
	if !identifiersReferenced(g.Title, rep.Title, rep.Body) {
		return false
	}
	contentMatch := summaryTitleContentMatches(rep.Title, g.Title)
	similar := textSimilar(textTokens(rep.Title, rep.Body), Finding{Title: g.Title, Body: g.Body}, nearDuplicateSameCategory)
	if len(titleIdentifiers(rep.Title)) == 0 {
		// Nothing ties a prose-only title to the candidate's identifier except
		// the body, so both content and text evidence are required.
		return contentMatch && similar
	}
	return identifiersReferenced(rep.Title, g.Title, g.Body) && (contentMatch || similar)
}

// SummaryFoldedGroups returns the groups the summary lists or counts: those
// folded into another entry by the summary presentation are removed. Stored
// findings are unaffected; callers use it for display-only counts.
func SummaryFoldedGroups(groups []Group) []Group {
	fold := foldUnpositionedForSummary(groups)
	out := make([]Group, 0, len(groups))
	for _, g := range groups {
		if !fold.folded[g.ID] {
			out = append(out, g)
		}
	}
	return out
}

// identifiersReferenced reports whether a non-generic identifier of title
// occurs in the other title or body.
func identifiersReferenced(title, otherTitle, otherBody string) bool {
	other := textTokens(otherTitle, otherBody)
	for id := range titleIdentifiers(title) {
		if _, ok := other[id]; ok {
			return true
		}
	}
	return false
}

// SortRelated returns related items without duplicate IDs, ordered by
// severity, then line, then ID, as the 「相关描述」 list is capped after sorting.
func SortRelated(list []RelatedGroup) []RelatedGroup {
	seen := make(map[string]bool, len(list))
	out := make([]RelatedGroup, 0, len(list))
	for _, r := range list {
		if !seen[r.ID] {
			seen[r.ID] = true
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(x, y int) bool {
		a, b := out[x], out[y]
		if ra, rb := severityRank[a.Severity], severityRank[b.Severity]; ra != rb {
			return ra < rb
		}
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		return a.ID < b.ID
	})
	return out
}
