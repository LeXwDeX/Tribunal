package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/agent"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/config"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/gitlab"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/review"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/pkg/dag"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/prompts"
)

const publicationSecret = "glpat-supersecret-value"

// reviewWithFailedNote completes a review whose summary note cannot be
// created. Publication must stay retryable instead of being lost.
func reviewWithFailedNote(t *testing.T, f *reviewFixture, jobID string) *store.Workflow {
	t.Helper()
	f.gitlab.noteErr = errors.New("gitlab: create note failed: token=" + publicationSecret)
	job := f.seedReview(t, jobID)
	if err := f.service.RunReview(context.Background(), job); err == nil {
		t.Fatal("RunReview succeeded with a failing note endpoint")
	}
	f.gitlab.noteErr = nil
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowCompleted) {
		t.Fatalf("workflow status = %q, want completed execution", workflow.Status)
	}
	return workflow
}

func assertNoSecret(t *testing.T, value string) {
	t.Helper()
	for _, secret := range []string{publicationSecret, "supersecret", "glpat"} {
		if strings.Contains(value, secret) {
			t.Fatalf("published error leaked %q: %q", secret, value)
		}
	}
}

func TestPublisherPublicationFailureKeepsRetryablePendingAndSanitizesError(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	workflow := reviewWithFailedNote(t, f, "job-publication-retry")

	if workflow.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("publication state = %q, want pending for a retryable failure", workflow.PublicationState)
	}
	if workflow.PublicationError != publicationRetryReason {
		t.Fatalf("publication error = %q, want %q", workflow.PublicationError, publicationRetryReason)
	}
	assertNoSecret(t, workflow.PublicationError)

	if err := f.publisher.Republish(context.Background(), workflow.ID); err != nil {
		t.Fatalf("Republish: %v", err)
	}
	published, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if published.PublicationState != store.WorkflowPublicationPublished || published.PublicationError != "" {
		t.Fatalf("republished publication = %q/%q", published.PublicationState, published.PublicationError)
	}
}

func TestPublisherFailureNoteRetryKeepsPending(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	f.gitlab.noteErr = errors.New("gitlab: create note failed: token=" + publicationSecret)

	job := f.seedReview(t, "job-publication-failure-note")
	if err := f.service.RunReview(context.Background(), job); err == nil || IsPermanentJob(err) {
		t.Fatalf("RunReview error = %v, want a retryable publication failure", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowFailed) {
		t.Fatalf("workflow status = %q, want failed", workflow.Status)
	}
	if workflow.PublicationState != store.WorkflowPublicationPending ||
		workflow.PublicationError != publicationRetryReason {
		t.Fatalf("failure-note publication = %q/%q", workflow.PublicationState, workflow.PublicationError)
	}
	assertNoSecret(t, workflow.PublicationError)
}

func TestPublisherStalePublicationStoresSkippedReason(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	workflow := reviewWithFailedNote(t, f, "job-publication-stale")

	f.gitlab.mr.SHA = strings.Repeat("9", 40)
	err := f.publisher.Publish(context.Background(), workflow.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("Publish error = %v, want ErrStalePublication", err)
	}
	stale, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if stale.PublicationState != store.WorkflowPublicationSkipped ||
		stale.PublicationError != "stale: merge request head changed" {
		t.Fatalf("stale publication = %q/%q", stale.PublicationState, stale.PublicationError)
	}
}

func TestPublisherPermanentFailureMarksFailed(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	workflow := reviewWithFailedNote(t, f, "job-publication-failed")

	f.gitlab.mrErr = &gitlab.Error{Op: "get_merge_request", Status: 404}
	err := f.publisher.Publish(context.Background(), workflow.ID)
	if !IsPermanentJob(err) {
		t.Fatalf("Publish error = %v, want a permanent failure", err)
	}
	failed, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if failed.PublicationState != store.WorkflowPublicationFailed ||
		failed.PublicationError != "merge request is not available" {
		t.Fatalf("failed publication = %q/%q", failed.PublicationState, failed.PublicationError)
	}
	if pending, _, err := f.db.ListWorkflows(context.Background(),
		store.WorkflowFilter{Kind: "review", PublicationState: store.WorkflowPublicationFailed}, "", 10); err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	} else if len(pending) != 1 || pending[0].ID != workflow.ID {
		t.Fatalf("failed filter = %+v", pending)
	}
}

