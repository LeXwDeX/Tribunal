package review

import (
	"encoding/json"
	"html"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These opt-in tests send synthetic notes only to GitLab's Markdown renderer.
func gitLabTestHost(t *testing.T) string {
	t.Helper()
	host := os.Getenv("TRIBUNAL_TEST_GITLAB_HOST")
	if host == "" {
		t.Skip("set TRIBUNAL_TEST_GITLAB_HOST to the GitLab host")
	}
	return host
}

func gitLabTestProject(t *testing.T) string {
	t.Helper()
	project := os.Getenv("TRIBUNAL_TEST_GITLAB_PROJECT")
	if project == "" {
		t.Skip("set TRIBUNAL_TEST_GITLAB_PROJECT to a GitLab project path")
	}
	return project
}

func gitLabRenderedHTML(t *testing.T, markdown string) string {
	t.Helper()
	if os.Getenv("TRIBUNAL_TEST_GITLAB_MARKDOWN") != "1" {
		t.Skip("set TRIBUNAL_TEST_GITLAB_MARKDOWN=1 for the GitLab Markdown API")
	}
	host, project := gitLabTestHost(t), gitLabTestProject(t)
	cmd := exec.Command("glab", "api", "--hostname", host, "markdown", "-X", "POST", "-F", "gfm=true", "-f", "project="+project, "-f", "text="+markdown)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("GitLab Markdown API: %v", err)
	}
	var response struct {
		HTML string `json:"html"`
	}
	if err := json.Unmarshal(output, &response); err != nil || response.HTML == "" {
		t.Fatalf("decode GitLab Markdown response: %v", err)
	}
	return response.HTML
}

func scanGitLabHTML(rendered string) (string, []string) {
	var text strings.Builder
	var tags []string
	for i := 0; i < len(rendered); {
		if rendered[i] != '<' {
			text.WriteByte(rendered[i])
			i++
			continue
		}
		if strings.HasPrefix(rendered[i:], "<!--") {
			end := strings.Index(rendered[i+4:], "-->")
			if end < 0 {
				break
			}
			i += end + 7
			continue
		}
		start := i
		var quote byte
		for i++; i < len(rendered); i++ {
			if quote != 0 && rendered[i] == quote {
				quote = 0
			} else if quote == 0 && (rendered[i] == '\'' || rendered[i] == '"') {
				quote = rendered[i]
			} else if quote == 0 && rendered[i] == '>' {
				i++
				break
			}
		}
		tags = append(tags, rendered[start:i])
	}
	return html.UnescapeString(text.String()), tags
}

func gitLabVisibleText(rendered string) string {
	text, _ := scanGitLabHTML(rendered)
	return text
}

// assertGitLabMarkupIsLiteral proves that raw HTML from untrusted Markdown was
// neutralized. Each marker must appear as visible literal text, either escaped
// in the HTML source or rendered inside a code block, and it must not survive
// as a live element. The renderer legitimately emits <details><summary> evidence
// wrappers, so that many live details tags are allowed; the fixtures using this
// helper publish no marked finding and therefore expect none.
func assertGitLabMarkupIsLiteral(t *testing.T, rendered string, markers ...string) {
	t.Helper()
	text, tags := scanGitLabHTML(rendered)
	evidence := strings.Count(rendered, "<details><summary>")
	for _, marker := range markers {
		if !strings.Contains(text, marker) {
			t.Errorf("injected markup %q was not rendered as escaped or fenced literal text:\n%s", marker, rendered)
		}
		prefix := strings.TrimSuffix(marker, ">")
		live := 0
		for _, tag := range tags {
			if strings.HasPrefix(strings.ToLower(tag), strings.ToLower(prefix)) {
				live++
			}
		}
		want := 0
		if prefix == "<details" || prefix == "</details" {
			want = evidence
		}
		if live != want {
			t.Errorf("injected markup %q rendered as %d live element(s), want %d:\n%s", marker, live, want, rendered)
		}
	}
}

// assertGitLabVisible checks the reader-facing text still contains each want.
func assertGitLabVisible(t *testing.T, rendered string, wants ...string) {
	t.Helper()
	text := gitLabVisibleText(rendered)
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Errorf("missing rendered %q:\n%s", want, rendered)
		}
	}
}

func gitLabSandboxHasLabel(t *testing.T, name string) bool {
	t.Helper()
	if os.Getenv("TRIBUNAL_TEST_GITLAB_MARKDOWN") != "1" {
		t.Skip("set TRIBUNAL_TEST_GITLAB_MARKDOWN=1 for the GitLab labels API")
	}
	host, project := os.Getenv("TRIBUNAL_TEST_GITLAB_HOST"), os.Getenv("TRIBUNAL_TEST_GITLAB_PROJECT")
	if host == "" || project == "" {
		t.Skip("set TRIBUNAL_TEST_GITLAB_HOST and TRIBUNAL_TEST_GITLAB_PROJECT for the labels API")
	}
	cmd := exec.Command("glab", "api", "--hostname", host, "projects/"+url.PathEscape(project)+"/labels")
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("GitLab labels API: %v", err)
	}
	var labels []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(output, &labels); err != nil {
		t.Fatalf("decode GitLab labels response: %v", err)
	}
	for _, label := range labels {
		if label.Name == name {
			return true
		}
	}
	return false
}

