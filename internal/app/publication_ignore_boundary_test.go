package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
	"github.com/LeXwDeX/tribunal/internal/gitlab"
	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type legacyPublicationStore struct{ ReviewStore }

func (s legacyPublicationStore) GetWorkflowDossier(ctx context.Context, id string) (*store.WorkflowDossier, error) {
	record, err := s.ReviewStore.GetWorkflowDossier(ctx, id)
	if err != nil {
		return nil, err
	}
	var d review.Dossier
	if err := json.Unmarshal(record.Data, &d); err != nil {
		return nil, err
	}
	d.IgnorePolicy = nil
	record.Data, err = json.Marshal(d)
	return record, err
}

func TestLegacyPublicationCannotBeRetriedOrRepublished(t *testing.T) {
	for _, mode := range []string{"pending", "manual", "direct"} {
		t.Run(mode, func(t *testing.T) {
			f := newReviewFixture(t, reviewTestHead)
			wf := reviewWithFailedNote(t, f, "legacy-publication")
			f.publisher.store = legacyPublicationStore{f.db}
			// Any GitLab call after the local policy check would panic.
			f.publisher.gitlab = nil
			var err error
			if mode == "pending" {
				err = f.service.PublishPending(context.Background())
			} else if mode == "manual" {
				err = f.publisher.Republish(context.Background(), wf.ID)
			} else {
				err = f.publisher.Publish(context.Background(), wf.ID)
			}
			if (mode == "pending" && err != nil) || (mode != "pending" && !errors.Is(err, ErrStalePublication)) {
				t.Fatal(err)
			}
			state, err := f.db.GetWorkflow(context.Background(), wf.ID)
			if err != nil || state.PublicationState != store.WorkflowPublicationSkipped || f.gitlab.noteCount() != 0 {
				t.Fatalf("state=%+v notes=%d err=%v", state, f.gitlab.noteCount(), err)
			}
		})
	}
}

func TestLegacyRepublishPreservesTerminalPublicationAndDossier(t *testing.T) {
	for _, state := range []string{store.WorkflowPublicationPublished, store.WorkflowPublicationFailed} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			wf := reviewWithFailedNote(t, f, "legacy-terminal")
			if err := f.db.SetWorkflowPublicationState(ctx, wf.ID, state, "previous outcome"); err != nil {
				t.Fatal(err)
			}
			before, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			f.publisher.store = legacyPublicationStore{f.db}
			f.publisher.gitlab = nil
			if err := f.publisher.Republish(ctx, wf.ID); !errors.Is(err, ErrStalePublication) {
				t.Fatalf("republish = %v", err)
			}
			after, err := f.db.GetWorkflowDossier(ctx, wf.ID)
			if err != nil {
				t.Fatal(err)
			}
			if after.PublicationState != state || after.PublicationError != before.PublicationError || after.Hash != before.Hash || !bytes.Equal(after.Data, before.Data) {
				t.Fatalf("historical publication changed: state=%s error=%s", after.PublicationState, after.PublicationError)
			}
		})
	}
}

func TestIgnoredFindingNeverReachesGitLab(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.Ignore = []string{"private.txt"}
	f.defaults.PostFailureNote = false
	f.defaults.CommitStatus = true
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	f.gitlab.diffs = append(f.gitlab.diffs, gitlab.FileDiff{NewPath: "private.txt", Diff: "+CANARY_DIFF\n"})
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) {
		return &agent.RunResult{Output: &review.CodeReviewOut{Summary: "CANARY_SUMMARY", Findings: []review.FindingOut{{Path: "private.txt", Line: 1, Side: "new", Severity: review.SeverityP2, Category: "correctness", Title: "CANARY_TITLE", Body: "CANARY_BODY", Evidence: "CANARY_EVIDENCE", Confidence: review.ConfidenceHigh}}}, Usage: llm.Usage{InputTokens: 42, OutputTokens: 9}}, nil
	}
	job := f.seedReview(t, "ignored-finding")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.engine.Snapshot(context.Background(), wf.ID)
	if err != nil || snapshot.Status != dag.WorkflowFailed || snapshot.Usage.InputTokens != 42 || snapshot.Usage.OutputTokens != 9 {
		t.Fatalf("workflow status=%s usage=%+v err=%v", snapshot.Status, snapshot.Usage, err)
	}
	if got := countPublicationEffects(f); got != (publicationEffects{}) {
		t.Fatalf("ignored output was published: %+v", got)
	}
	for _, note := range f.gitlab.notes {
		if strings.Contains(note.Body, "CANARY_") {
			t.Fatalf("note leaked: %s", note.Body)
		}
	}
	for _, disc := range f.gitlab.discussions {
		for _, note := range disc.Notes {
			if strings.Contains(note.Body, "CANARY_") {
				t.Fatalf("discussion leaked: %s", note.Body)
			}
		}
	}
}