func TestReviewIncompleteVerdictPersistsAndNeverReportsSuccess(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	profile := deepReviewProfile()
	profile.Synthesize = config.SynthesizeConfig{Mode: "code"}
	f.profiles["default"] = profile
	f.defaults.CommitStatus = true
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults

	outputs := deepReviewOutputs("unresolved evidence cannot pass")
	outputs[prompts.ReviewStandards] = &review.ReviewOut{
		Verdict:        "BLOCKING",
		EvidenceStatus: "insufficient",
		Findings:       []review.FindingOut{},
		EvidenceGaps:   []string{"pipeline log unavailable"},
		Summary:        "the standards channel cannot verify the change",
	}
	outputs[prompts.ReviewIntent] = &review.ReviewOut{
		Verdict:        "PASS",
		EvidenceStatus: "sufficient",
		Findings:       []review.FindingOut{},
		EvidenceGaps:   []string{},
		Summary:        "the change matches the MR intent",
	}
	outputs[prompts.ReviewGate] = &review.ReviewGateOut{
		Verdict:  "BLOCKING",
		Accepted: []review.Ref{},
		Rebutted: []review.Rebut{},
		Summary:  "no accepted findings explain the blocking channel",
	}
	runner := newScriptedAgentRunner(f.bundle, outputs)
	f.service.agentRun = runner.Run

	job := f.seedReview(t, "job-publication-incomplete")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowCompleted) {
		t.Fatalf("workflow status = %q", workflow.Status)
	}
	if workflow.Verdict != review.VerdictIncomplete {
		t.Fatalf("workflow verdict = %q, want INCOMPLETE", workflow.Verdict)
	}
	if workflow.PublicationState != store.WorkflowPublicationPublished || workflow.PublicationError != "" {
		t.Fatalf("publication = %q/%q", workflow.PublicationState, workflow.PublicationError)
	}

	snapshot, err := f.engine.Snapshot(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(f.gitlab.statuses) != 1 {
		t.Fatalf("commit statuses = %d, want 1", len(f.gitlab.statuses))
	}
	if state := f.gitlab.statuses[0].State; state != "failed" {
		t.Fatalf("commit status = %q, want failed for INCOMPLETE", state)
	}
	publications, err := f.db.ListPublications(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("ListPublications: %v", err)
	}
	for _, publication := range publications {
		if publication.Kind == "commit_status" && strings.Contains(publication.PublishKey, ":success") {
			t.Fatalf("INCOMPLETE wrote a success commit status: %+v", publication)
		}
	}
	if got := f.gitlab.noteCount(); got != 1 {
		t.Fatalf("notes = %d, want one INCOMPLETE summary", got)
	}
	body := f.gitlab.notes[0].Body
	if !strings.Contains(body, "评审未完成") || strings.Contains(body, "🟢 PASS") {
		t.Fatalf("INCOMPLETE summary is wrong:\n%s", body)
	}
	if len(snapshot.Order) == 0 {
		t.Fatal("empty workflow snapshot")
	}
}