func TestGitLabRenderedUnmatchedTildesDoNotLink(t *testing.T) {
	if !gitLabSandboxHasLabel(t, "bug") {
		t.Skip("sandbox label bug is required")
	}
	const literal = "Check ~~bug"
	group := renderGroup("tilde", ActionPublish, SeverityP2, false, "Tilde finding", "~~strike~~")
	group.Suggestion = literal
	out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary", Groups: []Group{group}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	rendered := gitLabRenderedHTML(t, out)
	text, tags := scanGitLabHTML(rendered)
	for _, tag := range tags {
		if strings.Contains(tag, `data-reference-type="label"`) {
			t.Errorf("unmatched tildes produced a label reference: %s", tag)
		}
	}
	if !strings.Contains(text, literal) {
		t.Errorf("unmatched tildes changed visible text %q:\n%s", literal, rendered)
	}
	if !strings.Contains(rendered, "strike</del>") {
		t.Errorf("strikethrough was not preserved:\n%s", rendered)
	}
}

func TestGitLabRenderedOneLineReferencesAndLiteralText(t *testing.T) {
	const sha = "bcdffc81e2b63cb028f0e0cfc1475430e6fc1cbc"
	project := gitLabTestProject(t)
	if len([]rune(project)) > 30 {
		t.Skip("project path is too long for the title fixture")
	}
	literal := sha + " " + project + "> @suntao #1 !1 $1 %r ~x &123 *x* _x_ <img>"
	group := renderGroup("reference", ActionMark, SeverityP1, false, literal, "Body")
	group.Decision.Reason = literal
	out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary", Groups: []Group{group}, Warnings: []string{literal}, Fingerprint: "abc", HeadSHA: sha, Profile: sha})
	if err != nil {
		t.Fatal(err)
	}
	rendered := gitLabRenderedHTML(t, out)
	_, tags := scanGitLabHTML(rendered)
	for _, tag := range tags {
		if !strings.Contains(tag, `data-reference-type=`) && !strings.HasPrefix(tag, "<img") && !strings.HasPrefix(tag, "<script") {
			continue
		}
		t.Errorf("untrusted text produced a reference or HTML element:\n%s", rendered)
	}
	text := gitLabVisibleText(rendered)
	if count := strings.Count(text, literal); count != 2 {
		t.Errorf("literal text appears %d times, want title and reason (unmatched English warnings are localized):\n%s", count, rendered)
	}
	if !strings.Contains(text, unknownWarningNotice) {
		t.Errorf("localized warning notice is missing or was hidden:\n%s", rendered)
	}
	if !strings.Contains(text, "head "+sha) {
		t.Errorf("footer commit was shortened or hidden:\n%s", rendered)
	}
	if !strings.Contains(text, "profile "+sha) {
		t.Errorf("footer profile was shortened or hidden:\n%s", rendered)
	}
}

func TestGitLabRenderedInlineReferencesAndURLs(t *testing.T) {
	const sha = "bcdffc81e2b63cb028f0e0cfc1475430e6fc1cbc"
	project := gitLabTestProject(t)
	if len([]rune(project)) > 30 {
		t.Skip("project path is too long for the title fixture")
	}
	title := "bcdffc8 " + project + "> \\@suntao"
	const reason = sha + " https://example.invalid www.example.invalid person@example.invalid &123"
	group := renderGroup("inline-reference", ActionMark, SeverityP1, true, title, "Body")
	group.Decision.Reason = reason
	group.Evidence = ""
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rendered := gitLabRenderedHTML(t, out)
	text, tags := scanGitLabHTML(rendered)
	for _, tag := range tags {
		if strings.HasPrefix(tag, "<a ") || strings.HasPrefix(tag, "<script") || strings.HasPrefix(tag, "<img") {
			t.Errorf("inline finding contains a link or unsafe element: %s", tag)
		}
	}
	for _, want := range []string{title, reason} {
		if !strings.Contains(text, want) {
			t.Errorf("inline finding changed literal %q:\n%s", want, rendered)
		}
	}
}