type ignoredSynthesisStore struct {
	dag.EventStore
	mutate func(*review.SynthesisResult)
}

func (s ignoredSynthesisStore) Append(ctx context.Context, id string, rev int64, events ...dag.Event) (int64, error) {
	for i := range events {
		if events[i].Type != dag.EventNodeCompleted {
			continue
		}
		var payload dag.NodeCompletedPayload
		if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
			return 0, err
		}
		if payload.NodeID != review.NodeSynth {
			continue
		}
		var result review.SynthesisResult
		if err := json.Unmarshal(payload.Output, &result); err != nil {
			return 0, err
		}
		s.mutate(&result)
		result.Summary = "CANARY_IGNORED_SUMMARY"
		payload.Output, _ = json.Marshal(result)
		events[i].Payload, _ = json.Marshal(payload)
	}
	return s.EventStore.Append(ctx, id, rev, events...)
}

func TestPublisherRejectsAllIgnoredFindingLocations(t *testing.T) {
	for name, mutate := range map[string]func(*review.SynthesisResult){
		"group":  func(r *review.SynthesisResult) { r.Groups[0].Path = "private.txt" },
		"source": func(r *review.SynthesisResult) { r.Groups[0].Sources[0].Path = "private.txt" },
		"evidence": func(r *review.SynthesisResult) {
			r.Groups[0].Challenge = &review.Challenge{Status: review.ChallengeConfirmed, Evidence: []review.CodeRef{{Path: "private.txt", StartLine: 1, EndLine: 1, Note: "CANARY_EVIDENCE"}}}
		},
		"new-finding": func(r *review.SynthesisResult) {
			r.Output.NewFindings = []review.FindingOut{{Path: "private.txt", Line: 1, Side: "new", Severity: review.SeverityP2, Category: "correctness", Title: "CANARY_TITLE", Body: "CANARY_BODY", Evidence: "CANARY_EVIDENCE", Confidence: review.ConfidenceHigh}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			f := newReviewFixture(t, reviewTestHead)
			f.defaults.Ignore = []string{"private.txt"}
			f.defaults.CommitStatus = true
			f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
			f.engine = dag.NewEngine(ignoredSynthesisStore{f.db, mutate}, f.executors, f.executors, dag.Options{})
			f.service.engine, f.publisher.engine = f.engine, f.engine
			job := f.seedReview(t, "ignored-synthesis")
			if err := f.service.RunReview(ctx, job); !errors.Is(err, ErrStalePublication) {
				t.Fatalf("publish = %v, want stale rejection", err)
			}
			wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
			if err != nil || wf.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(wf.PublicationError, "ignored finding") {
				t.Fatalf("workflow=%+v err=%v", wf, err)
			}
			if got := countPublicationEffects(f); got != (publicationEffects{}) {
				t.Fatalf("ignored result reached GitLab: %+v", got)
			}
			findings, err := f.db.ListFindings(ctx, store.FindingFilter{WorkflowID: wf.ID})
			if err != nil || len(findings) != 0 {
				t.Fatalf("ignored result projected: %d findings, %v", len(findings), err)
			}
			snapshot, err := f.engine.Snapshot(ctx, wf.ID)
			if err != nil || !strings.Contains(string(snapshot.Nodes[review.NodeSynth].Output), "CANARY_IGNORED_SUMMARY") {
				t.Fatalf("persisted output was rewritten: %v", err)
			}
		})
	}
}

