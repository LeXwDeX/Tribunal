package review

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func TestRenderMarkdownTableCellBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, markdown string
	}{
		{"body", "| first | second |\n| --- | --- |\n| `<a href=\"https://example.invalid/\">click</a> | @suntao` |"},
		{"header", "| `<!-- risk evidence --> | @suntao` |\n| --- | --- |"},
		{"list", "- intro\n\n  first | second\n  --- | ---\n  `<details> | @suntao`"},
		{"single column", "first\n| --- |\n`<details> | @suntao`"},
		{"numbered row", "first | second\n--- | ---\n1234567. `<details> | @suntao`"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderMarkdown(tc.markdown, 1000)
			for _, unsafe := range []string{"<a href=", "<details>", "<!-- risk evidence -->", "@suntao"} {
				if strings.Contains(got, unsafe) {
					t.Errorf("table cell contains unsafe %q:\n%s", unsafe, got)
				}
			}
			if !strings.Contains(got, "@<span></span>suntao") {
				t.Errorf("table mention was not neutralized:\n%s", got)
			}
		})
	}
}

func TestRenderMarkdownTableKeepsRealCode(t *testing.T) {
	markdown := "Inline `<tag> | @suntao` is code.\n\n| first | second |\n| --- | --- |\n| `<tag>\\|@suntao` | safe |\n- `<tag> | @suntao`\n# `<tag> | @suntao`\n\n| first | second |\n| --- | --- |\n```go\ncode\n```\nAfter fence: `<tag> | @suntao`"
	got := renderMarkdown(markdown, 1000)
	for _, want := range []string{"Inline `<tag> | @suntao` is code.", "| `<tag>\\|@suntao` | safe |", "- `<tag> | @suntao`", "# `<tag> | @suntao`", "After fence: `<tag> | @suntao`"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing intact code %q:\n%s", want, got)
		}
	}
}

func TestRenderGLIssueReferenceIsNeutralized(t *testing.T) {
	const reference = "GL-71"
	for _, tc := range []struct{ name, got string }{
		{"single line", escapeOneLine(reference, 100)},
		{"Markdown", renderMarkdown(reference, 100)},
	} {
		if !strings.Contains(tc.got, "GL-<span></span>71") {
			t.Errorf("%s did not split %s: %q", tc.name, reference, tc.got)
		}
	}
	if got := renderMarkdown("`GL-71`\n\n```\nGL-71\n```", 100); got != "`GL-71`\n\n```\nGL-71\n```" {
		t.Errorf("issue reference inside code changed: %q", got)
	}
}

func TestRenderRisksAcrossPublishedFields(t *testing.T) {
	attack := "| first | second |\n| --- | --- |\n| `<details> | @suntao` | GL-71"
	group := renderGroup("risk", ActionPublish, SeverityP1, false, "GL-71", attack)
	group.Suggestion = attack
	out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: attack, Groups: []Group{group}, Warnings: []string{"GL-71"}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "@<span></span>suntao") != 3 || strings.Count(out, "GL-<span></span>71") != 5 || strings.Contains(out, "<details>") {
		t.Errorf("summary, body, suggestion, title or warning was unsafe:\n%s", out)
	}
	group.PositionOK = true
	out, err = RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(out, "@<span></span>suntao") != 2 || strings.Count(out, "GL-<span></span>71") != 3 || strings.Contains(out, "<details>") {
		t.Errorf("inline body, suggestion or title was unsafe:\n%s", out)
	}
}

func TestGitLabRenderedTableCellsAreSafe(t *testing.T) {
	for _, tc := range []struct {
		name, markdown string
		visible        []string
	}{
		{"body", "| first | second |\n| --- | --- |\n| `<a href=\"https://example.invalid/\">click</a> | @suntao` |", []string{"<a href=\"https://example.invalid/\">click</a>", "@suntao"}},
		{"header", "| `<!-- risk evidence --> | @suntao` |\n| --- | --- |", []string{"<!-- risk evidence -->", "@suntao"}},
		{"list", "- intro\n\n  first | second\n  --- | ---\n  `<details> | @suntao`", []string{"<details>", "@suntao"}},
		{"numbered row", "first | second\n--- | ---\n1234567. `<details> | @suntao`", []string{"<details>", "@suntao"}},
		{"blockquote", "> first | second\n> --- | ---\n> `<details> | @suntao`", []string{"<details>", "@suntao"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := gitLabRenderedHTML(t, renderMarkdown(tc.markdown, 1000))
			text, tags := scanGitLabHTML(rendered)
			for _, tag := range tags {
				if strings.HasPrefix(tag, "<a ") || strings.HasPrefix(tag, "<details") || strings.Contains(tag, `data-reference-type=`) {
					t.Errorf("unsafe table element %q:\n%s", tag, rendered)
				}
			}
			if strings.Contains(rendered, "<!-- risk evidence -->") {
				t.Errorf("table hid the evidence as an HTML comment:\n%s", rendered)
			}
			for _, want := range tc.visible {
				if !strings.Contains(text, want) {
					t.Errorf("missing visible text %q:\n%s", want, rendered)
				}
			}
		})
	}
}

