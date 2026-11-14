package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const (
	scopeHeadSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	scopeStartSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// rawFileGitLab overrides raw repository reads so one path can return a
// different body per ref.
type rawFileGitLab struct {
	*reviewGitLab
	bodies map[string]map[string]string
	errs   map[string]error
	calls  int
}

func (f *rawFileGitLab) GetRawFile(_ context.Context, _ string, _ int64, path, ref string) ([]byte, error) {
	f.calls++
	if err, ok := f.errs[path]; ok {
		return nil, err
	}
	if refs, ok := f.bodies[path]; ok {
		if body, ok := refs[ref]; ok {
			return []byte(body), nil
		}
	}
	return nil, &gitlab.Error{Op: "get_raw_file", Status: 404}
}

func newScopeService(fake *rawFileGitLab) *ReviewService {
	return &ReviewService{gitlab: fake}
}

func TestApplyDiffScopeReportsGitLabTruncation(t *testing.T) {
	fake := &rawFileGitLab{reviewGitLab: newReviewGitLab(scopeHeadSHA)}
	service := newScopeService(fake)
	diffs := []gitlab.FileDiff{{NewPath: "a.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "overflow", RealSize: "12"}
	if err := service.applyDiffScope(context.Background(), 42, "token", version, "12", diffs); err != nil {
		t.Fatalf("applyDiffScope: %v", err)
	}
	if diffs[0].Collection == nil || diffs[0].Collection.Total != 12 || !diffs[0].Collection.Truncated {
		t.Fatalf("collection = %+v, want total 12 truncated", diffs[0].Collection)
	}
	if fake.calls != 0 {
		t.Fatalf("raw file reads = %d, want none for a complete diff list", fake.calls)
	}
	_, _, summary, warnings := buildReviewFiles(diffs, nil, 200, 1000)
	if summary.Total != 12 || !summary.Truncated {
		t.Fatalf("summary = %+v, want the GitLab-reported total and truncation", summary)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "overflow") {
		t.Fatalf("warnings = %v, want the version state disclosed", warnings)
	}
}

func TestApplyDiffScopeUsesChangesCountWhenRealSizeMissing(t *testing.T) {
	fake := &rawFileGitLab{reviewGitLab: newReviewGitLab(scopeHeadSHA)}
	service := newScopeService(fake)
	diffs := []gitlab.FileDiff{{NewPath: "a.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "without_files"}
	if err := service.applyDiffScope(context.Background(), 42, "token", version, "50+", diffs); err != nil {
		t.Fatalf("applyDiffScope: %v", err)
	}
	if diffs[0].Collection == nil || diffs[0].Collection.Total != 50 || !diffs[0].Collection.Truncated {
		t.Fatalf("collection = %+v, want changes_count fallback", diffs[0].Collection)
	}
}

func TestApplyDiffScopeRejectsEmptyTruncatedDiffList(t *testing.T) {
	fake := &rawFileGitLab{reviewGitLab: newReviewGitLab(scopeHeadSHA)}
	service := newScopeService(fake)
	version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "without_files", RealSize: "0"}
	err := service.applyDiffScope(context.Background(), 42, "token", version, "4", nil)
	if err == nil {
		t.Fatal("empty diff list with reported changes must be retryable")
	}
	if IsPermanentJob(err) {
		t.Fatalf("error must be retryable, got %v", err)
	}
	if !strings.Contains(err.Error(), "without_files") {
		t.Fatalf("error does not name the GitLab version state: %v", err)
	}
}

func TestApplyDiffScopeAcceptsTrulyEmptyMergeRequest(t *testing.T) {
	fake := &rawFileGitLab{reviewGitLab: newReviewGitLab(scopeHeadSHA)}
	service := newScopeService(fake)
	version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "collected", RealSize: "0"}
	if err := service.applyDiffScope(context.Background(), 42, "token", version, "0", nil); err != nil {
		t.Fatalf("empty merge request rejected: %v", err)
	}
}

func TestApplyDiffScopeRecoversWithheldFileBody(t *testing.T) {
	fake := &rawFileGitLab{
		reviewGitLab: newReviewGitLab(scopeHeadSHA),
		bodies: map[string]map[string]string{
			"css/big.css": {
				scopeStartSHA: "alpha\nbeta\ngamma\n",
				scopeHeadSHA:  "alpha\nBETA\nadded\ngamma\n",
			},
		},
	}
	service := newScopeService(fake)
	diffs := []gitlab.FileDiff{{OldPath: "css/big.css", NewPath: "css/big.css", TooLarge: true}}
	version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "collected", RealSize: "1"}
	if err := service.applyDiffScope(context.Background(), 42, "token", version, "1", diffs); err != nil {
		t.Fatalf("applyDiffScope: %v", err)
	}
	if !strings.Contains(diffs[0].Diff, "@@ -1,3 +1,4 @@") ||
		!strings.Contains(diffs[0].Diff, "-beta") || !strings.Contains(diffs[0].Diff, "+BETA") {
		t.Fatalf("recovered diff = %q", diffs[0].Diff)
	}
	files, _, summary, warnings := buildReviewFiles(diffs, nil, 200, 1000)
	if len(files) != 1 || files[0].Path != "css/big.css" {
		t.Fatalf("files = %+v, want the recovered file reviewed", files)
	}
	if files[0].Additions != 2 || files[0].Deletions != 1 {
		t.Fatalf("additions/deletions = %d/%d, want 2/1", files[0].Additions, files[0].Deletions)
	}
	if summary.Truncated || len(summary.Omitted) != 0 {
		t.Fatalf("summary = %+v, want no omission after recovery", summary)
	}
	if len(warnings) != 0 {
		t.Fatalf("warnings = %v, want none after recovery", warnings)
	}
}

func TestBuildReviewFilesWithheldFileStaysVisibleWhenBodyUnavailable(t *testing.T) {
	fake := &rawFileGitLab{
		reviewGitLab: newReviewGitLab(scopeHeadSHA),
		errs:         map[string]error{"css/big.css": errors.New("upstream unavailable")},
	}
	service := newScopeService(fake)
	diffs := []gitlab.FileDiff{{OldPath: "css/big.css", NewPath: "css/big.css", Collapsed: true}}
	version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "collected", RealSize: "1"}
	if err := service.applyDiffScope(context.Background(), 42, "token", version, "1", diffs); err != nil {
		t.Fatalf("applyDiffScope: %v", err)
	}
	files, _, summary, warnings := buildReviewFiles(diffs, nil, 200, 1000)
	if len(files) != 0 || len(summary.Omitted) != 1 || summary.Omitted[0].Reason != "collapsed" {
		t.Fatalf("files/omitted = %+v / %+v", files, summary.Omitted)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "css/big.css") || !strings.Contains(joined, "not reviewed") {
		t.Fatalf("warnings = %v, want the unreviewed file disclosed", warnings)
	}
}

func TestApplyDiffScopeRejectsBinaryAndOversizedBodies(t *testing.T) {
	cases := []struct {
		name string
		head string
	}{
		{name: "binary", head: "alpha\x00beta\n"},
		{name: "invalid utf8", head: "alpha\xffbeta\n"},
		{name: "oversized", head: strings.Repeat("x", maxRecoveredFileBytes+1) + "\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &rawFileGitLab{
				reviewGitLab: newReviewGitLab(scopeHeadSHA),
				bodies:       map[string]map[string]string{"big.css": {scopeStartSHA: "alpha\n", scopeHeadSHA: tc.head}},
			}
			service := newScopeService(fake)
			diffs := []gitlab.FileDiff{{OldPath: "big.css", NewPath: "big.css", TooLarge: true}}
			version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "collected", RealSize: "1"}
			if err := service.applyDiffScope(context.Background(), 42, "token", version, "1", diffs); err != nil {
				t.Fatalf("applyDiffScope: %v", err)
			}
			if strings.TrimSpace(diffs[0].Diff) != "" {
				t.Fatalf("unreviewable body was recovered: %q", diffs[0].Diff)
			}
			files, _, summary, _ := buildReviewFiles(diffs, nil, 200, 1000)
			if len(files) != 0 || len(summary.Omitted) != 1 {
				t.Fatalf("files/omitted = %+v / %+v", files, summary.Omitted)
			}
		})
	}
}