func TestBuildReviewFilesReportsTruncation(t *testing.T) {
	diff := func(path string, lines int) gitlab.FileDiff {
		var b strings.Builder
		b.WriteString("@@ -1 +1,2 @@\n")
		for i := 0; i < lines; i++ {
			b.WriteString("+line\n")
		}
		return gitlab.FileDiff{NewPath: path, Diff: b.String()}
	}

	cases := []struct {
		name       string
		diffs      []gitlab.FileDiff
		maxFiles   int
		maxLines   int
		wantFiles  int
		wantWarn   string
		wantOmit   string
		wantReason string
	}{
		{
			name:       "max files",
			diffs:      []gitlab.FileDiff{diff("a.go", 1), diff("b.go", 1)},
			maxFiles:   1,
			maxLines:   100,
			wantFiles:  1,
			wantWarn:   "max_files",
			wantOmit:   "b.go",
			wantReason: "max_files",
		},
		{
			name:       "max diff lines",
			diffs:      []gitlab.FileDiff{diff("a.go", 2), diff("b.go", 2)},
			maxFiles:   10,
			maxLines:   3,
			wantFiles:  1,
			wantWarn:   "max_diff_lines",
			wantOmit:   "b.go",
			wantReason: "max_diff_lines",
		},
		{
			name: "too large",
			diffs: []gitlab.FileDiff{
				{NewPath: "big.bin", TooLarge: true},
				diff("a.go", 1),
			},
			maxFiles:   10,
			maxLines:   100,
			wantFiles:  1,
			wantWarn:   "too_large",
			wantOmit:   "big.bin",
			wantReason: "too_large",
		},
		{
			name: "collapsed",
			diffs: []gitlab.FileDiff{
				{NewPath: "big.go", Collapsed: true},
				diff("a.go", 1),
			},
			maxFiles:   10,
			maxLines:   100,
			wantFiles:  1,
			wantWarn:   "collapsed",
			wantOmit:   "big.go",
			wantReason: "collapsed",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			files, _, summary, warnings := buildReviewFiles(tc.diffs, nil, tc.maxFiles, tc.maxLines)
			if len(files) != tc.wantFiles {
				t.Fatalf("files = %d, want %d", len(files), tc.wantFiles)
			}
			if summary.Total != len(tc.diffs) || summary.Files != tc.wantFiles {
				t.Fatalf("summary total/included = %d/%d, want %d/%d",
					summary.Total, summary.Files, len(tc.diffs), tc.wantFiles)
			}
			if !summary.Truncated {
				t.Fatal("summary.Truncated = false, want true")
			}
			var omitted *review.OmittedFile
			for i := range summary.Omitted {
				if summary.Omitted[i].Path == tc.wantOmit {
					omitted = &summary.Omitted[i]
				}
			}
			if omitted == nil || omitted.Reason != tc.wantReason {
				t.Fatalf("omitted = %+v, want %s/%s", summary.Omitted, tc.wantOmit, tc.wantReason)
			}
			if len(warnings) == 0 {
				t.Fatal("no truncation warning")
			}
			joined := strings.Join(warnings, "\n")
			if !strings.Contains(joined, tc.wantWarn) {
				t.Fatalf("warnings = %v, want %q", warnings, tc.wantWarn)
			}
		})
	}

	files, _, summary, warnings := buildReviewFiles([]gitlab.FileDiff{diff("a.go", 2)}, nil, 10, 100)
	if len(files) != 1 || summary.Truncated || len(warnings) != 0 {
		t.Fatalf("complete diff = files %d truncated %v warnings %v", len(files), summary.Truncated, warnings)
	}
}

func TestBuildReviewFilesSeparatesInvalidPathsAndIgnore(t *testing.T) {
	diffs := []gitlab.FileDiff{
		{NewPath: "vendor/generated.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		{NewPath: "../secret", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
		{NewPath: "a.go", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
	}
	files, ignored, summary, warnings := buildReviewFiles(diffs, []string{"vendor/**"}, 10, 100)
	if len(files) != 1 || files[0].Path != "a.go" {
		t.Fatalf("files = %+v, want only a.go", files)
	}
	if len(ignored) != 1 || ignored[0] != "vendor/generated.go" {
		t.Fatalf("ignored = %+v", ignored)
	}
	if !summary.Truncated || summary.Total != 3 || summary.Files != 1 ||
		len(summary.Omitted) != 1 || summary.Omitted[0].Reason != "invalid_path" {
		t.Fatalf("summary = %+v", summary)
	}
	label := summary.Omitted[0].Path
	if !strings.HasPrefix(label, "<invalid-path/") || label == "../secret" || !validRepoPath(label) {
		t.Fatalf("unsafe invalid path label = %q", label)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "invalid path") || !strings.Contains(joined, label) {
		t.Fatalf("warnings = %v", warnings)
	}
}

func TestReviewRunStoresDiffTruncationInDossier(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{
		{NewPath: "a.go", Diff: "@@ -1 +1,2 @@\n a\n+b\n"},
		{NewPath: "b.go", Diff: "@@ -1 +1,2 @@\n a\n+c\n"},
	}
	f.defaults.MaxFiles = 1
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults

	job := f.seedReview(t, "job-dossier-truncated")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier: %v", err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatalf("decode dossier: %v", err)
	}
	if !dossier.DiffSummary.Truncated || dossier.DiffSummary.Total != 2 ||
		dossier.DiffSummary.Files != 1 || len(dossier.DiffSummary.Omitted) != 1 ||
		dossier.DiffSummary.Omitted[0].Path != "b.go" ||
		dossier.DiffSummary.Omitted[0].Reason != "max_files" {
		t.Fatalf("dossier diff summary = %+v, want one max_files omission", dossier.DiffSummary)
	}
	if workflow.Verdict != review.VerdictIncomplete {
		t.Fatalf("workflow verdict = %q, want INCOMPLETE for an omitted change", workflow.Verdict)
	}
	if workflow.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("publication state = %q, want published", workflow.PublicationState)
	}
	if len(f.gitlab.notes) != 1 {
		t.Fatalf("notes = %d, want one summary", len(f.gitlab.notes))
	}
	body := f.gitlab.notes[0].Body
	for _, want := range []string{"评审未完成", "omitted 1 of 2", "max_files"} {
		if !strings.Contains(body, want) {
			t.Fatalf("published coverage summary misses %q:\n%s", want, body)
		}
	}
}

