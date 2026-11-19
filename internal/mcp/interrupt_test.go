package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const interruptServer = "private-graph"

// toolErrorMarkers reads the optional markers of a tool error through the
// interfaces the agent and review layers use, so the tests do not depend on
// the concrete error type.
type toolErrorMarkers struct {
	unavailable bool
	interrupted bool
	hasFeedback bool
	feedback    string
}

func markersOf(err error) toolErrorMarkers {
	var m toolErrorMarkers
	var unavailable interface{ ToolUnavailable() bool }
	if errors.As(err, &unavailable) {
		m.unavailable = unavailable.ToolUnavailable()
	}
	var interrupted interface{ ToolInterrupted() bool }
	if errors.As(err, &interrupted) {
		m.interrupted = interrupted.ToolInterrupted()
	}
	var retry interface{ AgentRetryFeedback() string }
	if errors.As(err, &retry) {
		m.hasFeedback = true
		m.feedback = retry.AgentRetryFeedback()
	}
	return m
}

// newSeededManager returns a manager for one stdio server whose command does
// not exist, so a call that is not served from the seeded cache entry fails to
// connect at once instead of starting a process.
func newSeededManager(t *testing.T, server string) (*Manager, entryKey, ServerConfig) {
	t.Helper()
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project:   Config{MCPServers: map[string]ServerConfig{server: {Command: "tribunal-test-missing-command"}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { _ = m.Close() })
	cfg := m.servers[server]
	return m, entryKey{workspace: m.workspace, server: server, fingerprint: fingerprint(cfg)}, cfg
}

// seedEntry caches a connected entry without a watcher. It stays cached after
// its connection drops, the window in which a call can still reach it.
func seedEntry(m *Manager, key entryKey, cfg ServerConfig, set func(*serverEntry)) {
	ready := make(chan struct{})
	close(ready)
	entry := &serverEntry{m: m, key: key, cfg: cfg, ready: ready, connected: true, lastUsed: m.now()}
	set(entry)
	m.mu.Lock()
	m.entries[key] = entry
	m.mu.Unlock()
}

func echoToolOf(m *Manager, server string) *mcpTool {
	return &mcpTool{m: m, server: server, remote: "echo", def: llm.ToolDef{Name: ToolName(server, "echo")}}
}

// dropConnection closes the server side and waits until the client has seen it.
func dropConnection(t *testing.T, session *sdk.ClientSession, server *sdk.ServerSession) {
	t.Helper()
	_ = server.Close()
	done := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("client session did not observe the dropped connection")
	}
}