func TestApplyDiffScopeStateAloneMarksTruncated(t *testing.T) {
	fake := &rawFileGitLab{reviewGitLab: newReviewGitLab(scopeHeadSHA)}
	service := newScopeService(fake)
	diffs := []gitlab.FileDiff{{NewPath: "a.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	// The reported count matches the delivered list; only the version state
	// says the collection is not complete.
	version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "overflow", RealSize: "1"}
	if err := service.applyDiffScope(context.Background(), 42, "token", version, "1", diffs); err != nil {
		t.Fatalf("applyDiffScope: %v", err)
	}
	_, _, summary, _ := buildReviewFiles(diffs, nil, 200, 1000)
	if !summary.Truncated || summary.Total != 1 {
		t.Fatalf("summary = %+v, want a truncated collection", summary)
	}
}

func TestApplyDiffScopeRecoversAddedAndDeletedFiles(t *testing.T) {
	cases := []struct {
		name      string
		diff      gitlab.FileDiff
		bodies    map[string]string
		wantHunk  string
		wantPlus  int
		wantMinus int
	}{
		{
			name: "added", diff: gitlab.FileDiff{NewPath: "new.css", NewFile: true, TooLarge: true},
			bodies:   map[string]string{scopeHeadSHA: "one\ntwo\n"},
			wantHunk: "@@ -0,0 +1,2 @@", wantPlus: 2,
		},
		{
			name: "deleted", diff: gitlab.FileDiff{OldPath: "old.css", NewPath: "old.css", DeletedFile: true, Collapsed: true},
			bodies:   map[string]string{scopeStartSHA: "one\ntwo\n"},
			wantHunk: "@@ -1,2 +0,0 @@", wantMinus: 2,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &rawFileGitLab{
				reviewGitLab: newReviewGitLab(scopeHeadSHA),
				bodies:       map[string]map[string]string{tc.diff.NewPath: tc.bodies},
			}
			service := newScopeService(fake)
			diffs := []gitlab.FileDiff{tc.diff}
			version := &gitlab.Version{HeadSHA: scopeHeadSHA, StartSHA: scopeStartSHA, State: "collected", RealSize: "1"}
			if err := service.applyDiffScope(context.Background(), 42, "token", version, "1", diffs); err != nil {
				t.Fatalf("applyDiffScope: %v", err)
			}
			if !strings.Contains(diffs[0].Diff, tc.wantHunk) {
				t.Fatalf("diff = %q, want hunk %q", diffs[0].Diff, tc.wantHunk)
			}
			files, _, _, _ := buildReviewFiles(diffs, nil, 200, 1000)
			if len(files) != 1 || files[0].Additions != tc.wantPlus || files[0].Deletions != tc.wantMinus {
				t.Fatalf("files = %+v, want additions=%d deletions=%d", files, tc.wantPlus, tc.wantMinus)
			}
		})
	}
}