func TestCancelReviewSkipsPublication(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	runJob := f.seedReview(t, "job-cancel-publication-run")
	wfID := createPendingReview(t, f, runJob.ID, jobGeneration(t, runJob))
	cancelJob := f.seedCancel(t, "job-cancel-publication")

	if err := f.service.CancelReview(context.Background(), cancelJob); err != nil {
		t.Fatalf("CancelReview: %v", err)
	}
	workflow, err := f.db.GetWorkflow(context.Background(), wfID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if workflow.Status != string(dag.WorkflowCancelled) {
		t.Fatalf("workflow status = %q, want cancelled", workflow.Status)
	}
	if workflow.PublicationState != store.WorkflowPublicationSkipped ||
		workflow.PublicationError != "review cancelled" {
		t.Fatalf("cancelled publication = %q/%q", workflow.PublicationState, workflow.PublicationError)
	}
	if f.gitlab.noteCount() != 0 {
		t.Fatalf("cancel published %d notes", f.gitlab.noteCount())
	}
}

func TestReviewFailedWithoutFailureNoteSkipsPublication(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.PostFailureNote = false
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}

	job := f.seedReview(t, "job-failed-no-note")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowFailed) {
		t.Fatalf("workflow status = %q, want failed", workflow.Status)
	}
	if workflow.PublicationState != store.WorkflowPublicationSkipped ||
		workflow.PublicationError != "failure note disabled" {
		t.Fatalf("failed publication = %q/%q", workflow.PublicationState, workflow.PublicationError)
	}
	if f.gitlab.noteCount() != 0 {
		t.Fatalf("a disabled failure note published %d notes", f.gitlab.noteCount())
	}
}

func TestPublishPendingRetriesFailedFailureNote(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, errors.New("provider failed token=" + publicationSecret)
	}
	f.gitlab.noteErr = errors.New("gitlab: create note failed")
	job := f.seedReview(t, "job-failed-note-recover")
	if err := f.service.RunReview(context.Background(), job); err == nil || IsPermanentJob(err) {
		t.Fatalf("RunReview error = %v, want a retryable publication failure", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Status != string(dag.WorkflowFailed) ||
		workflow.PublicationState != store.WorkflowPublicationPending {
		t.Fatalf("failed workflow = %q/%q, want failed/pending",
			workflow.Status, workflow.PublicationState)
	}

	f.gitlab.noteErr = nil
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("PublishPending: %v", err)
	}
	published, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if published.PublicationState != store.WorkflowPublicationPublished || published.PublicationError != "" {
		t.Fatalf("recovered publication = %q/%q", published.PublicationState, published.PublicationError)
	}
	if len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "评审未完成") {
		t.Fatalf("failure note = %+v", f.gitlab.notes)
	}
	assertNoSecret(t, f.gitlab.notes[0].Body)
}

func TestRepublishFailedWorkflowPublishesFailureSummary(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	job := f.seedReview(t, "job-failed-republish")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if err := f.db.SetWorkflowPublicationState(context.Background(), workflow.ID,
		store.WorkflowPublicationFailed, "publication is not possible"); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	before := f.gitlab.noteCount()
	if err := f.publisher.Republish(context.Background(), workflow.ID); err != nil {
		t.Fatalf("Republish: %v", err)
	}
	published, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if published.PublicationState != store.WorkflowPublicationPublished || published.PublicationError != "" {
		t.Fatalf("republished publication = %q/%q", published.PublicationState, published.PublicationError)
	}
	if f.gitlab.noteCount() != before {
		t.Fatalf("republish created a normal review summary: notes %d -> %d", before, f.gitlab.noteCount())
	}
	if len(f.gitlab.notes) != 1 || !strings.Contains(f.gitlab.notes[0].Body, "评审未完成") ||
		strings.Contains(f.gitlab.notes[0].Body, "🟢 PASS") {
		t.Fatalf("failed republish note = %+v", f.gitlab.notes)
	}
}

