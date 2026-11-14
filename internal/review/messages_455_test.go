package review

import (
	"fmt"
	"strings"
	"testing"
)

func TestLocalizeScopeCompletenessMessages(t *testing.T) {
	cases := []struct {
		name    string
		message string
		want    []string
	}{
		{
			name:    "withheld too large keeps the path and reason",
			message: "review coverage gap: GitLab withheld the diff for css/reimagined.css (too_large); the file was not reviewed",
			want:    []string{"css/reimagined.css", "文件过大", "该文件未被评审"},
		},
		{
			name:    "withheld collapsed keeps the path and reason",
			message: "review coverage gap: GitLab withheld the diff for web/big.css (collapsed); the file was not reviewed",
			want:    []string{"web/big.css", "diff 被折叠", "该文件未被评审"},
		},
		{
			name:    "generated skip keeps count and paths",
			message: "review skipped 2 generated file(s), which were not reviewed: api.pb.go, gen/x.go",
			want:    []string{"2", "api.pb.go", "gen/x.go", "生成文件"},
		},
		{
			name:    "gitlab version truncation keeps counts",
			message: "review coverage gap: GitLab diff collection is incomplete (state=overflow, real_size=1000+): 3 of 10 changed file(s) were provided; the remaining files were not reviewed",
			want:    []string{"超出收集上限", "10", "3", "7", "未被评审"},
		},
		{
			name:    "gitlab-side truncation keeps counts",
			message: "diff collection was truncated by the GitLab diff limits: GitLab reported 10 changed file(s); 4 were delivered (3 included, 1 ignored by rules, 0 generated); 6 file(s) were not delivered and were not reviewed",
			want:    []string{"10", "4", "6", "未送达"},
		},
		{
			name:    "not collected states the missing proof",
			message: "diff collection is marked incomplete by GitLab (the diff version is not collected): 3 changed file(s) were provided; whether every changed file was reviewed cannot be proven",
			want:    []string{"3", "无法证明"},
		},
		{
			name:    "generated-only no-review count",
			message: "no reviewable files: 2 generated file(s) were skipped",
			want:    []string{"2", "生成文件"},
		},
		{
			name:    "out-of-scope severe finding keeps path and severity",
			message: "incremental review ignored a P1 finding outside the reviewed file set: src/cache.go",
			want:    []string{"src/cache.go", "P1", "范围外", "不覆盖"},
		},
		{
			name:    "hard omission keeps reasons and paths",
			message: "diff collection omitted 2 of 4 changed file(s): 2 included in the materials snapshot, 0 ignored by rules, 0 generated; reasons: max_files=1, max_diff_lines=1; omitted paths: c.go, d.go",
			want:    []string{"4", "2", "超过文件数量上限", "c.go", "d.go", "未进入评审的路径"},
		},
		{
			name:    "standalone summary reason keeps omitted path",
			message: "omitted 1 of 1 changed file(s) (included=0, ignored=0, generated=0) before review: [big.bin]; reasons: too_large=1",
			want:    []string{"1", "文件过大", "big.bin", "未进入评审的路径"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, ok := localizeMachineMessage(tc.message)
			if !ok {
				t.Fatalf("message was not mapped: %q", tc.message)
			}
			for _, want := range tc.want {
				if !strings.Contains(text, want) {
					t.Fatalf("localized text %q does not contain %q", text, want)
				}
			}
		})
	}
}

func TestUnknownScopeMessageDegradesSafely(t *testing.T) {
	if text, ok := localizeMachineMessage("totally unknown machine prose here"); ok || text != "" {
		t.Fatalf("unknown message matched: %q / %v", text, ok)
	}
	if got := localizeWarning("totally unknown machine prose here"); got != unknownWarningNotice {
		t.Fatalf("warning fallback = %q, want %q", got, unknownWarningNotice)
	}
}

func TestRenderSummaryDisclosesFullReviewScope(t *testing.T) {
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictPass, Summary: "clean", Fingerprint: "fp", Groups: []Group{},
		Scope: &ReviewScope{Full: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"### 评审范围", "全量评审"} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary misses %q:\n%s", want, out)
		}
	}
}

