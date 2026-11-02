package dag_test

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/internal/store/sqlite"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type sqliteCancelExecutor struct {
	started chan struct{}
	release chan struct{}
}

func (e sqliteCancelExecutor) Execute(ctx context.Context, in dag.NodeInput) (dag.NodeOutput, error) {
	if in.Node.ID == "a" {
		return dag.NodeOutput{Usage: dag.Usage{InputTokens: 7}}, nil
	}
	close(e.started)
	<-ctx.Done()
	<-e.release
	return dag.NodeOutput{Usage: dag.Usage{InputTokens: 99}}, nil
}

func TestCancelledUnknownUsagePersistsInSQLite(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "cancel.db")
	open := func() *sqlite.Store {
		t.Helper()
		st, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		return st
	}
	st := open()
	defer func() { _ = st.Close() }()
	_, err := st.RegisterProject(ctx, store.Project{ID: 42, PathWithNamespace: "group/project", WebURL: "https://gitlab.invalid/group/project", DefaultBranch: "main", TokenEnc: []byte("fake"), WebhookSecretEnc: []byte("fake"), TokenScopes: []string{"api"}})
	if err != nil {
		t.Fatal(err)
	}
	exec := sqliteCancelExecutor{started: make(chan struct{}), release: make(chan struct{})}
	var once sync.Once
	release := func() { once.Do(func() { close(exec.release) }) }
	defer release()
	engine := dag.NewEngine(st, exec, nil, dag.Options{})
	id, err := engine.Create(ctx, dag.GraphSpec{Nodes: []dag.NodeSpec{
		{ID: "a", Kind: "x", Required: true},
		{ID: "b", Kind: "x", Required: true, TokenFree: true, DependsOn: []string{"a"}},
	}}, map[string]string{"project_id": "42", "kind": "review", "mr_iid": "7", "trigger": "api"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- engine.Run(ctx, id) }()
	select {
	case <-exec.started:
	case <-time.After(5 * time.Second):
		t.Fatal("executor did not start")
	}
	if err := engine.Cancel(ctx, id, "user"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("Run returned before the executor exited: %v", err)
	default:
	}
	check := func(stage string) {
		t.Helper()
		wf, err := st.GetWorkflow(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		nodes, err := st.GetWorkflowNodes(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		byID := make(map[string]store.WorkflowNode, len(nodes))
		for _, node := range nodes {
			byID[node.ID] = node
		}
		if wf.Status != string(dag.WorkflowCancelled) || !wf.UsageIncomplete || wf.TokensIn != 7 || len(nodes) != 2 || byID["a"].UsageIncomplete || byID["a"].TokensIn != 7 || byID["b"].Status != string(dag.NodeAborted) || !byID["b"].UsageIncomplete || byID["b"].TokensIn != 0 {
			t.Fatalf("%s: workflow=%+v nodes=%+v", stage, wf, nodes)
		}
		events, err := st.Load(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		projected, err := dag.Project(events)
		if err != nil || !projected.UsageIncomplete || !projected.Nodes["b"].UsageIncomplete || projected.Usage.InputTokens != 7 {
			t.Fatalf("%s replay: workflow=%+v err=%v", stage, projected, err)
		}
	}
	check("before executor exit")
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not finish after executor exit")
	}
	check("after executor exit")
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	st = open()
	check("after restart")
}