func TestCallOnClosedConnectionIsInterrupted(t *testing.T) {
	var feedbacks []string
	for _, tc := range []struct {
		name   string
		server string
		seed   func(t *testing.T, entry *serverEntry)
	}{
		{
			name:   "session dropped by the server",
			server: interruptServer,
			seed: func(t *testing.T, entry *serverEntry) {
				session, serverSession, cancel := connectedMemorySession(t, false)
				t.Cleanup(cancel)
				t.Cleanup(func() { _ = session.Close() })
				entry.session = session
				dropConnection(t, session, serverSession)
			},
		},
		{
			name:   "session closed from this side",
			server: "other.server",
			seed: func(t *testing.T, entry *serverEntry) {
				session, serverSession, cancel := connectedMemorySession(t, false)
				t.Cleanup(cancel)
				t.Cleanup(func() { _ = serverSession.Close() })
				entry.session = session
				if err := session.Close(); err != nil {
					t.Fatalf("session Close: %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, key, cfg := newSeededManager(t, tc.server)
			seedEntry(m, key, cfg, func(entry *serverEntry) { tc.seed(t, entry) })
			_, err := echoToolOf(m, tc.server).Call(context.Background(), json.RawMessage(`{}`))
			if !errors.Is(err, ErrConnectionClosed) {
				t.Fatalf("Call error = %v, want ErrConnectionClosed", err)
			}
			got := markersOf(err)
			if !got.unavailable || !got.interrupted || !got.hasFeedback || got.feedback == "" {
				t.Fatalf("markers = %+v, want unavailable, interrupted and a retry feedback", got)
			}
			// The feedback reaches the model: it must carry no server name or
			// transport detail.
			for _, leak := range []string{tc.server, "tools/list", "EOF", "closing"} {
				if strings.Contains(got.feedback, leak) {
					t.Fatalf("feedback %q leaks %q", got.feedback, leak)
				}
			}
			// It holds for stdio and HTTP servers alike: nothing restarts on an
			// HTTP server, so the hint only promises a new connection.
			if strings.Contains(got.feedback, "restart") || !strings.Contains(got.feedback, "re-established") {
				t.Fatalf("feedback %q must promise a new connection, not a restart", got.feedback)
			}
			feedbacks = append(feedbacks, got.feedback)
		})
	}
	if len(feedbacks) == 2 && feedbacks[0] != feedbacks[1] {
		t.Fatalf("feedback differs by server or cause: %q vs %q", feedbacks[0], feedbacks[1])
	}
}

// lateCancelContext reports cancellation only when asked: its Done channel is
// nil, so the cache lookup proceeds, while Err already says the caller gave up.
type lateCancelContext struct{ context.Context }

func (lateCancelContext) Err() error { return context.Canceled }

func TestCallClosedConnectionOfDoneCallerIsNotInterrupted(t *testing.T) {
	m, key, cfg := newSeededManager(t, interruptServer)
	seedEntry(m, key, cfg, func(entry *serverEntry) {
		session, serverSession, cancel := connectedMemorySession(t, false)
		t.Cleanup(cancel)
		t.Cleanup(func() { _ = session.Close() })
		entry.session = session
		dropConnection(t, session, serverSession)
	})
	_, err := echoToolOf(m, interruptServer).Call(lateCancelContext{context.Background()}, json.RawMessage(`{}`))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want the caller's cancellation", err)
	}
	if got := markersOf(err); !got.unavailable || got.interrupted || got.hasFeedback {
		t.Fatalf("markers = %+v, want a plain unavailable tool for a caller that is done", got)
	}
}

func TestCallConnectFailureIsNotInterrupted(t *testing.T) {
	m, _, _ := newSeededManager(t, interruptServer)
	_, err := echoToolOf(m, interruptServer).Call(context.Background(), json.RawMessage(`{}`))
	if !errors.Is(err, ErrConnectFailed) {
		t.Fatalf("Call error = %v, want ErrConnectFailed", err)
	}
	if got := markersOf(err); !got.unavailable || got.interrupted || got.hasFeedback {
		t.Fatalf("markers = %+v, want a plain unavailable tool", got)
	}
}

func TestStdioCallTimeoutIsNotInterrupted(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{CallTimeout: Duration(300 * time.Millisecond)}, nil)
	set, err := m.Tools(context.Background(), []string{"mcp__local__slow"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	_, err = findTool(t, set.Tools, "mcp__local__slow").Call(context.Background(), json.RawMessage(`{"ms":2000}`))
	if !errors.Is(err, ErrCallFailed) || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("slow call error = %v", err)
	}
	if got := markersOf(err); !got.unavailable || got.interrupted || got.hasFeedback {
		t.Fatalf("markers = %+v, want a plain unavailable tool: a timeout is not retried", got)
	}
}

func TestCallRefusedByAnnotationsOrDuplicateIsNotInterrupted(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(server *sdk.Server, state *annotationServerState, calls *atomic.Int32)
	}{
		{"annotations no longer permit the tool", func(server *sdk.Server, _ *annotationServerState, calls *atomic.Int32) {
			addAnnotatedTool(server, "change", &sdk.ToolAnnotations{}, calls)
		}},
		{"duplicate definitions", func(_ *sdk.Server, state *annotationServerState, _ *atomic.Int32) {
			state.duplicate.Store(true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, manager, state := annotationServer(t)
			var calls atomic.Int32
			addAnnotatedTool(server, "change", &sdk.ToolAnnotations{ReadOnlyHint: true}, &calls)
			set, err := manager.Tools(context.Background(), []string{"mcp__remote__change"})
			if err != nil || len(set.Tools) != 1 {
				t.Fatalf("binding = %+v, %v", set, err)
			}
			tc.mutate(server, state, &calls)
			_, err = set.Tools[0].Call(context.Background(), json.RawMessage(`{}`))
			if !errors.Is(err, ErrCallFailed) || calls.Load() != 0 {
				t.Fatalf("Call: calls=%d err=%v, want a refusal", calls.Load(), err)
			}
			if got := markersOf(err); !got.unavailable || got.interrupted || got.hasFeedback {
				t.Fatalf("markers = %+v, want a plain unavailable tool", got)
			}
		})
	}
}
