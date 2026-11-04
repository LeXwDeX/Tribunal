package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/llm"
)

// forcingClient declares forced tool_choice support. forcedErr is returned
// for a forced choice, for example HTTP 400 from a reasoning gateway.
type forcingClient struct {
	*fakeClient
	name      string
	forcedErr error
	forced    int
}

func (c *forcingClient) Info() llm.ProviderInfo {
	return llm.ProviderInfo{Name: c.name, Model: "m", ForcedToolChoice: true}
}

func (c *forcingClient) Generate(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	if req.ToolChoice != "" {
		c.forced++
		if c.forcedErr != nil {
			c.fakeClient.mu.Lock()
			c.fakeClient.reqs = append(c.fakeClient.reqs, req)
			c.fakeClient.mu.Unlock()
			return nil, c.forcedErr
		}
	}
	if hasSubmitPrompt(req) {
		c.fakeClient.mu.Lock()
		c.fakeClient.reqs = append(c.fakeClient.reqs, req)
		c.fakeClient.mu.Unlock()
		return toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`)), nil
	}
	return c.fakeClient.Generate(ctx, req)
}

func TestRunForcesSubmitWhenProviderSupportsIt(t *testing.T) {
	client := &forcingClient{name: "forcing-ok", fakeClient: &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
	}}}
	t.Cleanup(func() { rejectedForcedChoice.Delete(forcedChoiceKey(client.Info())) })
	spec := testSpec(client, echoTool{name: "echo"})
	spec.MaxTurns = 2
	if _, err := Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	reqs := client.requests()
	if reqs[0].ToolChoice != "" || reqs[1].ToolChoice != SubmitToolName || !hasSubmitPrompt(reqs[1]) {
		t.Fatalf("tool choices = %q, %q; the last turn must force submit and keep the prompt", reqs[0].ToolChoice, reqs[1].ToolChoice)
	}
}

// forcedGatewayClient is a real openai-responses client that declares
// forced tool_choice support. The gateway answers an object tool_choice
// (a forced function) with HTTP 400 and, unless alwaysReject, submits for
// any other request.
func forcedGatewayClient(t *testing.T, alwaysReject bool) (llm.Client, *atomic.Int32) {
	return forcedGatewayClientStatus(t, alwaysReject, http.StatusBadRequest)
}

func forcedGatewayClientStatus(t *testing.T, alwaysReject bool, status int) (llm.Client, *atomic.Int32) {
	t.Helper()
	good := safetyResponse(t, "openai-responses", "", false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
	var forced atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if _, isObject := body["tool_choice"].(map[string]any); isObject || alwaysReject {
			if isObject {
				forced.Add(1)
			}
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"Thinking mode does not support this tool_choice","type":"invalid_request_error"}}`))
			return
		}
		_, _ = w.Write(good)
	}))
	t.Cleanup(server.Close)
	t.Setenv("FORCED_GATEWAY_TEST_KEY", "key")
	registry, err := llm.NewRegistry(map[string]config.ProviderConfig{"forced": {
		Protocol: "openai-responses", Model: "test", BaseURL: server.URL, APIKeyEnv: "FORCED_GATEWAY_TEST_KEY",
		MaxTokens: 512, ForcedToolChoice: "supported",
	}}, llm.WithRetryBaseDelay(time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	client, err := registry.Client("forced")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { rejectedForcedChoice.Delete(forcedChoiceKey(client.Info())) })
	return client, &forced
}

func TestRunFallsBackAndRemembersRejectedForcedChoice(t *testing.T) {
	client, forced := forcedGatewayClient(t, false)
	spec := testSpec(client)
	spec.MaxTurns = 1
	result, err := Run(context.Background(), spec)
	if err != nil || result.Output == nil || forced.Load() != 1 {
		t.Fatalf("err=%v forced=%d, want a rejected forced request, then submit with the prompt", err, forced.Load())
	}
	// A later run on the same provider does not try the forced choice again.
	if _, err := Run(context.Background(), spec); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if forced.Load() != 1 {
		t.Fatalf("forced choice sent %d times, want 1", forced.Load())
	}
}

func TestRunDoesNotRememberWhenFallbackAlsoFails(t *testing.T) {
	client, forced := forcedGatewayClient(t, true)
	spec := testSpec(client)
	spec.MaxTurns = 1
	for i := 0; i < 2; i++ {
		if _, err := Run(context.Background(), spec); err == nil {
			t.Fatal("Run succeeded against a gateway that rejects every request")
		}
	}
	// The 400 was not about the forced choice, so each run still tries it.
	if forced.Load() != 2 {
		t.Fatalf("forced choice sent %d times, want 2", forced.Load())
	}
}

func TestRunDoesNotFallBackOnLocalBadRequest(t *testing.T) {
	client := &forcingClient{name: "forcing-local", forcedErr: &llm.Error{Kind: llm.ErrBadRequest}, fakeClient: &fakeClient{}}
	t.Cleanup(func() { rejectedForcedChoice.Delete(forcedChoiceKey(client.Info())) })
	spec := testSpec(client)
	spec.MaxTurns = 1
	_, err := Run(context.Background(), spec)
	var failure *llm.Error
	if !errors.As(err, &failure) || failure.Kind != llm.ErrBadRequest || len(client.requests()) != 1 {
		t.Fatalf("err=%v requests=%d, want the local bad request without a fallback", err, len(client.requests()))
	}
	if _, rejected := rejectedForcedChoice.Load(forcedChoiceKey(client.Info())); rejected {
		t.Fatal("a local bad request must not be remembered as a rejected forced choice")
	}
}

func TestRunDoesNotFallBackOnOtherErrors(t *testing.T) {
	client := &forcingClient{name: "forcing-transient", forcedErr: &llm.Error{Kind: llm.ErrTransient}, fakeClient: &fakeClient{}}
	t.Cleanup(func() { rejectedForcedChoice.Delete(forcedChoiceKey(client.Info())) })
	spec := testSpec(client)
	spec.MaxTurns = 1
	_, err := Run(context.Background(), spec)
	var failure *llm.Error
	if !errors.As(err, &failure) || failure.Kind != llm.ErrTransient || len(client.requests()) != 1 {
		t.Fatalf("err=%v requests=%d, want the transient error without a fallback", err, len(client.requests()))
	}
}

func TestRunNeverForcesForUnsupportedProvider(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("c2", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo"})
	spec.MaxTurns = 3
	if _, err := Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	for i, req := range client.requests() {
		if req.ToolChoice != "" {
			t.Fatalf("request %d tool choice = %q; a provider without forced support must only get auto", i+1, req.ToolChoice)
		}
	}
}

func TestRunDoesNotFallBackOnOtherClientErrors(t *testing.T) {
	client, forced := forcedGatewayClientStatus(t, false, http.StatusNotFound)
	spec := testSpec(client)
	spec.MaxTurns = 1
	_, err := Run(context.Background(), spec)
	var failure *llm.Error
	if !errors.As(err, &failure) || failure.Status != http.StatusNotFound || forced.Load() != 1 {
		t.Fatalf("err=%v forced=%d, want the 404 without a fallback", err, forced.Load())
	}
	if _, rejected := rejectedForcedChoice.Load(forcedChoiceKey(client.Info())); rejected {
		t.Fatal("a 404 must not be remembered as a rejected forced choice")
	}
}
