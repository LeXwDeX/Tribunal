package server

import (
	"bufio"
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// createSessionAPIToken creates one API token owned by the client's browser
// session and returns its plaintext and id.
func createSessionAPIToken(t *testing.T, client *m6Client) (token, id string) {
	t.Helper()
	response := client.json(http.MethodPost, "/api/v1/api-tokens",
		map[string]any{"name": "ci", "expires_in_days": 30})
	if response.status != http.StatusCreated {
		t.Fatalf("create API token = %d", response.status)
	}
	body := decodeMap(t, response.body)
	token, _ = body["token"].(string)
	id, _ = body["id"].(string)
	if token == "" || id == "" {
		t.Fatal("create API token response is missing id or token")
	}
	return token, id
}

// seedRunningStreamWorkflow registers a running review the stream can follow.
func seedRunningStreamWorkflow(fake *fakeReviewStore, workflowID string) {
	fake.setWorkflow(store.Workflow{ID: workflowID, Kind: "review", ProjectID: 42, Trigger: "api", Status: "running"})
	fake.setNodes(workflowID, store.WorkflowNode{WorkflowID: workflowID, ID: "plan", Kind: "plan", Status: "running"})
}

// readStreamUntilClosed consumes the stream body and reports whether the
// server closed it before ctx expired.
func readStreamUntilClosed(t *testing.T, ctx context.Context, response *http.Response) bool {
	t.Helper()
	defer response.Body.Close()
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
	}
	return ctx.Err() == nil
}

// TestReviewStreamClosesAfterTokenRenewal proves that rotating an API token
// invalidates the stream authentication an old plaintext established, even
// when the caching TTL would otherwise keep the stream open.
func TestReviewStreamClosesAfterTokenRenewal(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond,
		streamAuthTTL: time.Hour})
	seedRunningStreamWorkflow(fake, "wf-renew")
	env.grant(8, 42, store.ProjectAccessRead)
	bob := env.loginAs("code-bob")
	token, id := createSessionAPIToken(t, bob)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		env.server.URL+"/api/v1/reviews/wf-renew/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", response.StatusCode)
	}

	renewed := bob.json(http.MethodPost, "/api/v1/api-tokens/"+id+"/renew",
		map[string]any{"expires_in_days": 30})
	if renewed.status != http.StatusCreated {
		t.Fatalf("renew status = %d", renewed.status)
	}

	if !readStreamUntilClosed(t, ctx, response) {
		t.Fatal("stream opened with the old plaintext stayed open after renewal")
	}
}

// TestReviewStreamDoesNotCacheRevocationRacingFirstAuth covers a revocation
// that lands after apiAuth accepted the request but before the stream handler
// sampled the auth epoch. The handler must not adopt the post-revocation epoch
// as verified, or the cached authentication would hide the revocation for the
// whole TTL.
func TestReviewStreamDoesNotCacheRevocationRacingFirstAuth(t *testing.T) {
	fake := newFakeReviewStore()
	env := newM6Env(t, m6Options{store: fake, streamPoll: 10 * time.Millisecond,
		streamAuthTTL: time.Hour})
	seedRunningStreamWorkflow(fake, "wf-race")
	env.grant(8, 42, store.ProjectAccessRead)
	bob := env.loginAs("code-bob")
	token, id := createSessionAPIToken(t, bob)

	// The review guard reads the workflow after apiAuth accepted the request.
	// Blocking there leaves the window open to revoke the token in between.
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	fake.setGetWorkflowHook(func() {
		once.Do(func() { close(entered) })
		<-release
		fake.setGetWorkflowHook(nil)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet,
		env.server.URL+"/api/v1/reviews/wf-race/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Header.Set("Authorization", "Bearer "+token)

	type streamResult struct {
		response *http.Response
		err      error
	}
	opened := make(chan streamResult, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		opened <- streamResult{response: response, err: err}
	}()

	<-entered
	deleted := bob.request(http.MethodDelete, "/api/v1/api-tokens/"+id, nil, nil)
	assertStatus(t, deleted, http.StatusNoContent)
	close(release)

	result := <-opened
	if result.err != nil {
		t.Fatalf("open stream: %v", result.err)
	}
	if result.response.StatusCode != http.StatusOK {
		t.Fatalf("stream status = %d", result.response.StatusCode)
	}
	if !readStreamUntilClosed(t, ctx, result.response) {
		t.Fatal("stream cached the post-revocation epoch and stayed open")
	}
}
