package app

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestRenameFollowupAmbiguousPathFallsBackToSummary(t *testing.T) {
	version := gitlab.Version{BaseSHA: strings.Repeat("1", 40), StartSHA: strings.Repeat("2", 40), HeadSHA: strings.Repeat("3", 40)}
	index := gitlab.BuildDiffIndex(version, []gitlab.FileDiff{
		renameDiff("b.go", "c.go", "@@ -1 +1 @@\n-old\n+new\n"),
		renameDiff("a.go", "b.go", "@@ -1 +1 @@\n-old\n+new\n"),
	})
	groups := markPositions([]review.Group{
		{Path: "b.go", Line: 1, Side: review.SideOld},
		{Path: "b.go", Line: 1, Side: review.SideNew},
		{Path: "a.go", Line: 1, Side: review.SideOld},
		{Path: "c.go", Line: 1, Side: review.SideNew},
	}, index)
	for i, want := range []bool{false, false, true, true} {
		if groups[i].PositionOK != want {
			t.Errorf("group %d position_ok = %v, want %v", i, groups[i].PositionOK, want)
		}
	}
}

func TestRenameFollowupMissingOldRenamePathCannotPosition(t *testing.T) {
	d := testDossier(t, reviewTestHead)
	d.Files = []review.FileChange{
		{Path: "new.go", Status: "renamed", Diff: "@@ -1 +1 @@\n-old\n+new\n"},
		{Path: "other.go", Status: "modified", Diff: "@@ -1 +1 @@\n-old\n+new\n"},
	}
	index := dossierDiffIndex(d.DiffVersion, d)
	groups := markPositions([]review.Group{
		{Path: "new.go", Line: 1, Side: review.SideOld},
		{Path: "new.go", Line: 1, Side: review.SideNew},
		{Path: "other.go", Line: 1, Side: review.SideNew},
	}, index)
	for _, group := range groups {
		if group.PositionOK {
			t.Fatalf("rename with unavailable old path got a position: %+v", group)
		}
	}
}

func TestRenameFollowupLegacyTruncatedSnapshotCannotProveProvenance(t *testing.T) {
	d := testDossier(t, reviewTestHead)
	d.DiffSummary.Truncated = true
	d.DiffSummary.Omitted = nil
	index := dossierDiffIndex(d.DiffVersion, d)
	groups := markPositions([]review.Group{{Path: d.Files[0].Path, Line: 2, Side: review.SideNew}}, index)
	if groups[0].PositionOK {
		t.Fatalf("legacy snapshot with unknown omissions got a position: %+v", groups[0])
	}
}

func TestRenameFollowupIncompleteRenameIsCoverageGap(t *testing.T) {
	for _, diff := range []gitlab.FileDiff{
		{NewPath: "new.go", RenamedFile: true, Diff: "@@ -1 +1 @@\n-old\n+new\n"},
		{OldPath: "old.go", RenamedFile: true, Diff: "@@ -1 +1 @@\n-old\n+new\n"},
	} {
		files, _, summary, _ := buildReviewFiles([]gitlab.FileDiff{diff}, nil, 10, 100)
		if len(files) != 0 || !summary.Truncated || len(summary.Omitted) != 1 {
			t.Fatalf("incomplete rename accepted: files=%+v summary=%+v", files, summary)
		}
	}
}

