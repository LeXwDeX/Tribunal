package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

// deadlineTester records the deadline each provider probe receives. The
// handler must derive a bounded context from the request; a plain HTTP
// request context carries no deadline, so an unbounded probe hangs until the
// upstream answers or the client disconnects.
type deadlineTester struct {
	mu          sync.Mutex
	deadlines   []time.Duration
	hasDeadline []bool
}

func (d *deadlineTester) TestProvider(ctx context.Context, name string) (ProviderTestResult, error) {
	deadline, ok := ctx.Deadline()
	d.mu.Lock()
	d.hasDeadline = append(d.hasDeadline, ok)
	d.deadlines = append(d.deadlines, time.Until(deadline))
	d.mu.Unlock()
	if !ok {
		return ProviderTestResult{}, errors.New("probe context has no deadline")
	}
	return ProviderTestResult{Model: "probe-model", LatencyMS: 1}, nil
}

func (d *deadlineTester) ProviderInfos() []llm.ProviderInfo { return nil }

func (d *deadlineTester) lastDeadline(t *testing.T) (time.Duration, bool) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.deadlines) == 0 {
		t.Fatal("provider probe was not called")
	}
	return d.deadlines[len(d.deadlines)-1], d.hasDeadline[len(d.hasDeadline)-1]
}

func assertBoundedProbe(t *testing.T, tester *deadlineTester) {
	t.Helper()
	remaining, ok := tester.lastDeadline(t)
	if !ok {
		t.Fatal("provider probe received an unbounded context")
	}
	if remaining <= 0 || remaining > draftTestTimeout {
		t.Fatalf("probe deadline = %v, want within %v", remaining, draftTestTimeout)
	}
}

func TestProviderTestHandlerBoundsTheProbe(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	tester := &deadlineTester{}
	env.api.llm = tester
	response := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/providers/claude/test", nil, nil)
	assertStatus(t, response, http.StatusOK)
	if body := env.decode(t, response); body["ok"] != true {
		t.Fatalf("body = %v", body)
	}
	assertBoundedProbe(t, tester)
}

func TestModelTestHandlerBoundsTheProbe(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	tester := &deadlineTester{}
	env.api.llm = tester
	response := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/models/user:demo/test", nil, nil)
	assertStatus(t, response, http.StatusOK)
	if body := env.decode(t, response); body["ok"] != true {
		t.Fatalf("body = %v", body)
	}
	assertBoundedProbe(t, tester)
}

func TestModelTestHandlerMapsEgressDenied(t *testing.T) {
	env := newM6Env(t, m6Options{platform: systemPlatform()})
	env.api.llm = &fakeLLM{err: &llm.Error{Kind: llm.ErrEgress, Err: llm.ErrEgressDenied}}
	response := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/models/user:demo/test", nil, nil)
	assertStatus(t, response, http.StatusUnprocessableEntity)
	if !strings.Contains(string(response.body), "egress_denied") {
		t.Fatalf("missing egress_denied error: %s", response.body)
	}
}
