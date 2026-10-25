package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
)

type sseEvent struct {
	name string
	data string
}

// streamEvents opens the SSE endpoint and returns a channel closed when the
// handler ends. The initial snapshot is loaded before the response headers are
// written, so mutating the fake store after Do returns is race-free.
func streamEvents(t *testing.T, env *m6Env, workflowID string) (*http.Response, <-chan sseEvent) {
	t.Helper()
	return streamEventsAs(t, env, env.businessClient(), workflowID)
}

func streamEventsAs(t *testing.T, env *m6Env, client *m6Client, workflowID string) (*http.Response, <-chan sseEvent) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		env.server.URL+"/api/v1/reviews/"+workflowID+"/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	resp, err := client.client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("content type = %q", contentType)
	}
	events := make(chan sseEvent, 32)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(resp.Body)
		var name, data string
		for scanner.Scan() {
			line := scanner.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				name = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = strings.TrimPrefix(line, "data: ")
			case line == "":
				if name != "" {
					events <- sseEvent{name: name, data: data}
					name, data = "", ""
				}
			}
		}
	}()
	return resp, events
}

func collectEvents(t *testing.T, events <-chan sseEvent, want int) []sseEvent {
	t.Helper()
	collected := make([]sseEvent, 0, want)
	timeout := time.After(5 * time.Second)
	for len(collected) < want {
		select {
		case event, ok := <-events:
			if !ok {
				return collected
			}
			collected = append(collected, event)
		case <-timeout:
			t.Fatalf("timed out after %d/%d events", len(collected), want)
		}
	}
	return collected
}

func drainEvents(t *testing.T, events <-chan sseEvent) []sseEvent {
	t.Helper()
	collected := make([]sseEvent, 0)
	timeout := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-events:
			if !ok {
				return collected
			}
			collected = append(collected, event)
		case <-timeout:
			t.Fatalf("stream did not terminate, events=%v", collected)
		}
	}
}

func TestReviewStreamEmitsNodeAndWorkflowEvents(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond})
	started := time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	fake.setWorkflow(store.Workflow{ID: "wf-s", Kind: "review", ProjectID: 42, Trigger: "api",
		Status: "running", CreatedAt: started, StartedAt: &started})
	fake.setNodes("wf-s", store.WorkflowNode{WorkflowID: "wf-s", ID: "plan", Kind: "plan",
		Status: "running", Attempt: 0})

	_, events := streamEvents(t, env, "wf-s")

	// Wait for the connection preamble to be consumed, then advance the
	// workflow. The handler already has its initial snapshot.
	time.Sleep(20 * time.Millisecond)
	fake.setWorkflow(store.Workflow{ID: "wf-s", Kind: "review", ProjectID: 42, Trigger: "api",
		Status: "completed", Verdict: "PASS", CreatedAt: started, StartedAt: &started})
	fake.setNodes("wf-s",
		store.WorkflowNode{WorkflowID: "wf-s", ID: "plan", Kind: "plan", Status: "completed", Attempt: 1},
		store.WorkflowNode{WorkflowID: "wf-s", ID: "review.std.p1", Kind: "review.standards",
			Status: "completed", DependsOn: []string{"plan"}, Attempt: 1},
	)

	received := drainEvents(t, events)
	var sawPlan, sawReview, sawWorkflow bool
	for _, event := range received {
		var payload map[string]any
		if err := json.Unmarshal([]byte(event.data), &payload); err != nil {
			t.Fatalf("event %s data %q: %v", event.name, event.data, err)
		}
		switch event.name {
		case "node":
			if payload["id"] == "plan" && payload["status"] == "completed" {
				sawPlan = true
			}
			if payload["id"] == "review.std.p1" {
				sawReview = true
			}
		case "workflow":
			if payload["status"] == "completed" && payload["verdict"] == "PASS" {
				sawWorkflow = true
			}
		}
	}
	if !sawPlan || !sawReview || !sawWorkflow {
		t.Fatalf("events missing: node=%v review=%v workflow=%v all=%v", sawPlan, sawReview, sawWorkflow, received)
	}
}

func TestReviewStreamEmitsFindingsAndPublications(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond})
	fake.setWorkflow(store.Workflow{ID: "wf-f", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-f", store.WorkflowNode{WorkflowID: "wf-f", ID: "plan", Kind: "plan", Status: "completed"})

	_, events := streamEvents(t, env, "wf-f")
	time.Sleep(20 * time.Millisecond)
	fake.setFindings("wf-f", store.Finding{ID: "f9", WorkflowID: "wf-f", OriginNode: "plan",
		Severity: "P1", Category: "bug", Title: "t", Body: "b", Decision: "publish"})
	fake.setPublications("wf-f", store.Publication{ID: "p9", Kind: "summary", GitlabID: "777", WorkflowID: "wf-f"})
	fake.setWorkflow(store.Workflow{ID: "wf-f", Kind: "review", ProjectID: 42, Trigger: "api",
		Status: "completed", Verdict: "PASS"})

	received := drainEvents(t, events)
	var sawFinding, sawPublished bool
	for _, event := range received {
		switch event.name {
		case "finding":
			if strings.Contains(event.data, `"id":"f9"`) && strings.Contains(event.data, `"decision":"publish"`) {
				sawFinding = true
			}
		case "published":
			if strings.Contains(event.data, `"kind":"summary"`) && strings.Contains(event.data, `"gitlab_id":"777"`) {
				sawPublished = true
			}
		}
	}
	if !sawFinding || !sawPublished {
		t.Fatalf("finding=%v published=%v events=%v", sawFinding, sawPublished, received)
	}
}

func TestReviewStreamTerminatesOnCancel(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond})
	fake.setWorkflow(store.Workflow{ID: "wf-c", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-c", store.WorkflowNode{WorkflowID: "wf-c", ID: "plan", Kind: "plan", Status: "running"})

	_, events := streamEvents(t, env, "wf-c")
	time.Sleep(20 * time.Millisecond)
	fake.setWorkflow(store.Workflow{ID: "wf-c", Kind: "review", ProjectID: 42, Trigger: "api", Status: "cancelled"})

	received := drainEvents(t, events)
	if len(received) == 0 {
		t.Fatal("no events before termination")
	}
	last := received[len(received)-1]
	if last.name != "workflow" || !strings.Contains(last.data, `"status":"cancelled"`) {
		t.Fatalf("last event = %+v (all=%v)", last, received)
	}
}

func TestReviewStreamTerminalWorkflowEndsImmediately(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond})
	fake.setWorkflow(store.Workflow{ID: "wf-done", Kind: "review", ProjectID: 42, Trigger: "api",
		Status: "completed", Verdict: "BLOCKING"})
	fake.setNodes("wf-done")

	_, events := streamEvents(t, env, "wf-done")
	received := drainEvents(t, events)
	if len(received) != 1 || received[0].name != "workflow" {
		t.Fatalf("events = %v", received)
	}
	if !strings.Contains(received[0].data, `"verdict":"BLOCKING"`) {
		t.Fatalf("workflow event = %q", received[0].data)
	}
}

func TestReviewStreamUnknownWorkflow(t *testing.T) {
	env := newM6Env(t, m6Options{store: newFakeReviewStore()})
	response := env.businessClient().request(http.MethodGet, "/api/v1/reviews/missing/stream", nil, nil)
	assertStatus(t, response, http.StatusNotFound)
}
