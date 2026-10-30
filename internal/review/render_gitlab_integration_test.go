package review

import (
	"encoding/json"
	"html"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// These opt-in tests send synthetic notes only to GitLab's Markdown renderer.
func gitLabRenderedHTML(t *testing.T, markdown string) string {
	t.Helper()
	if os.Getenv("TRIBUNAL_TEST_GITLAB_MARKDOWN") != "1" {
		t.Skip("set TRIBUNAL_TEST_GITLAB_MARKDOWN=1 for the GitLab Markdown API")
	}
	cmd := exec.Command("glab", "api", "--hostname", "git.ycgame.com", "markdown", "-X", "POST", "-F", "gfm=true", "-f", "project=suntao/tribunal-sandbox", "-f", "text="+markdown)
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

func gitLabSandboxHasLabel(t *testing.T, name string) bool {
	t.Helper()
	if os.Getenv("TRIBUNAL_TEST_GITLAB_MARKDOWN") != "1" {
		return false
	}
	cmd := exec.Command("glab", "api", "--hostname", "git.ycgame.com", "projects/suntao%2Ftribunal-sandbox/labels")
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
	const literal = sha + " suntao/tribunal-sandbox> @suntao #1 !1 $1 %r ~x &123 *x* _x_ <img>"
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
	if count := strings.Count(gitLabVisibleText(rendered), literal); count != 3 {
		t.Errorf("literal text appears %d times, want title, reason and warning:\n%s", count, rendered)
	}
	if !strings.Contains(gitLabVisibleText(rendered), "head "+sha) {
		t.Errorf("footer commit was shortened or hidden:\n%s", rendered)
	}
	if !strings.Contains(gitLabVisibleText(rendered), "profile "+sha) {
		t.Errorf("footer profile was shortened or hidden:\n%s", rendered)
	}
}

func TestGitLabRenderedInlineReferencesAndURLs(t *testing.T) {
	const sha = "bcdffc81e2b63cb028f0e0cfc1475430e6fc1cbc"
	const title = "bcdffc8 suntao/tribunal-sandbox> \\@suntao"
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
	const literal = "bcdffc81e2b63cb028f0e0cfc1475430e6fc1cbc suntao/tribunal-sandbox> @suntao #1 !1 $1 %release ~bug &123 https://example.invalid www.example.invalid person@example.invalid"
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
			if strings.Contains(rendered, "<details>") || !strings.Contains(rendered, ">[P2] Next issue<") || !strings.Contains(rendered, ">建议</strong>：Next suggestion") {
				t.Errorf("next finding or suggestion was hidden or HTML was injected:\n%s", rendered)
			}
			for _, want := range []string{"Next body", "Next warning", "internal/a  file.go"} {
				if !strings.Contains(gitLabVisibleText(rendered), want) {
					t.Errorf("missing rendered %q:\n%s", want, rendered)
				}
			}
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
			if strings.Contains(rendered, "<details>") || !strings.Contains(rendered, ">[P2] Next issue<") || !strings.Contains(rendered, ">建议</strong>：Next suggestion") {
				t.Errorf("later finding or suggestion was hidden:\n%s", rendered)
			}
			if field == "body" && !strings.Contains(rendered, ">建议</strong>：First suggestion") {
				t.Errorf("first suggestion was hidden:\n%s", rendered)
			}
			if !strings.Contains(gitLabVisibleText(rendered), "Next warning") {
				t.Errorf("warning was hidden:\n%s", rendered)
			}
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
			if strings.Contains(rendered, "<details>") || !strings.Contains(rendered, ">[P2] Next issue<") || !strings.Contains(rendered, ">建议</strong>：Next suggestion") {
				t.Errorf("untrusted Markdown hid the following content:\n%s", rendered)
			}
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