func TestRenameFollowupDistinctSideFindingsSurvivePublication(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.gitlab.diffs = []gitlab.FileDiff{
		renameDiff("b.go", "c.go", "@@ -1 +1 @@\n-old\n+new\n"),
		renameDiff("a.go", "b.go", "@@ -1 +1 @@\n-old\n+new\n"),
	}
	run := f.runner.Run
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		result, err := run(ctx, spec)
		if err == nil {
			out := result.Output.(*review.CodeReviewOut)
			old := out.Findings[0]
			old.Path, old.Side, old.Line = "b.go", "old", 1
			newSide := old
			newSide.Side = "new"
			out.Findings = []review.FindingOut{old, newSide}
		}
		return result, err
	}
	job := f.seedReview(t, "job-rename-distinct-findings")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Verdict != review.VerdictIncomplete || len(f.gitlab.statuses) != 1 || f.gitlab.statuses[0].State != "failed" {
		t.Fatalf("ambiguous provenance must not green-light the review: verdict=%q statuses=%+v", wf.Verdict, f.gitlab.statuses)
	}
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) != 2 || findings[0].PublishKey == findings[1].PublishKey {
		t.Fatalf("stored findings=%+v err=%v", findings, err)
	}
	if f.gitlab.discussionCreates != 0 || len(f.gitlab.notes) != 1 {
		t.Fatalf("discussions=%d notes=%+v", f.gitlab.discussionCreates, f.gitlab.notes)
	}
	body := f.gitlab.notes[0].Body
	if strings.Count(body, "bad assignment") != 2 || !strings.Contains(body, "旧侧") || !strings.Contains(body, "新侧") {
		t.Fatalf("summary did not distinguish both findings:\n%s", body)
	}
	if !strings.Contains(body, "文件归属存在歧义") {
		t.Fatalf("summary did not explain the ambiguous provenance:\n%s", body)
	}
}

func TestRenameFollowupEmptyIncompleteSetsFailedCommitStatus(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.defaults.MaxDiffLines = 1
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.gitlab.diffs = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1 +1 @@\n-old\n+new\n"}}
	f.gitlab.statuses = []gitlab.CommitStatus{{Name: "ai-review", State: "success"}}
	api := &mrSerialGitLab{reviewGitLab: f.gitlab}
	f.publisher.gitlab = api
	job := f.seedReview(t, "job-first-file-omitted-status")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "评审未完成") {
		t.Fatalf("summary notes = %+v", f.gitlab.notes)
	}
	if len(f.gitlab.statuses) != 2 || f.gitlab.statuses[1].State != "failed" || f.gitlab.statuses[1].Name != "ai-review" {
		t.Fatalf("statuses = %+v, want failed status after prior success", f.gitlab.statuses)
	}
	var statusWrites []mrSerialWrite
	for _, write := range api.snapshot() {
		if write.kind == "commit-status" {
			statusWrites = append(statusWrites, write)
		}
	}
	if len(statusWrites) != 1 || statusWrites[0].sha != f.head || statusWrites[0].state != "failed" {
		t.Fatalf("status writes = %+v, want failed status for head %s", statusWrites, f.head)
	}
}

func TestRenameFollowupOmittedOtherChangeCannotProvideAmbiguousPosition(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.MaxFiles = 1
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.gitlab.diffs = []gitlab.FileDiff{
		renameDiff("b.go", "c.go", "@@ -1 +1 @@\n-old\n+new\n"),
		renameDiff("a.go", "b.go", "@@ -1 +1 @@\n-old\n+new\n"),
	}
	run := f.runner.Run
	f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
		result, err := run(ctx, spec)
		if err == nil {
			out := result.Output.(*review.CodeReviewOut)
			out.Findings[0].Path, out.Findings[0].Side, out.Findings[0].Line = "b.go", "old", 1
		}
		return result, err
	}
	job := f.seedReview(t, "job-omitted-rename-collision")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: wf.ID})
	if err != nil || len(findings) != 1 || findings[0].PositionOK == nil || *findings[0].PositionOK {
		t.Fatalf("ambiguous omitted-path findings=%+v err=%v", findings, err)
	}
	if f.gitlab.discussionCreates != 0 || len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "bad assignment") {
		t.Fatalf("finding was not preserved in the summary: discussions=%d notes=%+v", f.gitlab.discussionCreates, f.gitlab.notes)
	}
}