func TestBuildReviewFilesSkipsGeneratedWithWarning(t *testing.T) {
	diffs := []gitlab.FileDiff{
		{NewPath: "api.pb.go", GeneratedFile: true, Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		{NewPath: "main.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
	}
	files, _, summary, warnings := buildReviewFiles(diffs, nil, 1, 1000)
	if len(files) != 1 || files[0].Path != "main.go" {
		t.Fatalf("files = %+v, want only the non-generated file", files)
	}
	if summary.Generated != 1 || summary.Total != 2 || summary.Truncated {
		t.Fatalf("summary = %+v, want one generated path counted, not truncated", summary)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "api.pb.go") || !strings.Contains(joined, "generated") {
		t.Fatalf("warnings = %v, want the skipped generated path", warnings)
	}
}

func TestBuildReviewFilesAllGeneratedLeavesNoReviewableFile(t *testing.T) {
	diffs := []gitlab.FileDiff{{NewPath: "api.pb.go", GeneratedFile: true, Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	files, _, summary, _ := buildReviewFiles(diffs, nil, 200, 1000)
	if len(files) != 0 || summary.Generated != 1 || summary.Truncated || len(summary.Omitted) != 0 {
		t.Fatalf("files/summary = %+v / %+v", files, summary)
	}
}

func TestEmptyReviewOutcomeDistinguishesGeneratedFiles(t *testing.T) {
	inputs := &reviewInputs{
		MR:      &gitlab.MergeRequest{IID: 7},
		Version: &gitlab.Version{HeadSHA: scopeHeadSHA},
		Diffs:   []gitlab.FileDiff{{NewPath: "api.pb.go", GeneratedFile: true}},
	}
	cfg := effectiveReviewConfig{}
	verdict, detail, text, reason, warnings := emptyReviewOutcome(inputs, cfg, review.DiffSummary{Generated: 1, Total: 1}, nil)
	if verdict != review.VerdictPass || detail != store.StatusDetailNoReviewable {
		t.Fatalf("verdict/detail = %q/%q, want a no-review PASS", verdict, detail)
	}
	if !strings.Contains(text, "生成文件") {
		t.Fatalf("summary = %q, want the generated-file reason", text)
	}
	if reason != "" {
		t.Fatalf("incomplete reason = %q, want none", reason)
	}
	if !strings.Contains(strings.Join(warnings, "\n"), "generated") {
		t.Fatalf("warnings = %v, want the generated-file notice", warnings)
	}

	empty := &reviewInputs{MR: &gitlab.MergeRequest{IID: 7}, Version: &gitlab.Version{HeadSHA: scopeHeadSHA}}
	verdict, detail, _, reason, _ = emptyReviewOutcome(empty, cfg, review.DiffSummary{}, nil)
	if verdict != review.VerdictIncomplete || detail != store.StatusDetailEmptyDiff || reason != "empty diff" {
		t.Fatalf("empty diff = %q/%q/%q", verdict, detail, reason)
	}
}

func TestReviewAllGeneratedFilesPublishNoReview(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{
		NewPath: "api.pb.go", GeneratedFile: true, Diff: "@@ -1 +1 @@\n-a\n+b\n",
	}}
	job := f.seedReview(t, "job-all-generated")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if _, err := f.db.GetWorkflowByJob(context.Background(), job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("all-generated review created a workflow: %v", err)
	}
	if len(f.gitlab.notes) != 1 {
		t.Fatalf("notes = %d, want one summary", len(f.gitlab.notes))
	}
	body := f.gitlab.notes[0].Body
	for _, want := range []string{"无需评审", "生成文件"} {
		if !strings.Contains(body, want) {
			t.Fatalf("all-generated summary misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "评审未完成") {
		t.Fatalf("all-generated review was not a no-review result:\n%s", body)
	}
}

func TestPublisherDisclosesScopeGeneratedAndInlineDetailLink(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.publisher.publicURL = "https://review.example"
	f.gitlab.diffs = []gitlab.FileDiff{
		{NewPath: "main.go", Diff: "@@ -1,2 +1,3 @@\n a\n+bad := 1\n b\n"},
		{NewPath: "api.pb.go", GeneratedFile: true, Diff: "@@ -1 +1 @@\n-a\n+b\n"},
	}
	f.runner.findings = []review.FindingOut{{
		Path: "main.go", Line: 2, Side: "new", Severity: review.SeverityP2,
		Category: "correctness", Title: "长正文问题", Body: strings.Repeat("长正文内容。", 120),
		Evidence: "bad := 1", Confidence: review.ConfidenceHigh,
	}}
	job := f.seedReview(t, "job-scope-link")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	detail := "https://review.example/reviews/" + workflow.ID

	summary := ""
	for _, note := range f.gitlab.notes {
		if strings.Contains(note.Body, "### 评审范围") {
			summary = note.Body
		}
	}
	if summary == "" {
		t.Fatalf("summary does not disclose the review scope: %+v", f.gitlab.notes)
	}
	for _, want := range []string{
		"全量评审", "生成文件：跳过 1 个，未进入评审：api.pb.go",
		fmt.Sprintf(`<a href="%s">详情</a>`, detail),
	} {
		if !strings.Contains(summary, want) {
			t.Fatalf("summary misses %q:\n%s", want, summary)
		}
	}

	if len(f.gitlab.discussions) != 1 {
		t.Fatalf("discussions = %d, want one inline finding", len(f.gitlab.discussions))
	}
	body := f.gitlab.discussions[0].Notes[0].Body
	if !strings.Contains(body, "内容已截断") {
		t.Fatalf("long inline body was not clipped:\n%s", body)
	}
	link := fmt.Sprintf(`<a href="%s">控制台</a>`, detail)
	if !strings.Contains(body, link) {
		t.Fatalf("inline body misses the detail link %q:\n%s", link, body)
	}
}

func TestParseSettingsRejectsMaxFilesAboveHardLimit(t *testing.T) {
	raw := json.RawMessage(fmt.Sprintf(`{"max_files":%d}`, review.HardMaxFiles+1))
	if _, err := ParseSettings(raw, nil); err == nil {
		t.Fatal("max_files above the hard limit was accepted")
	}
	allowed := json.RawMessage(fmt.Sprintf(`{"max_files":%d}`, review.HardMaxFiles))
	if _, err := ParseSettings(allowed, nil); err != nil {
		t.Fatalf("max_files at the hard limit rejected: %v", err)
	}
}
