package server

import (
	"context"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// attemptStream issues one stream request and returns the status with the body
// when it was rejected. A successful response is closed immediately: reading an
// open event stream would block.
func attemptStream(t *testing.T, client *m6Client, workflowID string) (int, []byte) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		client.base+"/api/v1/reviews/"+workflowID+"/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	if client.bearer != "" {
		req.Header.Set("Authorization", "Bearer "+client.bearer)
	}
	resp, err := client.client.Do(req)
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return resp.StatusCode, data
}

func assertStreamLimitRejected(t *testing.T, status int, body []byte) {
	t.Helper()
	if status != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429, body=%s", status, body)
	}
	envelope, ok := decodeMap(t, body)["error"].(map[string]any)
	if !ok {
		t.Fatalf("error envelope missing: %s", body)
	}
	if envelope["code"] != "stream_limit_exceeded" {
		t.Fatalf("error code = %v, want stream_limit_exceeded", envelope["code"])
	}
}

// waitForStreamSlot retries while the released connection unwinds. The stream
// handler runs in its own goroutine, so slot release is not synchronous with
// closing the client body.
func waitForStreamSlot(t *testing.T, client *m6Client, workflowID string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		status, body := attemptStream(t, client, workflowID)
		if status == http.StatusOK {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stream slot was not released: status=%d body=%s", status, body)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestReviewStreamRejectsPerUserConcurrencyLimit(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond,
		streamMaxConcurrent: 8, streamMaxPerUser: 1})
	fake.setWorkflow(store.Workflow{ID: "wf-limit", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-limit", store.WorkflowNode{WorkflowID: "wf-limit", ID: "plan", Kind: "plan", Status: "running"})

	client := env.businessClient()
	response, events := streamEventsAs(t, env, client, "wf-limit")

	status, body := attemptStream(t, client, "wf-limit")
	assertStreamLimitRejected(t, status, body)

	_ = response.Body.Close()
	drainEvents(t, events)
	waitForStreamSlot(t, client, "wf-limit")
}

func TestReviewStreamRejectsGlobalConcurrencyLimit(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond,
		streamMaxConcurrent: 1, streamMaxPerUser: 8})
	fake.setWorkflow(store.Workflow{ID: "wf-global", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-global", store.WorkflowNode{WorkflowID: "wf-global", ID: "plan", Kind: "plan", Status: "running"})

	env.grant(8, 42, store.ProjectAccessRead)
	bob := env.loginAs("code-bob")
	alice := env.businessClient()

	aliceResponse, aliceEvents := streamEventsAs(t, env, alice, "wf-global")
	status, body := attemptStream(t, bob, "wf-global")
	assertStreamLimitRejected(t, status, body)

	_ = aliceResponse.Body.Close()
	drainEvents(t, aliceEvents)
	waitForStreamSlot(t, bob, "wf-global")
}

// outOfBandGrantRemoval deletes the grant without going through the server so
// only the re-authentication TTL can notice it.
func outOfBandGrantRemoval(t *testing.T, env *m6Env, projectID, userID int64) {
	t.Helper()
	if err := env.sqlite.DeleteProjectGrant(context.Background(), projectID, userID); err != nil {
		t.Fatalf("DeleteProjectGrant: %v", err)
	}
}

func TestReviewStreamCachesReauthenticationUntilRevocation(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond,
		streamAuthTTL: time.Hour})
	fake.setWorkflow(store.Workflow{ID: "wf-auth", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-auth", store.WorkflowNode{WorkflowID: "wf-auth", ID: "plan", Kind: "plan", Status: "running"})

	env.grant(8, 42, store.ProjectAccessRead)
	bob := env.loginAs("code-bob")

	response, events := streamEventsAs(t, env, bob, "wf-auth")
	outOfBandGrantRemoval(t, env, 42, 8)

	// The cached grant hides an out-of-band change for the whole TTL: idle
	// polls must not repeat the full authentication and grant queries.
	select {
	case event, open := <-events:
		t.Fatalf("stream ended after out-of-band revocation: event=%+v open=%v", event, open)
	case <-time.After(150 * time.Millisecond):
	}

	_ = response.Body.Close()
	drainEvents(t, events)
}

func TestReviewStreamClosesOnServerRevocation(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond,
		streamAuthTTL: time.Hour})
	fake.setWorkflow(store.Workflow{ID: "wf-revoke", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-revoke", store.WorkflowNode{WorkflowID: "wf-revoke", ID: "plan", Kind: "plan", Status: "running"})

	admin := env.businessClient()
	env.grant(8, 42, store.ProjectAccessRead)
	bob := env.loginAs("code-bob")

	response, events := streamEventsAs(t, env, bob, "wf-revoke")

	// The TTL is an hour, so only the revocation epoch can close the stream.
	revoked := admin.request(http.MethodDelete, "/api/v1/admin/projects/42/grants/8", nil, nil)
	assertStatus(t, revoked, http.StatusNoContent)
	drainEvents(t, events)
	_ = response.Body.Close()
}

func TestReviewStreamRevalidatesAfterAuthTTL(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond,
		streamAuthTTL: 40 * time.Millisecond})
	fake.setWorkflow(store.Workflow{ID: "wf-ttl", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-ttl", store.WorkflowNode{WorkflowID: "wf-ttl", ID: "plan", Kind: "plan", Status: "running"})

	env.grant(8, 42, store.ProjectAccessRead)
	bob := env.loginAs("code-bob")

	response, events := streamEventsAs(t, env, bob, "wf-ttl")
	outOfBandGrantRemoval(t, env, 42, 8)

	// The short TTL bounds how long a revoked grant can keep a stream open
	// even when the revocation never reaches this process.
	drainEvents(t, events)
	_ = response.Body.Close()
}

func TestReviewStreamSkipsSnapshotReloadWhenRevisionIsUnchanged(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond})
	fake.setWorkflow(store.Workflow{ID: "wf-rev", Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes("wf-rev", store.WorkflowNode{WorkflowID: "wf-rev", ID: "plan", Kind: "plan", Status: "running"})

	response, events := streamEvents(t, env, "wf-rev")
	waitFor(t, "idle stream polls", func() bool { return fake.snapshotProbeCalls() >= 5 })
	if loads := fake.snapshotCollectionLoads(); loads != 1 {
		t.Fatalf("collection loads after idle polls = %d, want 1", loads)
	}

	fake.setNodes("wf-rev", store.WorkflowNode{WorkflowID: "wf-rev", ID: "plan", Kind: "plan", Status: "completed", Attempt: 1})
	waitFor(t, "reload after node change", func() bool { return fake.snapshotCollectionLoads() == 2 })

	_ = response.Body.Close()
	drainEvents(t, events)
}

// waitFor polls condition until it holds, failing the test after a bounded
// wait. It keeps timing assertions robust on a loaded machine.
func waitFor(t *testing.T, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