func TestRenameFollowupExcludedOldPathCannotAliasIncludedNewSide(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		t.Run(map[bool]string{false: "omitted", true: "ignored"}[ignored], func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			if ignored {
				f.defaults.Ignore = []string{"c.go"}
			} else {
				f.defaults.MaxFiles = 1
			}
			f.service.reviewDefaults = f.defaults
			f.publisher.reviewDefaults = f.defaults
			f.gitlab.diffs = []gitlab.FileDiff{
				renameDiff("a.go", "b.go", "@@ -1 +1 @@\n-old\n+new\n"),
				renameDiff("b.go", "c.go", "@@ -1 +1 @@\n-old\n+new\n"),
			}
			run := f.runner.Run
			f.service.agentRun = func(ctx context.Context, spec agent.RunSpec) (*agent.RunResult, error) {
				result, err := run(ctx, spec)
				if err == nil {
					out := result.Output.(*review.CodeReviewOut)
					out.Findings[0].Path, out.Findings[0].Side, out.Findings[0].Line = "b.go", "new", 1
				}
				return result, err
			}
			job := f.seedReview(t, "job-excluded-old-alias")
			if err := f.service.RunReview(context.Background(), job); err != nil {
				t.Fatal(err)
			}
			wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
			if err != nil {
				t.Fatal(err)
			}
			want := review.VerdictIncomplete
			if ignored {
				want = review.VerdictPass
			}
			if wf.Verdict != want {
				t.Fatalf("verdict = %q, want %q", wf.Verdict, want)
			}
			if f.gitlab.discussionCreates != 0 || len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "bad assignment") {
				t.Fatalf("excluded old alias was posted inline or lost: discussions=%d notes=%+v", f.gitlab.discussionCreates, f.gitlab.notes)
			}
		})
	}
}

func TestRenameFollowupEmptyStatusFailureRetriesWithoutDuplicatingSummary(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	api := usePartialGitLab(f)
	api.fail(nil, errSummaryTransport)
	control := seedSummaryControl(t, f)
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	inputs := &reviewInputs{MR: f.gitlab.mr, Version: f.gitlab.version}
	summary := review.DiffSummary{Total: 1, Truncated: true, Omitted: []review.OmittedFile{{Path: "a.go", Reason: "max_diff_lines"}}}
	cfg := effectiveReviewConfig{Settings: f.defaults}
	if err := f.service.publishEmpty(context.Background(), project, "token", control, inputs, cfg, summary, nil); !errors.Is(err, errSummaryTransport) {
		t.Fatalf("first publication error = %v, want status transport failure", err)
	}
	if len(f.gitlab.notes) != 1 || len(f.gitlab.statuses) != 0 {
		t.Fatalf("partial publication notes=%+v statuses=%+v", f.gitlab.notes, f.gitlab.statuses)
	}
	api.fail(nil, nil)
	if err := f.service.publishEmpty(context.Background(), project, "token", control, inputs, cfg, summary, nil); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(f.gitlab.notes) != 1 || len(f.gitlab.statuses) != 1 || f.gitlab.statuses[0].State != "failed" {
		t.Fatalf("retry notes=%+v statuses=%+v", f.gitlab.notes, f.gitlab.statuses)
	}
}

func TestRenameFollowupEmptyStatusRetryRechecksGeneration(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	control := seedSummaryControl(t, f)
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	api := &statusRetryGitLab{reviewGitLab: f.gitlab, bump: mrSerialBumpControl(t, f)}
	f.publisher.gitlab, f.publisher.retries = api, 2
	inputs := &reviewInputs{MR: f.gitlab.mr, Version: f.gitlab.version}
	summary := review.DiffSummary{Total: 1, Truncated: true, Omitted: []review.OmittedFile{{Path: "a.go", Reason: "max_diff_lines"}}}
	err = f.service.publishEmpty(context.Background(), project, "token", control, inputs, effectiveReviewConfig{Settings: f.defaults}, summary, nil)
	if !errors.Is(err, ErrStalePublication) || api.attempts() != 1 || len(f.gitlab.statuses) != 0 {
		t.Fatalf("status retry err=%v attempts=%d statuses=%+v", err, api.attempts(), f.gitlab.statuses)
	}
}

