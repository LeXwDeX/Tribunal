package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
	"github.com/LeXwDeX/tribunal/prompts"
)

func TestPublishRecoveryFromLegacySynthesisDoesNotExposeFailure(t *testing.T) {
	const canary = "historical-verifier-canary"
	group := review.Group{ID: "g", Severity: review.SeverityP1, Category: "logic", Title: "finding", Body: "business body", Challenge: &review.Challenge{Status: review.ChallengeUncertain, Reason: "challenge failed: " + canary}, Decision: &review.FindingDecision{Action: review.ActionMark, Reason: "challenge failed: " + canary + "; on_uncertain=mark", Source: "rules"}}
	result := review.SynthesisResult{Verdict: review.VerdictBlocking, Summary: "summary", Groups: []review.Group{group}, Warnings: []string{"verify failed for group g: " + canary}, Output: review.SynthesizeOut{Verdict: review.VerdictBlocking, Summary: "summary", Decisions: []review.Decision{{FindingID: "g", Action: "mark", Reason: group.Decision.Reason}}, NewFindings: []review.FindingOut{}, ExtendRequests: []review.ExtendRequest{}}}
	raw, _ := json.Marshal(result)
	wf := dag.WorkflowView{Order: []string{"verify.1", "synthesize"}, Nodes: map[string]dag.NodeView{
		"verify.1":   {ID: "verify.1", Status: dag.NodeFailed, Error: canary, Spec: dag.NodeSpec{Kind: review.KindVerify, Params: json.RawMessage(`{"group_id":"g"}`)}},
		"synthesize": {ID: "synthesize", Status: dag.NodeCompleted, Output: raw, Spec: dag.NodeSpec{Kind: review.KindSynthesize}},
	}}
	got, err := synthesisResult(wf)
	if err != nil {
		t.Fatal(err)
	}
	body, err := review.RenderSummary(review.SummaryInput{Verdict: got.Verdict, Summary: got.Summary, Groups: got.Groups, Warnings: got.Warnings, Fingerprint: "fp"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, canary) {
		t.Fatalf("published summary leaked: %s", body)
	}
	if got.Groups[0].Body != "business body" || got.Verdict != review.VerdictBlocking {
		t.Fatalf("business result changed: %+v", got)
	}
	if !strings.Contains(string(wf.Nodes["synthesize"].Output), canary) {
		t.Fatal("stored output changed")
	}
}

type legacyPublishStore struct{ dag.EventStore }

func (s legacyPublishStore) Append(ctx context.Context, id string, rev int64, events ...dag.Event) (int64, error) {
	const canary = "historical-publish-verifier-canary"
	for i := range events {
		switch events[i].Type {
		case dag.EventNodeFailed:
			var payload dag.NodeFailedPayload
			if err := json.Unmarshal(events[i].Payload, &payload); err != nil {
				return 0, err
			}
			if strings.HasPrefix(payload.NodeID, "verify.") {
				payload.Error = canary
				events[i].Payload, _ = json.Marshal(payload)
			}
		case dag.EventNodeCompleted:
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
			for i := range result.Groups {
				group := &result.Groups[i]
				if group.Challenge == nil || group.Decision == nil {
					continue
				}
				group.Challenge.Reason = "challenge failed: " + canary
				group.Decision.Reason = "challenge failed: " + canary + "; on_uncertain=mark"
				result.Warnings = append(result.Warnings, "verify failed for group "+group.ID+": "+canary)
			}
			payload.Output, _ = json.Marshal(result)
			events[i].Payload, _ = json.Marshal(payload)
		}
	}
	return s.EventStore.Append(ctx, id, rev, events...)
}

func TestPublishLegacySynthesisWithSQLiteAndFakeGitLab(t *testing.T) {
	const canary = "historical-publish-verifier-canary"
	f := newReviewFixture(t, reviewTestHead)
	f.profiles["default"] = deepReviewProfile()
	runner := newScriptedAgentRunner(f.bundle, deepReviewOutputs("summary"))
	runner.fail[prompts.Verify] = errors.New("simulated outage")
	f.service.agentRun = runner.Run
	f.engine = dag.NewEngine(legacyPublishStore{f.db}, f.executors, f.executors, dag.Options{})
	f.service.engine, f.publisher.engine = f.engine, f.engine
	job := f.seedReview(t, "legacy-publish")
	if err := f.service.Handle(context.Background(), job); err != nil {
		t.Fatal(err)
	}
	workflow, err := f.db.GetWorkflowByJob(context.Background(), job.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.engine.Snapshot(context.Background(), workflow.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snapshot.Nodes[review.NodeSynth].Output), canary) {
		t.Fatal("test did not persist legacy synthesis")
	}
	if snapshot.Nodes["verify.1"].Error != canary {
		t.Fatal("test did not persist legacy failure")
	}
	if f.gitlab.noteCount() == 0 {
		t.Fatal("no fake GitLab summary published")
	}
	for _, note := range f.gitlab.notes {
		if strings.Contains(note.Body, canary) {
			t.Fatalf("published legacy failure: %s", note.Body)
		}
	}
	findings, err := f.db.ListFindings(context.Background(), store.FindingFilter{WorkflowID: workflow.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) == 0 {
		t.Fatal("no findings published")
	}
	for _, finding := range findings {
		if strings.Contains(finding.DecisionReason, canary) || strings.Contains(string(finding.ChallengeEvidence), canary) {
			t.Fatalf("projected finding leaked: %+v", finding)
		}
	}
}