func TestRepublishFailedWorkflowKeepsStaleFence(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	job := f.seedReview(t, "job-failed-stale")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if err := f.db.SetWorkflowPublicationState(context.Background(), workflow.ID,
		store.WorkflowPublicationFailed, "publication is not possible"); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	if _, err := f.db.EnqueueReviewLatest(context.Background(), store.ReviewCommand{
		ProjectID: 42, MRIID: 7, Cancel: true,
		Payload: mustJSONBody(t, map[string]any{"trigger": "command"}),
	}); err != nil {
		t.Fatalf("EnqueueReviewLatest: %v", err)
	}
	err = f.publisher.Republish(context.Background(), workflow.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("Republish error = %v, want ErrStalePublication", err)
	}
	skipped, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if skipped.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("stale publication state = %q, want skipped", skipped.PublicationState)
	}
}

func TestReviewAllDiffsUnavailableCannotPass(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "big.bin", TooLarge: true}}
	job := f.seedReview(t, "job-all-too-large")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if _, err := f.db.GetWorkflowByJob(context.Background(), job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("all-omitted review created a workflow: %v", err)
	}
	if len(f.gitlab.notes) != 1 {
		t.Fatalf("notes = %d, want one summary", len(f.gitlab.notes))
	}
	body := f.gitlab.notes[0].Body
	for _, want := range []string{"评审未完成", "omitted 1 of 1", "too_large=1", "big.bin", "GitLab did not provide"} {
		if !strings.Contains(body, want) {
			t.Fatalf("all-omitted summary misses %q:\n%s", want, body)
		}
	}
}

func TestPublishPendingSkipsDisabledFailureNoteAfterCrash(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.PostFailureNote = false
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	job := f.seedReview(t, "job-failed-disabled-recover")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	// Simulate a crash after the failure event but before the skip write.
	if err := f.db.SetWorkflowPublicationState(context.Background(), workflow.ID,
		store.WorkflowPublicationPending, ""); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatalf("PublishPending: %v", err)
	}
	skipped, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if skipped.PublicationState != store.WorkflowPublicationSkipped ||
		skipped.PublicationError != "failure note disabled" {
		t.Fatalf("recovered publication = %q/%q", skipped.PublicationState, skipped.PublicationError)
	}
	if f.gitlab.noteCount() != 0 {
		t.Fatalf("disabled failure note published %d notes", f.gitlab.noteCount())
	}
}

func setReviewHeadForTest(f *reviewFixture, head string) {
	f.gitlab.mu.Lock()
	f.gitlab.mr.SHA = head
	f.gitlab.version.HeadSHA = head
	f.gitlab.mu.Unlock()
}

