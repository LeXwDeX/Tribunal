package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
)

func TestIgnoreDiffChecksDirectoryAncestors(t *testing.T) {
	for _, pattern := range []string{"private", "private/", "/private/**", "./private/**"} {
		t.Run(pattern, func(t *testing.T) {
			diffs := []gitlab.FileDiff{
				{OldPath: "private/a.go", NewPath: "private/a.go", Diff: "+CANARY_PRIVATE\n"},
				{OldPath: "private/old.go", NewPath: "public.go", RenamedFile: true, Diff: "-CANARY_OLD\n+new\n"},
				{OldPath: "outside.go", NewPath: "outside.go", Diff: "+allowed\n"},
			}
			files, ignored, summary, warnings := buildReviewFiles(diffs, []string{pattern}, 20, 200)
			if len(files) != 1 || files[0].Path != "outside.go" || strings.Contains(files[0].Diff, "CANARY") || len(ignored) != 2 || summary.Truncated || len(warnings) != 0 {
				t.Fatalf("files=%+v ignored=%v summary=%+v warnings=%v", files, ignored, summary, warnings)
			}
		})
	}
}

func TestUnicodeIgnoreExcludesModelPromptAndTool(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.Ignore = []string{"私密/**", "café/**"}
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	f.gitlab.diffs = append(f.gitlab.diffs, gitlab.FileDiff{NewPath: "私密/a.go", Diff: "+CANARY_DIFF_UNICODE\n"}, gitlab.FileDiff{NewPath: "café/b.go", Diff: "+CANARY_DIFF_ACCENT\n"})
	calls := 0
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		calls++
		text := spec.Prompt
		for _, block := range spec.System {
			text += block.Text
		}
		if strings.Contains(text, "CANARY_DIFF_") {
			t.Error("unicode diff reached prompt")
		}
		entries, err := os.ReadDir(filepath.Join(f.manager.Get(42).Root, "runs"))
		if err != nil || len(entries) != 1 {
			t.Fatalf("run dirs=%v err=%v", entries, err)
		}
		root := filepath.Join(f.manager.Get(42).Root, "runs", entries[0].Name())
		for _, dir := range []string{"私密", "café"} {
			if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, dir, "secret.go"), []byte("CANARY_TOOL_UNICODE\n"), 0600); err != nil {
				t.Fatal(err)
			}
			for _, tool := range spec.Tools {
				if tool.Def().Name == "read_file" {
					raw, _ := json.Marshal(map[string]string{"path": dir + "/secret.go"})
					result, err := tool.Call(ctx, raw)
					if err == nil || strings.Contains(result.Content, "CANARY_TOOL_UNICODE") {
						t.Errorf("read=%q err=%v", result.Content, err)
					}
				}
			}
		}
		return f.runner.Run(ctx, spec)
	}
	if err := f.service.RunReview(context.Background(), f.seedReview(t, "unicode-ignore")); err != nil {
		t.Fatal(err)
	}
	if calls == 0 {
		t.Fatal("model not called")
	}
}

func TestIgnoreDiffRejectsInvalidOldRenamePath(t *testing.T) {
	files, _, summary, _ := buildReviewFiles([]gitlab.FileDiff{{OldPath: "../private", NewPath: "safe.go", RenamedFile: true, Diff: "+CANARY\n"}}, []string{"private"}, 20, 200)
	if len(files) != 0 || !summary.Truncated || len(summary.Omitted) != 1 || summary.Omitted[0].Reason != "invalid_path" {
		t.Fatalf("files=%+v summary=%+v", files, summary)
	}
}

func TestIgnoreUnicodeDiffNeverEntersDossier(t *testing.T) {
	for _, dir := range []string{"私密", "café"} {
		files, ignored, _, _ := buildReviewFiles([]gitlab.FileDiff{{NewPath: dir + "/a.go", Diff: "+CANARY_UNICODE\n"}, {NewPath: "allowed.go", Diff: "+ok\n"}}, []string{dir + "/**"}, 10, 100)
		if len(files) != 1 || files[0].Path != "allowed.go" || len(ignored) != 1 {
			t.Errorf("%s files=%+v ignored=%v", dir, files, ignored)
		}
	}
}

func TestIgnoreDiffCoversNormalizedForms(t *testing.T) {
	tests := []struct {
		pattern string
		path    string
		ignored bool
	}{
		{"/private/**", "private/a.go", true},
		{"/private/**", "nested/private/a.go", false},
		{"./private/**", "private/a.go", true},
		{"vendor/", "nested/vendor/a.go", true},
		{"a/**/vendor/", "a/x/vendor/a.go", true},
		{"a/**/vendor/", "x/a/vendor/a.go", false},
	}
	for _, tc := range tests {
		t.Run(tc.pattern+":"+tc.path, func(t *testing.T) {
			files, ignored, _, warnings := buildReviewFiles(
				[]gitlab.FileDiff{{NewPath: tc.path, Diff: "+x\n"}}, []string{tc.pattern}, 10, 100)
			if len(warnings) != 0 {
				t.Fatalf("warnings = %v", warnings)
			}
			if got := len(ignored) == 1 && len(files) == 0; got != tc.ignored {
				t.Fatalf("ignored = %v, want %v (files=%+v ignored=%v)", got, tc.ignored, files, ignored)
			}
			if !tc.ignored && (len(files) != 1 || files[0].Path != tc.path) {
				t.Fatalf("allowed path was filtered: files=%+v ignored=%v", files, ignored)
			}
		})
	}
}