type emptySummaryBumpGitLab struct {
	*reviewGitLab
	bump func()
}

func (g *emptySummaryBumpGitLab) CreateMRNote(ctx context.Context, token string, projectID, iid int64, body string) (*gitlab.Note, error) {
	note, err := g.reviewGitLab.CreateMRNote(ctx, token, projectID, iid, body)
	if err == nil && strings.HasPrefix(body, review.SummaryMarkerPrefix) {
		g.bump()
	}
	return note, err
}

func TestRenameFollowupEmptyStatusChecksGenerationAfterSummary(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	control := seedSummaryControl(t, f)
	project, err := f.db.GetProject(context.Background(), 42)
	if err != nil {
		t.Fatal(err)
	}
	f.publisher.gitlab = &emptySummaryBumpGitLab{reviewGitLab: f.gitlab, bump: mrSerialBumpControl(t, f)}
	inputs := &reviewInputs{MR: f.gitlab.mr, Version: f.gitlab.version}
	summary := review.DiffSummary{Total: 1, Truncated: true, Omitted: []review.OmittedFile{{Path: "a.go", Reason: "max_diff_lines"}}}
	err = f.service.publishEmpty(context.Background(), project, "token", control, inputs, effectiveReviewConfig{Settings: f.defaults}, summary, nil)
	if !errors.Is(err, ErrStalePublication) || len(f.gitlab.notes) != 1 || len(f.gitlab.statuses) != 0 {
		t.Fatalf("stale after summary: err=%v notes=%+v statuses=%+v", err, f.gitlab.notes, f.gitlab.statuses)
	}
}

func TestRenameFollowupEmptyStatusRespectsDisabledAndIgnored(t *testing.T) {
	for _, ignored := range []bool{false, true} {
		f := newReviewFixture(t, reviewTestHead)
		f.defaults.CommitStatus = ignored
		if ignored {
			f.defaults.Ignore = []string{"a.go"}
		} else {
			f.defaults.MaxDiffLines = 1
		}
		f.service.reviewDefaults = f.defaults
		f.publisher.reviewDefaults = f.defaults
		f.gitlab.diffs = []gitlab.FileDiff{{OldPath: "a.go", NewPath: "a.go", Diff: "@@ -1 +1 @@\n-old\n+new\n"}}
		job := f.seedReview(t, "job-empty-status-disabled")
		if err := f.service.RunReview(context.Background(), job); err != nil {
			t.Fatal(err)
		}
		if len(f.gitlab.statuses) != 0 {
			t.Fatalf("ignored=%v: unexpected statuses %+v", ignored, f.gitlab.statuses)
		}
	}
}

func TestRenameFollowupRejectsOldAndUnknownRenderIdentity(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	current := f.bundle
	old := current
	old.Hash = "1035ef3ce9e397fc1921a195512274a2ad63906bf843966e7dddcf9c8326131f"
	applyPromptBundle(f, old)
	job := f.seedReview(t, "job-v2-identity")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	before := promptIdentitySnapshot(f)
	applyPromptBundle(f, current)
	if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("old render identity error = %v", err)
	}
	if after := promptIdentitySnapshot(f); after != before {
		t.Fatalf("old render identity published effects: before=%+v after=%+v", before, after)
	}

	// An unconfigured publisher cannot prove that any persisted workflow uses
	// the current task layout and dedupe semantics.
	f.publisher.promptsHash = ""
	if err := f.publisher.Publish(context.Background(), wf.ID); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("unknown current render identity error = %v", err)
	}
	if after := promptIdentitySnapshot(f); after != before {
		t.Fatalf("unknown render identity published effects: before=%+v after=%+v", before, after)
	}
}