func TestReviewInvalidPathsCannotPass(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{{NewPath: "../secret", Diff: "@@ -1 +1 @@\n-a\n+b\n"}}
	job := f.seedReview(t, "job-all-invalid")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if _, err := f.db.GetWorkflowByJob(context.Background(), job.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("all-invalid review created a workflow: %v", err)
	}
	if len(f.gitlab.notes) != 1 {
		t.Fatalf("notes = %d, want one summary", len(f.gitlab.notes))
	}
	body := f.gitlab.notes[0].Body
	for _, want := range []string{"评审未完成", "omitted 1 of 1", "invalid_path=1", "invalid-path/"} {
		if !strings.Contains(body, want) {
			t.Fatalf("invalid-path summary misses %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "../secret") {
		t.Fatalf("unsafe invalid path reached the summary:\n%s", body)
	}
}

func TestReviewMixedInvalidPathIsCoverageGap(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.gitlab.diffs = []gitlab.FileDiff{
		{NewPath: "a.go", Diff: "@@ -1 +1,2 @@\n a\n+b\n"},
		{NewPath: "../secret", Diff: "@@ -1 +1 @@\n-a\n+b\n"},
	}
	job := f.seedReview(t, "job-mixed-invalid")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if workflow.Verdict != review.VerdictIncomplete {
		t.Fatalf("workflow verdict = %q, want INCOMPLETE", workflow.Verdict)
	}
	record, err := f.db.GetWorkflowDossier(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflowDossier: %v", err)
	}
	var dossier review.Dossier
	if err := json.Unmarshal(record.Data, &dossier); err != nil {
		t.Fatalf("decode dossier: %v", err)
	}
	if err := review.ValidateDossier(&dossier); err != nil {
		t.Fatalf("invalid-path label failed dossier validation: %v", err)
	}
	summary := dossier.DiffSummary
	if !summary.Truncated || summary.Total != 2 || summary.Files != 1 ||
		len(summary.Omitted) != 1 || summary.Omitted[0].Reason != "invalid_path" ||
		!strings.HasPrefix(summary.Omitted[0].Path, "<invalid-path/") {
		t.Fatalf("mixed invalid diff summary = %+v", summary)
	}
	if len(f.gitlab.notes) != 1 {
		t.Fatalf("notes = %d, want one summary", len(f.gitlab.notes))
	}
	body := f.gitlab.notes[0].Body
	for _, want := range []string{"评审未完成", "omitted 1 of 2", "invalid_path=1"} {
		if !strings.Contains(body, want) {
			t.Fatalf("mixed invalid summary misses %q:\n%s", want, body)
		}
	}
}

func TestPublishFailureRejectsNewHeadWithoutOverwritingPublished(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	job := f.seedReview(t, "job-failure-new-head")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	published, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if published.PublicationState != store.WorkflowPublicationPublished {
		t.Fatalf("initial publication = %q", published.PublicationState)
	}
	before := len(f.gitlab.notes)
	if before != 1 {
		t.Fatalf("notes = %d, want one failure note", before)
	}
	setReviewHeadForTest(f, strings.Repeat("9", 40))

	err = f.publisher.PublishFailure(context.Background(), workflow.ID, "review execution failed")
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("PublishFailure error = %v, want ErrStalePublication", err)
	}
	after, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if after.PublicationState != store.WorkflowPublicationPublished || after.PublicationError != "" {
		t.Fatalf("late stale overwrote published state: %q/%q", after.PublicationState, after.PublicationError)
	}
	if len(f.gitlab.notes) != before {
		t.Fatalf("late stale changed notes: %d -> %d", before, len(f.gitlab.notes))
	}
}

func TestRepublishFailedWorkflowRejectsNewHead(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.PostFailureNote = false
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	job := f.seedReview(t, "job-failed-new-head")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if err := f.db.SetWorkflowPublicationState(context.Background(), workflow.ID,
		store.WorkflowPublicationFailed, "publication is not possible"); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	setReviewHeadForTest(f, strings.Repeat("9", 40))
	err = f.publisher.Republish(context.Background(), workflow.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("Republish error = %v, want ErrStalePublication", err)
	}
	skipped, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if skipped.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("new-head publication state = %q, want skipped", skipped.PublicationState)
	}
	if len(f.gitlab.notes) != 0 {
		t.Fatalf("new-head republish wrote %d failure notes", len(f.gitlab.notes))
	}
}

func TestRepublishFailedWorkflowRejectsChangedConfig(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.PostFailureNote = false
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return nil, agent.ErrNoSubmit
	}
	job := f.seedReview(t, "job-failed-new-config")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatalf("GetWorkflowByJob: %v", err)
	}
	if err := f.db.SetWorkflowPublicationState(context.Background(), workflow.ID,
		store.WorkflowPublicationFailed, "publication is not possible"); err != nil {
		t.Fatalf("SetWorkflowPublicationState: %v", err)
	}
	f.gitlab.mu.Lock()
	f.gitlab.docs["AGENTS.md"] = "changed repository configuration"
	f.gitlab.mu.Unlock()
	err = f.publisher.Republish(context.Background(), workflow.ID)
	if !errors.Is(err, ErrStalePublication) {
		t.Fatalf("Republish error = %v, want ErrStalePublication", err)
	}
	skipped, err := f.db.GetWorkflow(context.Background(), workflow.ID)
	if err != nil {
		t.Fatalf("GetWorkflow: %v", err)
	}
	if skipped.PublicationState != store.WorkflowPublicationSkipped {
		t.Fatalf("changed-config publication state = %q, want skipped", skipped.PublicationState)
	}
	if len(f.gitlab.notes) != 0 {
		t.Fatalf("changed-config republish wrote %d failure notes", len(f.gitlab.notes))
	}
}