func TestLegacyFailedPendingNeverPublishes(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.PostFailureNote = false
	f.service.reviewDefaults = f.defaults
	f.publisher.reviewDefaults = f.defaults
	f.service.agentRun = func(context.Context, agent.RunSpec) (*agent.RunResult, error) { return nil, agent.ErrNoSubmit }
	job := f.seedReview(t, "legacy-failed")
	if err := f.service.RunReview(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	wf, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.db.SetWorkflowPublicationState(context.Background(), wf.ID, store.WorkflowPublicationPending, ""); err != nil {
		t.Fatal(err)
	}
	f.service.store = legacyPublicationStore{f.db}
	f.publisher.store = legacyPublicationStore{f.db}
	if err := f.service.PublishPending(context.Background()); err != nil {
		t.Fatal(err)
	}
	state, err := f.db.GetWorkflow(context.Background(), wf.ID)
	if err != nil || state.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(state.PublicationError, "ignore policy") || f.gitlab.noteCount() != 0 {
		t.Fatalf("state=%+v notes=%d err=%v", state, f.gitlab.noteCount(), err)
	}
}

func TestDossierDiffIndexKeepsBothRenameSidesOnly(t *testing.T) {
	version := gitlab.Version{BaseSHA: strings.Repeat("1", 40), StartSHA: strings.Repeat("2", 40), HeadSHA: reviewTestHead}
	dossier := &review.Dossier{Files: []review.FileChange{{Path: "new.go", OldPath: "old.go", Status: "renamed", Diff: "@@ -1 +1 @@\n-old\n+new\n"}}}
	idx := gitlab.BuildDiffIndex(version, dossierDiffs(dossier))
	if _, ok := idx.Locate("old.go", 1, gitlab.SideOld); !ok {
		t.Fatal("old rename side missing")
	}
	if _, ok := idx.Locate("new.go", 1, gitlab.SideNew); !ok {
		t.Fatal("new rename side missing")
	}
	if _, ok := idx.Locate("excluded.go", 1, gitlab.SideNew); ok {
		t.Fatal("excluded diff was indexed")
	}
}

func TestPublisherRejectsRootAnchoredIgnoreFinding(t *testing.T) {
	ctx := context.Background()
	f := newReviewFixture(t, reviewTestHead)
	f.defaults.Ignore = []string{"/private/**"}
	f.defaults.CommitStatus = true
	f.service.reviewDefaults, f.publisher.reviewDefaults = f.defaults, f.defaults
	f.engine = dag.NewEngine(ignoredSynthesisStore{f.db, func(r *review.SynthesisResult) {
		r.Groups[0].Path = "private/a.go"
	}}, f.executors, f.executors, dag.Options{})
	f.service.engine, f.publisher.engine = f.engine, f.engine
	job := f.seedReview(t, "root-anchored-ignored-synthesis")

	if err := f.service.RunReview(ctx, job); !errors.Is(err, ErrStalePublication) {
		t.Fatalf("publish = %v, want stale rejection", err)
	}
	wf, err := f.db.GetWorkflowByJob(ctx, job.ID)
	if err != nil || wf.PublicationState != store.WorkflowPublicationSkipped || !strings.Contains(wf.PublicationError, "ignored finding") {
		t.Fatalf("workflow=%+v err=%v", wf, err)
	}
	if got := countPublicationEffects(f); got != (publicationEffects{}) {
		t.Fatalf("ignored result reached GitLab: %+v", got)
	}
}

func TestLegacyIgnorePolicyWithPatternsIsRejected(t *testing.T) {
	d := testDossier(t, reviewTestHead)
	d.IgnorePolicy = &review.IgnorePolicy{Version: review.IgnorePolicyVersion - 1, Patterns: []string{"private/**"}}
	if reason := publicationPolicyReason(d); reason == "" {
		t.Fatal("publication accepted a legacy ignore policy")
	}
	if d.WorkspaceProvenanceValid() {
		t.Fatal("legacy ignore policy passed workspace provenance")
	}
}
