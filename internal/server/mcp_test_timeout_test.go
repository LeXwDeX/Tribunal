package server

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/config"
)

func TestMCPTestTimeoutDerivesFromWriteTimeout(t *testing.T) {
	cases := []struct {
		write time.Duration
		want  time.Duration
	}{
		{0, 30 * time.Second},
		{120 * time.Second, 30 * time.Second},
		{60 * time.Second, 30 * time.Second},
		{40 * time.Second, 30 * time.Second},
		{30 * time.Second, 22500 * time.Millisecond},
		{8 * time.Second, 6 * time.Second},
		{1 * time.Second, 750 * time.Millisecond},
	}
	for _, tc := range cases {
		if got := mcpTestTimeoutFor(tc.write); got != tc.want {
			t.Fatalf("mcpTestTimeoutFor(%s) = %s, want %s", tc.write, got, tc.want)
		}
	}
}

// deadlineMCP records the handler deadline of one test call.
type deadlineMCP struct {
	deadline time.Time
}

func (d *deadlineMCP) TestMCPServer(ctx context.Context, _ MCPServer) (MCPTestResult, error) {
	d.deadline, _ = ctx.Deadline()
	return MCPTestResult{}, nil
}

func (d *deadlineMCP) TestMCPDraft(ctx context.Context, _ MCPDraftInput) (MCPTestResult, error) {
	d.deadline, _ = ctx.Deadline()
	return MCPTestResult{}, nil
}

// TestMCPTestHandlerUsesWriteTimeoutBudget checks the wired handler picks the
// derived budget instead of a fixed 30 seconds.
func TestMCPTestHandlerUsesWriteTimeoutBudget(t *testing.T) {
	env := newM6Env(t, m6Options{platform: &config.Config{
		Server: config.ServerConfig{WriteTimeout: 8 * time.Second},
	}})
	if got, want := env.api.mcpTestTimeout, 6*time.Second; got != want {
		t.Fatalf("wired test timeout = %s, want %s", got, want)
	}
	seedMCPServer(env)
	tester := &deadlineMCP{}
	env.api.mcp = tester
	before := time.Now()
	response := env.loginAs("code-admin").request(
		"POST", "/api/v1/mcp-servers/mcp-1/test", nil, nil)
	assertStatus(t, response, http.StatusOK)
	if tester.deadline.IsZero() {
		t.Fatal("test handler passed no deadline")
	}
	budget := tester.deadline.Sub(before)
	if budget < 5*time.Second || budget > 7*time.Second {
		t.Fatalf("handler deadline budget = %s, want about 6s", budget)
	}
}
