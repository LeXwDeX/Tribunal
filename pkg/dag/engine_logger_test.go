package dag

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func assertFailureWarn(t *testing.T, logged, errorKind string) {
	t.Helper()
	for _, want := range []string{
		"level=WARN", "workflow_id=", "node_id=a", "node_kind=x", "error_kind=" + errorKind,
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("log %q does not contain %q", logged, want)
		}
	}
	if strings.Count(logged, "level=WARN") != 1 {
		t.Fatalf("want one WARN, got %q", logged)
	}
}

func warnNodeSpec() NodeSpec { return NodeSpec{ID: "a", Kind: "x", Required: true} }

// queue_timeout from the scheduling pass is the path that never reaches the
// executor; it must still produce one attributable WARN.
func TestEngineWarnsOnQueueTimeoutFromSchedule(t *testing.T) {
	var logs bytes.Buffer
	engine := fastEngine(funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{DefaultNodeTimeout: 10 * time.Minute, Logger: testLogger(&logs)})
	now := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)

	view := planScheduleView(0, now.Add(-11*time.Minute))
	events, starts := engine.planSchedule(view, now)
	if len(events) != 1 || len(starts) != 0 {
		t.Fatalf("events=%v starts=%v", events, starts)
	}
	engine.warnScheduledFailures(view.ID, view, events)
	assertFailureWarn(t, logs.String(), "queue_timeout")
}

// A node that waits on the concurrency semaphore and expires while queued takes
// the second queue_timeout path.
func TestEngineWarnsOnQueueTimeoutWhileWaiting(t *testing.T) {
	var logs bytes.Buffer
	engine := fastEngine(funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{}, nil
	}), Options{
		MaxConcurrentNodes: 1,
		DefaultNodeTimeout: time.Second,
		Logger:             testLogger(&logs),
	})
	engine.sem <- struct{}{}
	defer func() { <-engine.sem }()
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "a", Kind: "x", Required: true, Timeout: 40 * time.Millisecond},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	assertFailureWarn(t, logs.String(), "queue_timeout")
}

func TestEngineWarnsOnInvalidOutput(t *testing.T) {
	var logs bytes.Buffer
	engine := fastEngine(funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: []byte(`{`)}, nil
	}), Options{Logger: testLogger(&logs)})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{warnNodeSpec()}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	assertFailureWarn(t, logs.String(), "invalid_output")
}

func TestEngineWarnsOnExpandFailure(t *testing.T) {
	var logs bytes.Buffer
	exec := funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return output(t, map[string]any{"ok": true}), nil
	})
	exp := funcExpander(func(context.Context, WorkflowView, NodeSpec, json.RawMessage) (Extension, error) {
		return Extension{}, errors.New("expander rejected")
	})
	engine := NewEngine(NewMemoryEventStore(), exec, exp, Options{Logger: testLogger(&logs)})
	wfID, err := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{
		{ID: "a", Kind: "x", Required: true, Expands: true},
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	assertFailureWarn(t, logs.String(), "expand_failed")
}

// The logger is optional: a nil logger keeps the old silent behavior and must
// not panic.
func TestEngineWithoutLoggerKeepsFailingSilently(t *testing.T) {
	engine := fastEngine(funcExecutor(func(context.Context, NodeInput) (NodeOutput, error) {
		return NodeOutput{Data: []byte(`{`)}, nil
	}), Options{})
	wfID, _ := engine.Create(context.Background(), GraphSpec{Nodes: []NodeSpec{warnNodeSpec()}}, nil)
	if err := engine.Run(context.Background(), wfID); err != nil {
		t.Fatal(err)
	}
	wf, _ := engine.Snapshot(context.Background(), wfID)
	if wf.Nodes["a"].Reason != "invalid_output" {
		t.Fatalf("a: %+v", wf.Nodes["a"])
	}
}
