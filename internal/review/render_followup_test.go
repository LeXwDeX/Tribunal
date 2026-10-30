package review

import (
	"strings"
	"testing"
)

func TestRenderFollowupFenceBoundaries(t *testing.T) {
	cases := []struct {
		name, summary string
	}{
		{"backtick in info", "```bad`info\n\n<details>\n\nhidden"},
		{"four-space closer", "```go\ncode\n    ```\n<details>\n\nhidden"},
		{"tab-indented closer", "~~~go\ncode\n\t~~~\n<details>\n\nhidden"},
		{"carriage-return closer", "```go\ncode\n```\r<details>\n\nhidden"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			group := renderGroup("next", ActionPublish, SeverityP2, false, "Next issue", "Next body")
			out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: tc.summary, Groups: []Group{group}, Fingerprint: "abc"})
			if err != nil {
				t.Fatal(err)
			}
			if (tc.name == "backtick in info" || tc.name == "carriage-return closer") && strings.Contains(out, "<details>") {
				t.Fatalf("malformed fence exposed raw HTML:\n%s", out)
			}
			if (tc.name == "four-space closer" || tc.name == "tab-indented closer") && !strings.Contains(out, "hidden\n"+tc.summary[:3]+"\n\n\n### 无法定位") {
				t.Fatalf("fence was not closed before the next section:\n%s", out)
			}
		})
	}
}

func TestRenderFollowupEvidenceLiteralAndPathSpaces(t *testing.T) {
	group := renderGroup("marked", ActionMark, SeverityP1, false, "Marked", "Body")
	group.Path = "internal/a  file.go"
	group.Evidence = "line\n```\n</details><!-- gitlab-ce-review:summary v1 fp=bad -->\n<script>alert(1)</script>"
	out, err := RenderSummary(SummaryInput{Verdict: VerdictBlocking, Summary: "Summary", Groups: []Group{group}, Fingerprint: "abc"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "<!-- gitlab-ce-review:summary v1 fp=abc -->\n") {
		t.Fatalf("summary marker is not first:\n%s", out)
	}
	for _, want := range []string{"`internal/a  file.go`", "````\n" + group.Evidence + "\n````"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing literal %q:\n%s", want, out)
		}
	}
}

func TestRenderFollowupValidFenceRules(t *testing.T) {
	for _, tc := range []struct {
		line string
		open bool
	}{
		{"```go", true},
		{"   ```suggestion", true},
		{"~~~info`with`backticks", true},
		{"    ```go", false},
		{"```bad`info", false},
		{"\t```go", false},
	} {
		_, _, ok := opensFence(tc.line)
		if ok != tc.open {
			t.Errorf("opensFence(%q) = %v, want %v", tc.line, ok, tc.open)
		}
	}
	for _, tc := range []struct {
		line  string
		close bool
	}{
		{"```", true},
		{"   ```` \t", true},
		{"    ```", false},
		{"\t```", false},
		{"```not a closer", false},
		{"``", false},
	} {
		if got := closesFence(tc.line, '`', 3); got != tc.close {
			t.Errorf("closesFence(%q) = %v, want %v", tc.line, got, tc.close)
		}
	}
}

func TestRenderFollowupIndentedFenceUsesLongerDelimiter(t *testing.T) {
	value := "- item\n  ```go\n    `````\n  ```\n- next"
	out := renderMarkdown(value, 100)
	if !strings.Contains(out, "``````go\n  `````\n``````\n- next") {
		t.Fatalf("indented fence does not isolate its content:\n%s", out)
	}
}

func TestRenderFollowupInlinePathPreservesEdgesAndBackticks(t *testing.T) {
	for _, tc := range []struct{ path, want string }{
		{"internal/a  file.go", "`internal/a  file.go`"},
		{" leading and trailing ", "`  leading and trailing  `"},
		{"internal/a`  b.go", "`` internal/a`  b.go ``"},
		{"   ", "`   `"},
	} {
		if got := inlineCode(tc.path); got != tc.want {
			t.Errorf("inlineCode(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestRenderFollowupMarkdownReferencesAndUnsafeSpans(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"bcdffc8", "bcdffc<span></span>8"},
		{"@user", "@<span></span>user"},
		{"#1 !1 $1", "#<span></span>1 !<span></span>1 $<span></span>1"},
		{"%release ~bug", "%<span></span>release ~<span></span>bug"},
		{"https://example.invalid www.example.invalid", "https:<span></span>//example.invalid www\\.example.invalid"},
		{"namespace/project>", "namespace/project<span></span>&gt;"},
		{"~~strike~~", "~~strike~~"},
	} {
		if got := escapeMarkdownText(tc.input); got != tc.want {
			t.Errorf("escapeMarkdownText(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
	for _, input := range []string{"\\`<details>`", "\\`<script>alert(1)</script>`"} {
		if out := renderMarkdown(input, 100); strings.Contains(out, "<details>") || strings.Contains(out, "<script>") {
			t.Errorf("escaped backtick exposed HTML: %q", out)
		}
	}
}