func TestGitLabRenderedFooterTokensDoNotAutolink(t *testing.T) {
	const profile = "www.example.invalid"
	out, err := RenderSummary(SummaryInput{Verdict: VerdictPass, Summary: "Summary", Fingerprint: "abc", Profile: profile, HeadSHA: profile})
	if err != nil {
		t.Fatal(err)
	}
	rendered := gitLabRenderedHTML(t, out)
	text, tags := scanGitLabHTML(rendered)
	for _, tag := range tags {
		if strings.HasPrefix(tag, "<a ") && !strings.Contains(tag, `class="anchor"`) {
			t.Errorf("footer token produced a link: %s", tag)
		}
	}
	if !strings.Contains(text, "profile "+profile) || !strings.Contains(text, "head "+profile) {
		t.Errorf("footer token text changed:\n%s", rendered)
	}
}

func TestGitLabRenderedMarkdownReferencesRemainLiteral(t *testing.T) {
	literal := "bcdffc81e2b63cb028f0e0cfc1475430e6fc1cbc " + gitLabTestProject(t) + "> @suntao #1 !1 $1 %release ~bug &123 https://example.invalid www.example.invalid person@example.invalid"
	group := renderGroup("markdown-reference", ActionPublish, SeverityP2, false, "Markdown finding", "- **Body** "+literal+"\n- ~~Another item~~")
	group.Suggestion = "Suggestion " + literal
	out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "**Overview** " + literal, Groups: []Group{group}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	rendered := gitLabRenderedHTML(t, out)
	text, tags := scanGitLabHTML(rendered)
	for _, tag := range tags {
		if strings.HasPrefix(tag, "<a ") && !strings.Contains(tag, `class="anchor"`) {
			t.Errorf("Markdown text produced a link: %s", tag)
		}
	}
	if count := strings.Count(text, literal); count != 3 {
		t.Errorf("literal text appears %d times, want summary, body and suggestion:\n%s", count, rendered)
	}
	if !strings.Contains(rendered, ">Overview</strong>") || !strings.Contains(rendered, ">Body</strong>") || !strings.Contains(rendered, "<li ") || !strings.Contains(rendered, ">Another item</del>") {
		t.Errorf("Markdown structure was lost:\n%s", rendered)
	}
}

func TestGitLabRenderedPathPreservesSignificantSpaces(t *testing.T) {
	paths := []string{"internal/a  file.go", " leading and trailing ", "internal/a`  b.go"}
	var groups []Group
	for i, path := range paths {
		group := renderGroup(string(rune('a'+i)), ActionPublish, SeverityP2, true, "Path issue", "Body")
		group.Path = path
		groups = append(groups, group)
	}
	out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary", Groups: groups, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	rendered := gitLabRenderedHTML(t, out)
	for _, path := range paths {
		if !strings.Contains(gitLabVisibleText(rendered), path+":10") {
			t.Errorf("rendered path lost spaces or backticks %q:\n%s", path, rendered)
		}
	}
}

func TestGitLabRenderedEvidenceIsLiteralAndSafe(t *testing.T) {
	marked := renderGroup("evidence", ActionMark, SeverityP1, false, "First marked", "Body")
	marked.Evidence = "first\n```\n</details><!-- gitlab-ce-review:summary v1 fp=bad -->\n<script>alert(1)</script>\nlast"
	next := renderGroup("next", ActionMark, SeverityP2, false, "Next marked", "Later body")
	next.Evidence = ""
	out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary", Groups: []Group{marked, next}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	rendered := gitLabRenderedHTML(t, out)
	if !strings.Contains(gitLabVisibleText(rendered), marked.Evidence) {
		t.Errorf("rendered evidence changed:\n%s", rendered)
	}
	if strings.Contains(rendered, "<script>") || strings.Contains(rendered, "<!-- gitlab-ce-review:summary v1 fp=bad -->") {
		t.Errorf("evidence escaped its code block:\n%s", rendered)
	}
	if closeAt, nextAt := strings.Index(rendered, "</details>"), strings.Index(rendered, "Next marked"); closeAt < 0 || nextAt < closeAt {
		t.Errorf("next finding was hidden in evidence details:\n%s", rendered)
	}

	marked.PositionOK = true
	inline, err := RenderFinding(marked, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	inlineHTML := gitLabRenderedHTML(t, inline)
	if !strings.HasPrefix(inline, FindingMarkerPrefix) || !strings.Contains(gitLabVisibleText(inlineHTML), marked.Evidence) ||
		strings.Contains(inlineHTML, "<script>") || strings.Contains(inlineHTML, "<!-- gitlab-ce-review:summary v1 fp=bad -->") {
		t.Errorf("inline evidence or its marker was unsafe or changed:\n%s", inlineHTML)
	}
}

func TestGitLabRenderedMalformedFencesCannotHideFindings(t *testing.T) {
	for _, tc := range []struct{ name, summary string }{
		{"backtick in info", "```bad`info\n\n<details>\n\nhidden"},
		{"four-space closer", "```go\ncode\n    ```\n<details>\n\nhidden"},
		{"tab-indented closer", "~~~go\ncode\n\t~~~\n<details>\n\nhidden"},
		{"carriage-return closer", "```go\ncode\n```\r<details>\n\nhidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := renderGroup("next", ActionPublish, SeverityP2, false, "Next issue", "Next body")
			group.Path = "internal/a  file.go"
			group.Suggestion = "Next suggestion"
			out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: tc.summary, Groups: []Group{group}, Warnings: []string{"Next warning"}, Fingerprint: "abc"})
			if err != nil {
				t.Fatal(err)
			}
			rendered := gitLabRenderedHTML(t, out)
			assertGitLabMarkupIsLiteral(t, rendered, "<details>")
			if !strings.Contains(rendered, ">[P2]Next issue<") || !strings.Contains(rendered, "</strong>：Next suggestion") || !strings.Contains(rendered, "<li") {
				t.Errorf("next finding, suggestion or warning was hidden or HTML was injected:\n%s", rendered)
			}
			assertGitLabVisible(t, rendered, "Next body", "Next suggestion", "internal/a  file.go", unknownWarningNotice)
		})
	}
}