func TestRenderSummaryDisclosesIncrementalReviewScope(t *testing.T) {
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictPass, Summary: "clean", Fingerprint: "fp", Groups: []Group{},
		Scope: &ReviewScope{
			LastHead: testLastSHA, Head: testHeadSHA,
			Files: []string{"a.go", "b.go"}, FileCount: 3, Unlocated: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"增量评审", "上次 head 444444", "本次 head 111111", "覆盖 3 个文件",
		"结论仅覆盖本次增量变更的文件", "其中 2 条问题无法定位到具体文件",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary misses %q:\n%s", want, out)
		}
	}
}

func TestRenderSummaryDisclosesCappedScopeAndGeneratedFiles(t *testing.T) {
	files := make([]string, 0, maxScopeFiles)
	for i := 0; i < maxScopeFiles; i++ {
		files = append(files, fmt.Sprintf("pkg/f%03d.go", i))
	}
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictPass, Summary: "clean", Fingerprint: "fp", Groups: []Group{},
		Scope: &ReviewScope{
			LastHead: testLastSHA, Head: testHeadSHA,
			Files: files, FileCount: maxScopeFiles + 50,
		},
		Generated: 2,
		GeneratedPaths: []string{
			"api.pb.go",
			"gen/x.go",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		fmt.Sprintf("覆盖 %d 个文件", maxScopeFiles+50),
		fmt.Sprintf("范围文件列表超过 %d 条", maxScopeFiles),
		"生成文件：跳过 2 个，未进入评审：",
		"api.pb.go",
		"gen/x.go",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("summary misses %q:\n%s", want, out)
		}
	}
}

func TestDossierValidatesGeneratedPaths(t *testing.T) {
	dossier := validDossier(t)
	dossier.DiffSummary.Generated = 1
	dossier.DiffSummary.GeneratedPaths = []string{"api.pb.go"}
	if err := ValidateDossier(dossier); err != nil {
		t.Fatalf("valid generated paths rejected: %v", err)
	}
	dossier.DiffSummary.Generated = 0
	if err := ValidateDossier(dossier); err == nil {
		t.Fatal("generated paths beyond the generated count accepted")
	}
	dossier.DiffSummary.Generated = 2
	dossier.DiffSummary.GeneratedPaths = []string{"../escape.pb.go"}
	if err := ValidateDossier(dossier); err == nil {
		t.Fatal("invalid generated path accepted")
	}
}

func TestRenderSummaryKeepsGeneratedPathsThroughLocalization(t *testing.T) {
	out, err := RenderSummary(SummaryInput{
		Verdict: VerdictPass, Summary: "clean", Fingerprint: "fp", Groups: []Group{},
		Warnings: []string{"review skipped 2 generated file(s), which were not reviewed: api.pb.go, gen/x.go"},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api.pb.go", "gen/x.go", "2 个生成文件"} {
		if !strings.Contains(out, want) {
			t.Fatalf("generated warning loses %q:\n%s", want, out)
		}
	}
}

func TestInlineTruncationLinksToDetailURL(t *testing.T) {
	body := strings.Repeat("长正文内容。", 120)
	group := mkGroup("long-body", 20, "logic", SeverityP2, "很长的正文", body)
	const detail = "https://review.example/reviews/wf-455"
	out, err := RenderFinding(group, FindingRenderOptions{DetailURL: detail})
	if err != nil {
		t.Fatal(err)
	}
	link := fmt.Sprintf(`<a href="%s">控制台</a>`, detail)
	if !strings.Contains(out, link) {
		t.Fatalf("truncated inline body has no detail link %q:\n%s", link, out)
	}
	summary, err := RenderSummary(SummaryInput{
		Verdict: VerdictPass, Summary: "clean", Fingerprint: "fp", Groups: []Group{},
		DetailURL: detail,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, detail) || !strings.Contains(summary, "详情") {
		t.Fatalf("summary detail link differs from the inline link:\n%s", summary)
	}
	if briefBodyRunes != 400 {
		t.Fatalf("briefBodyRunes = %d, the product limit must stay 400", briefBodyRunes)
	}
}

func TestInlineTruncationWithoutURLKeepsConsoleHint(t *testing.T) {
	body := strings.Repeat("长正文内容。", 120)
	group := mkGroup("long-body", 20, "logic", SeverityP2, "很长的正文", body)
	out, err := RenderFinding(group, FindingRenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "完整正文见控制台") || strings.Contains(out, "<a href=") {
		t.Fatalf("fallback truncation notice is wrong:\n%s", out)
	}
}
