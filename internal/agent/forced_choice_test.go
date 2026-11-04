package agent

import (
	"context"
	"errors"
	"testing"

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

func TestRunFallsBackAndRemembersRejectedForcedChoice(t *testing.T) {
	client := &forcingClient{name: "forcing-rejected", forcedErr: &llm.Error{Kind: llm.ErrBadRequest, Status: 400}, fakeClient: &fakeClient{}}
	spec := testSpec(client)
	spec.MaxTurns = 1
	result, err := Run(context.Background(), spec)
	if err != nil || result.Output == nil {
		t.Fatalf("Run: %v, want submit after falling back to the prompt", err)
	}
	reqs := client.requests()
	if len(reqs) != 2 || reqs[0].ToolChoice != SubmitToolName || reqs[1].ToolChoice != "" || !hasSubmitPrompt(reqs[1]) {
		t.Fatalf("requests = %d; want a forced request, then the same turn with auto and the prompt", len(reqs))
	}
	// A later run on the same provider does not try the forced choice again.
	if _, err := Run(context.Background(), spec); err != nil {
		t.Fatalf("second Run: %v", err)
	}
	if client.forced != 1 {
		t.Fatalf("forced choice sent %d times, want 1", client.forced)
	}
}

func TestRunDoesNotFallBackOnOtherErrors(t *testing.T) {
	client := &forcingClient{name: "forcing-transient", forcedErr: &llm.Error{Kind: llm.ErrTransient}, fakeClient: &fakeClient{}}
	spec := testSpec(client)
	spec.MaxTurns = 1
	_, err := Run(context.Background(), spec)
	var failure *llm.Error
	if !errors.As(err, &failure) || failure.Kind != llm.ErrTransient || len(client.requests()) != 1 {
		t.Fatalf("err=%v requests=%d, want the transient error without a fallback", err, len(client.requests()))
	}
}
