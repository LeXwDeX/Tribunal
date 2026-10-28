package review

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

func TestResumeLegacyAdjudicationFromSQLite(t *testing.T) {
	const canary = "historical-verifier-canary"
	ctx := context.Background()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "recovery.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.RegisterProject(ctx, store.Project{ID: 42, PathWithNamespace: "group/project", WebURL: "https://example.test/group/project", DefaultBranch: "main", TokenEnc: []byte("encrypted"), WebhookSecretEnc: []byte("encrypted"), TokenScopes: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	var engine *dag.Engine
	head, base := fixedSnapshot(t)
	dossier := validDossier(t)
	dossier.HeadDir, dossier.BaseDir = head, base
	executor := newTestExecutor(t, ExecutorDeps{Dossier: dossier, Snapshot: func(ctx context.Context, id string) (dag.WorkflowView, error) { return engine.Snapshot(ctx, id) }})
	engine = dag.NewEngine(db, executor, nil, dag.Options{})
	verifyParamsJSON, _ := json.Marshal(verifyParams{GroupID: "group"})
	synthParamsJSON, _ := json.Marshal(synthesizeParams{Mode: "code", OnUncertain: string(OnUncertainMark)})
	id, err := engine.Create(ctx, dag.GraphSpec{Nodes: []dag.NodeSpec{
		{ID: "verify.1", Kind: KindVerify, Params: verifyParamsJSON},
		{ID: NodeAdjudicate, Kind: KindAdjudicate, Required: true, DependsOn: []string{"verify.1"}},
		{ID: NodeSynth, Kind: KindSynthesize, Required: true, DependsOn: []string{NodeAdjudicate}, Params: synthParamsJSON},
	}}, map[string]string{"project_id": "42", "kind": "review", "mr_iid": "7", "trigger": "api"})
	if err != nil {
		t.Fatal(err)
	}
	group := adjudicateGroup("group", SeverityP1, ChallengeFailed, nil)
	group.Challenge.Reason = canary
	old, err := Adjudicate([]Group{group}, AdjudicateInput{ChallengeOnly: []Severity{SeverityP1}, OnUncertain: OnUncertainMark, Warnings: []string{"verify failed for group group: " + canary}})
	if err != nil {
		t.Fatal(err)
	}
	appendEvent := func(rev int64, kind string, payload any) int64 {
		t.Helper()
		raw, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		next, err := db.Append(ctx, id, rev, dag.Event{Type: kind, Payload: raw})
		if err != nil {
			t.Fatal(err)
		}
		return next
	}
	rev := appendEvent(1, dag.EventWorkflowStarted, dag.WorkflowTerminalPayload{})
	rev = appendEvent(rev, dag.EventNodeQueued, dag.NodeQueuedPayload{NodeID: "verify.1", Attempt: 1})
	rev = appendEvent(rev, dag.EventNodeStarted, dag.NodeStartedPayload{NodeID: "verify.1", Attempt: 1})
	rev = appendEvent(rev, dag.EventNodeFailed, dag.NodeFailedPayload{NodeID: "verify.1", Attempt: 1, Error: canary, Reason: "executor_error", Usage: dag.Usage{InputTokens: 7}})
	rev = appendEvent(rev, dag.EventNodeQueued, dag.NodeQueuedPayload{NodeID: NodeAdjudicate, Attempt: 1})
	rev = appendEvent(rev, dag.EventNodeStarted, dag.NodeStartedPayload{NodeID: NodeAdjudicate, Attempt: 1})
	legacy, _ := json.Marshal(AdjudicateOut(old))
	_ = appendEvent(rev, dag.EventNodeCompleted, dag.NodeCompletedPayload{NodeID: NodeAdjudicate, Attempt: 1, Output: legacy})
	if err := engine.Run(ctx, id); err != nil {
		t.Fatal(err)
	}
	wf, err := engine.Snapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if wf.Status != dag.WorkflowCompleted || wf.Nodes[NodeSynth].Status != dag.NodeCompleted || wf.Usage.InputTokens != 7 {
		t.Fatalf("recovery failed: %+v", wf)
	}
	if strings.Contains(string(wf.Nodes[NodeSynth].Output), canary) {
		t.Fatalf("new synthesis leaked: %s", wf.Nodes[NodeSynth].Output)
	}
	if !strings.Contains(string(wf.Nodes[NodeAdjudicate].Output), canary) || wf.Nodes["verify.1"].Error != canary {
		t.Fatal("historical data changed")
	}
}