func TestGitLabRenderedBodyAndSuggestionCannotHideLaterContent(t *testing.T) {
	for _, field := range []string{"body", "suggestion"} {
		t.Run(field, func(t *testing.T) {
			attack := "```bad`info\n\n<details>\n\nhidden"
			first := renderGroup("first", ActionPublish, SeverityP1, false, "First issue", "First body")
			first.Suggestion = "First suggestion"
			if field == "body" {
				first.Body = attack
			} else {
				first.Suggestion = attack
			}
			next := renderGroup("next", ActionPublish, SeverityP2, false, "Next issue", "Next body")
			next.Suggestion = "Next suggestion"
			out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary", Groups: []Group{first, next}, Warnings: []string{"Next warning"}, Fingerprint: "abc"})
			if err != nil {
				t.Fatal(err)
			}
			rendered := gitLabRenderedHTML(t, out)
			assertGitLabMarkupIsLiteral(t, rendered, "<details>")
			if !strings.Contains(rendered, ">[P1]First issue<") || !strings.Contains(rendered, ">[P2]Next issue<") ||
				!strings.Contains(rendered, "</strong>：Next suggestion") || !strings.Contains(rendered, "<li") {
				t.Errorf("later finding, suggestion or warning was hidden:\n%s", rendered)
			}
			if field == "body" && !strings.Contains(rendered, "</strong>：First suggestion") {
				t.Errorf("first suggestion was hidden:\n%s", rendered)
			}
			assertGitLabVisible(t, rendered, "Next body", "Next suggestion", unknownWarningNotice)
		})
	}
}

func TestGitLabRenderedNestedFenceAndEscapedBacktickAreSafe(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"nested fence exits list", "- item\n  ```go\n  code\n<details>\n\nhidden"},
		{"nested fence with inner closer", "- item\n  ```go\n    ```\n<details>\n\nhidden"},
		{"nested tilde fence", "- item\n  ~~~go\n\t~~~\n<details>\n\nhidden"},
		{"escaped opening backtick", "\\`<details>`\n\nhidden"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			group := renderGroup("nested", ActionPublish, SeverityP2, false, "First issue", tc.body)
			group.Suggestion = "Next suggestion"
			next := renderGroup("next", ActionPublish, SeverityP2, false, "Next issue", "Next body")
			out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary", Groups: []Group{group, next}, Fingerprint: "abc"})
			if err != nil {
				t.Fatal(err)
			}
			rendered := gitLabRenderedHTML(t, out)
			assertGitLabMarkupIsLiteral(t, rendered, "<details>")
			if !strings.Contains(rendered, ">[P2]Next issue<") || !strings.Contains(rendered, "</strong>：Next suggestion") {
				t.Errorf("untrusted Markdown hid the following content:\n%s", rendered)
			}
			assertGitLabVisible(t, rendered, "Next issue", "Next suggestion", "Next body")
		})
	}
}

func TestGitLabRenderedFencedBodyPreservesLiteralComment(t *testing.T) {
	group := renderGroup("code-comment", ActionPublish, SeverityP2, false, "Issue", "```\n<!-- literal -->\n```\n\nInline `<!-- inline -->`")
	out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary", Groups: []Group{group}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	rendered := gitLabRenderedHTML(t, out)
	if !strings.Contains(gitLabVisibleText(rendered), "<!-- literal -->") || !strings.Contains(gitLabVisibleText(rendered), "<!-- inline -->") {
		t.Errorf("comment in fenced body was changed:\n%s", rendered)
	}
	if strings.Contains(rendered, "<!-- literal -->") {
		t.Errorf("comment escaped its code fence:\n%s", rendered)
	}
}