func TestGitLabRenderedTableKeepsCode(t *testing.T) {
	markdown := "Inline `<tag> | @suntao` is code.\n\n| first | second |\n| --- | --- |\n| `<tag>\\|@suntao` | safe |\n| `<tag>\\\\|@suntao` | safe |\n- `<tag> | @suntao`"
	rendered := gitLabRenderedHTML(t, renderMarkdown(markdown, 1000))
	text, tags := scanGitLabHTML(rendered)
	for _, want := range []string{"Inline <tag> | @suntao is code.", "<tag>|@suntao", "<tag>\\\\|@suntao"} {
		if !strings.Contains(text, want) {
			t.Errorf("code lost visible content %q:\n%s", want, rendered)
		}
	}
	if strings.Count(rendered, "<code ") != 4 {
		t.Errorf("code spans changed:\n%s", rendered)
	}
	for _, tag := range tags {
		if strings.Contains(tag, `data-reference-type=`) {
			t.Errorf("reference inside code was linked: %s", tag)
		}
	}
}

func TestGitLabRenderedTableAcrossPublishedFields(t *testing.T) {
	attack := "| first | second |\n| --- | --- |\n| `<a href=\"https://example.invalid/\">click</a> | @suntao` |\n| `<!-- risk evidence --> | evidence` |"
	group := renderGroup("gitlab-table", ActionPublish, SeverityP1, false, "Finding", attack)
	group.Suggestion = attack
	summary, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: attack, Groups: []Group{group}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	group.PositionOK = true
	finding, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, markdown string
		count          int
	}{{"summary fields", summary, 3}, {"finding fields", finding, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := gitLabRenderedHTML(t, tc.markdown)
			text, tags := scanGitLabHTML(rendered)
			for _, tag := range tags {
				if (strings.HasPrefix(tag, "<a ") && !strings.Contains(tag, `class="anchor"`)) || strings.Contains(tag, `data-reference-type=`) {
					t.Errorf("published table produced a link: %s", tag)
				}
			}
			for _, literal := range []string{`<a href="https://example.invalid/">click</a>`, "@suntao", "<!-- risk evidence -->"} {
				if strings.Count(text, literal) != tc.count {
					t.Errorf("published table did not retain %q %d times:\n%s", literal, tc.count, rendered)
				}
			}
			if strings.Contains(rendered, "<!-- risk evidence -->") {
				t.Error("published table hid evidence in an HTML comment")
			}
		})
	}
}

// This test requires an existing issue in the supplied project. It checks that
// GitLab links the unmodified reference before testing the rendered output.
func TestGitLabRenderedGLIssueReferenceIsLiteral(t *testing.T) {
	if os.Getenv("TRIBUNAL_TEST_GITLAB_MARKDOWN") != "1" {
		t.Skip("set TRIBUNAL_TEST_GITLAB_MARKDOWN=1 for the GitLab Markdown API")
	}
	project := os.Getenv("TRIBUNAL_TEST_GITLAB_ISSUE_PROJECT")
	iid := os.Getenv("TRIBUNAL_TEST_GITLAB_ISSUE_IID")
	if project == "" || iid == "" {
		t.Skip("set TRIBUNAL_TEST_GITLAB_ISSUE_PROJECT and TRIBUNAL_TEST_GITLAB_ISSUE_IID to an existing issue")
	}
	render := func(markdown string) string {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "glab", "api", "--hostname", "git.ycgame.com", "markdown", "-X", "POST", "-F", "gfm=true", "-f", "project="+project, "-f", "text="+markdown)
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
	reference := "GL-" + iid
	baseline := render(reference)
	if !strings.Contains(baseline, `data-reference-type="issue"`) || !strings.Contains(baseline, `data-original="`+reference+`"`) {
		t.Skip("GitLab did not resolve the supplied issue reference; no reproduction is established")
	}
	for _, tc := range []struct{ name, markdown string }{
		{"single line", escapeOneLine(reference, 100)},
		{"Markdown", renderMarkdown(reference, 100)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := render(tc.markdown)
			if strings.Contains(rendered, `data-reference-type="issue"`) || !strings.Contains(gitLabVisibleText(rendered), reference) {
				t.Error("issue reference was linked or changed")
			}
		})
	}
	code := render(renderMarkdown("`"+reference+"`", 100))
	if strings.Contains(code, `data-reference-type="issue"`) || !strings.Contains(gitLabVisibleText(code), reference) || !strings.Contains(code, "<code ") {
		t.Error("issue reference inside code was linked or changed")
	}
	group := renderGroup("gl-reference", ActionPublish, SeverityP1, false, reference, reference)
	group.Suggestion = reference
	summary, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: reference, Groups: []Group{group}, Warnings: []string{reference}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	group.PositionOK = true
	finding, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, markdown string
		count          int
	}{{"summary fields", summary, 5}, {"finding fields", finding, 3}} {
		t.Run(tc.name, func(t *testing.T) {
			rendered := render(tc.markdown)
			if strings.Contains(rendered, `data-reference-type="issue"`) || strings.Count(gitLabVisibleText(rendered), reference) != tc.count {
				t.Error("published references were linked or changed")
			}
		})
	}
}
