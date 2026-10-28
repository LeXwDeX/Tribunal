package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
	"github.com/LeXwDeX/tribunal/pkg/dag"
)

type cancelAfterGapStore struct {
	dag.EventStore
	cancel context.CancelFunc
	once   sync.Once
}

func (s *cancelAfterGapStore) Append(ctx context.Context, id string, rev int64, events ...dag.Event) (int64, error) {
	next, err := s.EventStore.Append(ctx, id, rev, events...)
	if err == nil {
		for _, event := range events {
			if event.Type != dag.EventNodeCompleted {
				continue
			}
			var payload dag.NodeCompletedPayload
			var out struct {
				ToolGaps []string `json:"tool_gaps"`
			}
			if json.Unmarshal(event.Payload, &payload) == nil && json.Unmarshal(payload.Output, &out) == nil && len(out.ToolGaps) > 0 {
				s.once.Do(s.cancel)
			}
		}
	}
	return next, err
}

func TestCompletedMCPGapSurvivesRetryAfterIndexRecovers(t *testing.T) {
	f := newReviewFixture(t, reviewTestHead)
	f.service.reviewDefaults.Ignore, f.publisher.reviewDefaults.Ignore = nil, nil
	f.service.blocks = map[string]config.BlockBinding{review.BlockCodeReview: {MCPTools: []string{"mcp__graph__search"}}}
	provider := &contextCanaryProvider{}
	f.service.tools = provider
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := &cancelAfterGapStore{EventStore: f.db, cancel: cancel}
	f.engine = dag.NewEngine(events, f.executors, f.executors, dag.Options{})
	f.service.engine, f.publisher.engine = f.engine, f.engine
	job := f.seedReview(t, "completed-gap-retry")
	if err := f.service.Handle(ctx, job); !errors.Is(err, context.Canceled) {
		t.Fatalf("interrupted review: %v", err)
	}
	baseCtx := context.Background()
	wf, err := f.db.GetWorkflowByJob(baseCtx, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.db.GetWorkflowDossier(baseCtx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := f.engine.Snapshot(baseCtx, wf.ID)
	if err != nil {
		t.Fatal(err)
	}
	var gapID string
	var gapOutput []byte
	for id, node := range snapshot.Nodes {
		if node.Status == dag.NodeCompleted && bytes.Contains(node.Output, []byte(`"tool_gaps"`)) {
			gapID, gapOutput = id, append([]byte(nil), node.Output...)
			break
		}
	}
	if gapID == "" || provider.tools != 0 || provider.calls != 0 {
		t.Fatalf("missing completed gap or unsafe tool calls: id=%q tools=%d calls=%d", gapID, provider.tools, provider.calls)
	}
	if _, err := f.db.SaveWorkspace(baseCtx, store.Workspace{ProjectID: 42, State: "ready", IndexSHA: strings.Repeat("2", 40)}); err != nil {
		t.Fatal(err)
	}
	if err := f.service.Handle(baseCtx, job); err != nil {
		t.Fatal(err)
	}
	current, err := f.db.GetWorkflowByJob(baseCtx, job.ID)
	if err != nil || current.ID != wf.ID || current.Verdict != review.VerdictIncomplete {
		t.Fatalf("workflow=%+v err=%v", current, err)
	}
	after, err := f.db.GetWorkflowDossier(baseCtx, wf.ID)
	if err != nil || !bytes.Equal(before.Data, after.Data) {
		t.Fatalf("snapshot changed: %v", err)
	}
	final, err := f.engine.Snapshot(baseCtx, wf.ID)
	if err != nil || !bytes.Equal(gapOutput, final.Nodes[gapID].Output) {
		t.Fatalf("gap node changed: %v", err)
	}
	if provider.tools != 0 || provider.calls != 0 {
		t.Fatal("retry upgraded MCP permission")
	}
}
