package review

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestHistoricalVerifyFailureDoesNotExposeError(t *testing.T) {
	const canary = "historical-verifier-canary"
	params, err := json.Marshal(verifyParams{GroupID: "group"})
	if err != nil {
		t.Fatal(err)
	}
	wf := dag.WorkflowView{Nodes: map[string]dag.NodeView{"verify": {
		ID: "verify", Spec: dag.NodeSpec{Kind: KindVerify, Params: params}, Status: dag.NodeFailed,
		Error: canary, Reason: "executor_error",
	}}}
	_, failed, err := collectVerifyResults(wf)
	if err != nil {
		t.Fatal(err)
	}
	if failed["group"] == "" || strings.Contains(failed["group"], canary) {
		t.Fatalf("failed verification = %q", failed["group"])
	}
}

func TestResumeSynthesisAfterLegacyAdjudicateDoesNotCopyFailure(t *testing.T) {
	const canary = "historical-verifier-canary"
	group := adjudicateGroup("group", SeverityP1, ChallengeFailed, nil)
	group.Body = "ordinary finding body"
	group.Challenge.Reason = canary
	old, err := Adjudicate([]Group{group}, AdjudicateInput{ChallengeOnly: []Severity{SeverityP1}, OnUncertain: OnUncertainMark, Warnings: []string{"verify failed for group group: " + canary}})
	if err != nil {
		t.Fatal(err)
	}
	params, _ := json.Marshal(verifyParams{GroupID: "group"})
	wf := evidenceWorkflow(dag.NodeView{ID: "verify.1", Spec: dag.NodeSpec{ID: "verify.1", Kind: KindVerify, Params: params}, Status: dag.NodeFailed, Error: canary, Reason: "executor_error"})
	wf.Nodes[NodeAdjudicate] = completedNode(NodeAdjudicate, KindAdjudicate, nil, AdjudicateOut(old))
	result := runEvidenceSynthesis(t, evidenceExecutor(t, wf), wf, AdjudicateOut(old))
	raw, _ := json.Marshal(result)
	if strings.Contains(string(raw), canary) {
		t.Fatalf("resumed synthesis leaked: %s", raw)
	}
	if result.Groups[0].Body != group.Body || result.Groups[0].Decision.Action != ActionMark || result.Verdict != VerdictBlocking {
		t.Fatalf("business result changed: %+v", result)
	}
	if !strings.Contains(string(wf.Nodes[NodeAdjudicate].Output), canary) {
		t.Fatal("historical output changed")
	}
}

func TestSafeDiagnosticsDoesNotReplaceShortErrorInBusinessText(t *testing.T) {
	wf := evidenceWorkflow(dag.NodeView{ID: "verify", Status: dag.NodeFailed, Error: "a", Spec: dag.NodeSpec{Kind: KindVerify, Params: json.RawMessage(`{"group_id":"g"}`)}})
	raw := json.RawMessage(`{"groups":[{"ID":"g","Body":"a banana","Challenge":{"Reason":"challenge failed: a"},"Decision":{"Reason":"challenge failed: a; on_uncertain=mark"}},{"ID":"other","Body":"a banana","Challenge":{"Reason":"a"},"Decision":{"Reason":"ordinary a"}}],"warnings":["verify failed for group g: a","business a"],"extra":{"value":"a"}}`)
	got, err := SafeDiagnosticOutput(KindAdjudicate, raw, wf)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{"a banana", "ordinary a", "business a", `"value":"a"`, "challenge failed: node failed", "verify failed for group g: node failed"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in %s", want, text)
		}
	}
	if !strings.Contains(string(raw), "challenge failed: a") {
		t.Fatal("input history changed")
	}
}

func TestSafeDiagnosticsRedactsTruncatedGeneratedDecision(t *testing.T) {
	text := strings.Repeat("secret", 800)
	wf := evidenceWorkflow(dag.NodeView{ID: "verify", Status: dag.NodeFailed, Error: text, Spec: dag.NodeSpec{Kind: KindVerify, Params: json.RawMessage(`{"group_id":"g"}`)}})
	generated := truncateRunes("challenge failed: "+text+"; on_uncertain=mark", MaxReasonRunes)
	raw, _ := json.Marshal(map[string]any{"groups": []any{}, "output": map[string]any{"decisions": []any{map[string]string{"finding_id": "g", "reason": generated}}}})
	got, err := SafeDiagnosticOutput(KindSynthesize, raw, wf)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "secret") {
		t.Fatalf("truncated generated diagnostic leaked: %s", got)
	}
}
