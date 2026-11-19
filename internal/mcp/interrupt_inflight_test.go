package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// cachedEntry returns the entry the manager caches for server, or nil.
func cachedEntry(m *Manager, server string) *serverEntry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[entryKey{workspace: m.workspace, server: server, fingerprint: fingerprint(m.servers[server])}]
}

func isDead(entry *serverEntry) bool {
	entry.mu.Lock()
	defer entry.mu.Unlock()
	return entry.dead
}

// bareEntry is a published entry without a session, as start leaves it.
func bareEntry(manager *Manager, key entryKey) *serverEntry {
	ready := make(chan struct{})
	close(ready)
	return &serverEntry{m: manager, key: key, ready: ready, connected: true}
}

// SDK 1.8.0 completes a call that was in flight when the peer died with the
// bare read error, not with ErrConnectionClosed. The call is an interruption
// all the same, and the next call must reach a new server.
func TestStdioInFlightCallOfExitedServerIsInterruptedAndRetrySucceeds(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{}, nil)
	set, err := m.Tools(context.Background(), []string{"mcp__local__pid", "mcp__local__crash"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	pid := findTool(t, set.Tools, "mcp__local__pid")
	crash := findTool(t, set.Tools, "mcp__local__crash")
	first := pidOf(t, pid)
	crashed := cachedEntry(m, "local")
	if crashed == nil {
		t.Fatal("no cached entry for the running server")
	}

	// The server exits while this call is in flight.
	_, err = crash.Call(context.Background(), json.RawMessage(`{}`))
	if !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("in-flight call error = %v, want ErrConnectionClosed", err)
	}
	if got := markersOf(err); !got.unavailable || !got.interrupted || !got.hasFeedback || got.feedback == "" {
		t.Fatalf("markers = %+v, want unavailable, interrupted and a retry feedback", got)
	}

	// The closed entry is not reused, with no wait for the watcher.
	if !isDead(crashed) {
		t.Fatal("entry of the exited server is not marked dead")
	}
	if cachedEntry(m, "local") == crashed {
		t.Fatal("entry of the exited server is still cached")
	}

	// The retry the model is told to make succeeds at once, on a new process.
	second, err := pid.Call(context.Background(), json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("retry after the interruption: %v", err)
	}
	if got := strings.TrimSpace(second.Content); got == "" || got == first {
		t.Fatalf("retry ran on process %q after %q, want a new process", got, first)
	}
	if current := cachedEntry(m, "local"); current == nil || current == crashed {
		t.Fatalf("retry did not cache a new entry: %v", current)
	}
}

func TestCallErrorEndOfStreamIsClosedConnection(t *testing.T) {
	for name, cause := range map[string]error{
		"io.EOF as the SDK wraps it": fmt.Errorf("calling %q: %w", "tools/call", io.EOF),
		"bare io.EOF":                io.EOF,
		"io.ErrUnexpectedEOF":        fmt.Errorf("calling %q: %w", "tools/call", io.ErrUnexpectedEOF),
	} {
		t.Run(name, func(t *testing.T) {
			key := entryKey{server: "local"}
			manager := &Manager{entries: make(map[entryKey]*serverEntry)}
			entry := bareEntry(manager, key)
			manager.entries[key] = entry
			ctx := context.Background()
			if err := entry.callError(ctx, ctx, cause); !errors.Is(err, ErrConnectionClosed) {
				t.Fatalf("callError = %v, want ErrConnectionClosed", err)
			}
			if !isDead(entry) {
				t.Fatal("entry remained live after the stream ended")
			}
			if _, ok := manager.entries[key]; ok {
				t.Fatal("closed entry remained in manager cache")
			}
		})
	}
}

// A caller that gave up, or a call that ran out of time, says nothing about
// the connection: the entry stays and the error keeps its own meaning.
func TestCallErrorEndOfStreamOfCallerThatIsDoneOrTimedOut(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, stop := context.WithTimeout(context.Background(), time.Nanosecond)
	defer stop()
	<-expired.Done()
	for _, tc := range []struct {
		name         string
		parent, call context.Context
		want         error
		wantText     string
	}{
		{"caller canceled", canceled, canceled, context.Canceled, ""},
		{"call deadline passed", context.Background(), expired, ErrCallFailed, "timed out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key := entryKey{server: "local"}
			manager := &Manager{entries: make(map[entryKey]*serverEntry)}
			entry := bareEntry(manager, key)
			manager.entries[key] = entry
			err := entry.callError(tc.parent, tc.call, fmt.Errorf("calling %q: %w", "tools/call", io.EOF))
			if !errors.Is(err, tc.want) || errors.Is(err, ErrConnectionClosed) || !strings.Contains(err.Error(), tc.wantText) {
				t.Fatalf("callError = %v, want %v and not ErrConnectionClosed", err, tc.want)
			}
			if isDead(entry) || manager.entries[key] != entry {
				t.Fatal("a healthy entry was evicted")
			}
		})
	}
}

func TestCallErrorOtherFailuresAreNotClosedConnection(t *testing.T) {
	for name, cause := range map[string]error{
		"deadline": context.DeadlineExceeded,
		"other":    errors.New("call failure"),
	} {
		t.Run(name, func(t *testing.T) {
			key := entryKey{server: "local"}
			manager := &Manager{entries: make(map[entryKey]*serverEntry)}
			entry := bareEntry(manager, key)
			manager.entries[key] = entry
			ctx := context.Background()
			if err := entry.callError(ctx, ctx, cause); !errors.Is(err, ErrCallFailed) || errors.Is(err, ErrConnectionClosed) {
				t.Fatalf("callError = %v, want ErrCallFailed", err)
			}
			if isDead(entry) {
				t.Fatal("entry was marked dead")
			}
		})
	}
}

// A watcher marks a connected entry dead and records why its session ended. A
// lookup that finds the entry before the watcher removes it gets a closed
// connection, not a connect failure, and removes the entry itself.
func TestAwaitOnEntryThatDiedAfterConnectingEvictsIt(t *testing.T) {
	m, key, cfg := newSeededManager(t, interruptServer)
	seedEntry(m, key, cfg, func(entry *serverEntry) {
		entry.dead = true
		entry.err = errors.New("process exit detail")
	})
	entry := cachedEntry(m, interruptServer)
	if _, err := m.await(context.Background(), entry); !errors.Is(err, ErrConnectionClosed) || errors.Is(err, ErrConnectFailed) {
		t.Fatalf("await error = %v, want ErrConnectionClosed", err)
	}
	if cachedEntry(m, interruptServer) == entry {
		t.Fatal("entry that died after connecting stayed cached")
	}
}

// The call that races the watcher must not fail: it replaces the dead entry
// and runs on a new server.
func TestCallReplacesCachedEntryThatDied(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{}, nil)
	cfg := m.servers["local"]
	key := entryKey{workspace: m.workspace, server: "local", fingerprint: fingerprint(cfg)}
	seedEntry(m, key, cfg, func(entry *serverEntry) {
		entry.dead = true
		entry.err = errors.New("process exit detail")
	})
	dead := cachedEntry(m, "local")
	res, err := echoToolOf(m, "local").Call(context.Background(), json.RawMessage(`{"text":"after"}`))
	if err != nil || strings.TrimSpace(res.Content) != "echo:after" {
		t.Fatalf("call over a dead cached entry = %q, %v, want echo:after", res.Content, err)
	}
	if current := cachedEntry(m, "local"); current == nil || current == dead || isDead(current) {
		t.Fatalf("call did not cache a live replacement: %v", current)
	}
}

func TestCallOnEntryThatNeverConnectedKeepsConnectError(t *testing.T) {
	m, key, cfg := newSeededManager(t, interruptServer)
	seedEntry(m, key, cfg, func(entry *serverEntry) {
		entry.connected = false
		entry.dead = true
		entry.err = errors.New("start failure detail")
	})
	_, err := echoToolOf(m, interruptServer).Call(context.Background(), json.RawMessage(`{}`))
	if !errors.Is(err, ErrConnectFailed) || errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("Call error = %v, want ErrConnectFailed", err)
	}
	if got := markersOf(err); !got.unavailable || got.interrupted || got.hasFeedback {
		t.Fatalf("markers = %+v, want a plain unavailable tool", got)
	}
}

// Ending the stream of an HTTP request leaves the SDK session open. The
// evicted entry is no longer in the cache for Close to find, so the session has
// to be released with it.
func TestCallErrorEndOfStreamReleasesSession(t *testing.T) {
	session, serverSession, cancel := connectedMemorySession(t, false)
	t.Cleanup(cancel)
	t.Cleanup(func() { _ = serverSession.Close() })
	key := entryKey{server: "local"}
	manager := &Manager{entries: make(map[entryKey]*serverEntry)}
	entry := bareEntry(manager, key)
	entry.session = session
	manager.entries[key] = entry

	ctx := context.Background()
	if err := entry.callError(ctx, ctx, fmt.Errorf("calling %q: %w", "tools/call", io.EOF)); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("callError = %v, want ErrConnectionClosed", err)
	}
	released := make(chan struct{})
	go func() {
		_ = session.Wait()
		close(released)
	}()
	select {
	case <-released:
	case <-time.After(5 * time.Second):
		t.Fatal("session of the evicted entry was left open")
	}
}

func TestHTTPCallCutOffMidResponseIsInterruptedAndRetrySucceeds(t *testing.T) {
	inner := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return testServer() },
		&sdk.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	var cut atomic.Bool
	httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Method string `json:"method"`
		}
		// The server drops the connection for the first call and answers the rest.
		if json.Unmarshal(body, &req) == nil && req.Method == "tools/call" && cut.CompareAndSwap(false, true) {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(httpServer.Close)
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project:   Config{MCPServers: map[string]ServerConfig{"remote": {URL: httpServer.URL, XReview: XReviewConfig{CallTimeout: Duration(2 * time.Second)}}}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	closed := make(chan error, 1)
	var closeOnce atomic.Bool
	closeManager := func() {
		if closeOnce.CompareAndSwap(false, true) {
			go func() { closed <- m.Close() }()
		}
	}
	t.Cleanup(closeManager)
	set, err := m.Tools(context.Background(), []string{"mcp__remote__echo"})
	if err != nil || len(set.Tools) != 1 {
		t.Fatalf("Tools = %+v, %v", set, err)
	}
	echo := set.Tools[0]

	_, err = echo.Call(context.Background(), json.RawMessage(`{"text":"first"}`))
	if !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("cut-off call error = %v, want ErrConnectionClosed", err)
	}
	if got := markersOf(err); !got.unavailable || !got.interrupted || !got.hasFeedback {
		t.Fatalf("markers = %+v, want an interrupted call", got)
	}
	res, err := echo.Call(context.Background(), json.RawMessage(`{"text":"retry"}`))
	if err != nil || strings.TrimSpace(res.Content) != "echo:retry" {
		t.Fatalf("retry = %q, %v, want echo:retry", res.Content, err)
	}

	closeManager()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Manager.Close is blocked by a session that was left open")
	}
}

// Closing the manager under a call ends that call, but nothing was
// interrupted that a retry could make up: the manager will not serve it.
func TestManagerCloseDuringInFlightCallIsNotInterrupted(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{}, nil)
	set, err := m.Tools(context.Background(), []string{"mcp__local__crash_when"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	trigger := filepath.Join(t.TempDir(), "crash")
	failed := make(chan error, 1)
	go func() {
		_, err := findTool(t, set.Tools, "mcp__local__crash_when").Call(context.Background(), json.RawMessage(fmt.Sprintf(`{"path":%q}`, trigger)))
		failed <- err
	}()
	waitUntil(t, "the call to reach the server", func() bool {
		entry := cachedEntry(m, "local")
		if entry == nil {
			return false
		}
		entry.mu.Lock()
		defer entry.mu.Unlock()
		return entry.inflight > 0
	})

	// Close waits for the call it cannot end. Once the manager is marked
	// closed, the server dies under the call.
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	waitUntil(t, "Close to start", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.closed
	})
	if err := os.WriteFile(trigger, nil, 0o600); err != nil {
		t.Fatalf("trigger: %v", err)
	}

	select {
	case err := <-failed:
		if err == nil {
			t.Fatal("call succeeded although its server died")
		}
		if got := markersOf(err); !got.unavailable || got.interrupted || got.hasFeedback {
			t.Fatalf("call error = %v, markers = %+v, want a plain unavailable tool", err, got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight call did not end")
	}
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return")
	}

	_, err = echoToolOf(m, "local").Call(context.Background(), json.RawMessage(`{}`))
	if got := markersOf(err); !errors.Is(err, ErrClosed) || !got.unavailable || got.interrupted {
		t.Fatalf("call after Close = %v, markers = %+v, want a plain unavailable tool", err, got)
	}
}

func waitUntil(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !ready() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// blockingCloseTransport holds Close of its connection until release is closed.
type blockingCloseTransport struct {
	sdk.Transport
	release <-chan struct{}
}

func (transport blockingCloseTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := transport.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return blockingCloseConnection{Connection: conn, release: transport.release}, nil
}

type blockingCloseConnection struct {
	sdk.Connection
	release <-chan struct{}
}

func (conn blockingCloseConnection) Close() error {
	<-conn.release
	return conn.Connection.Close()
}

// The release of an evicted session runs in the background, with nothing else
// waiting for it: Close has to wait, or the session outlives the manager.
func TestManagerCloseWaitsForBackgroundRelease(t *testing.T) {
	m, key, cfg := newSeededManager(t, interruptServer)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseSession := func() { releaseOnce.Do(func() { close(release) }) }
	// Registered after the manager's own cleanup, so it runs before it.
	t.Cleanup(releaseSession)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	serverSession, err := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1"}, nil).Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server Connect: %v", err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil).
		Connect(ctx, blockingCloseTransport{Transport: clientTransport, release: release}, nil)
	if err != nil {
		t.Fatalf("client Connect: %v", err)
	}

	var entry *serverEntry
	seedEntry(m, key, cfg, func(e *serverEntry) {
		e.session = session
		entry = e
	})
	if err := entry.callError(ctx, ctx, fmt.Errorf("calling %q: %w", "tools/call", io.EOF)); !errors.Is(err, ErrConnectionClosed) {
		t.Fatalf("callError = %v, want ErrConnectionClosed", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case <-closed:
		t.Fatal("Close returned while the evicted session was still being released")
	case <-time.After(200 * time.Millisecond):
	}
	releaseSession()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return after the session was released")
	}
}

// A server that closes its output but keeps running ends the call with the end
// of the stream, and the evicted session still owns the process. Close has to
// wait until that session is released, and the process is gone by then.
func TestManagerCloseWaitsForEvictedSession(t *testing.T) {
	m, _ := newStdioManager(t, XReviewConfig{}, nil)
	set, err := m.Tools(context.Background(), []string{"mcp__local__pid", "mcp__local__hangup"})
	if err != nil {
		t.Fatalf("Tools: %v", err)
	}
	child, err := strconv.Atoi(pidOf(t, findTool(t, set.Tools, "mcp__local__pid")))
	if err != nil {
		t.Fatalf("server pid: %v", err)
	}
	_, err = findTool(t, set.Tools, "mcp__local__hangup").Call(context.Background(), json.RawMessage(`{}`))
	if !errors.Is(err, ErrConnectionClosed) || !markersOf(err).interrupted {
		t.Fatalf("hangup call error = %v, want an interrupted call", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Close did not return")
	}
	process, err := os.FindProcess(child)
	if err != nil {
		t.Fatalf("FindProcess: %v", err)
	}
	if err := process.Signal(syscall.Signal(0)); !errors.Is(err, os.ErrProcessDone) {
		_ = process.Kill()
		t.Fatalf("server process %d is still running after Close: %v", child, err)
	}
}

func TestCallFailureClassification(t *testing.T) {
	closedConnection := fmt.Errorf("%w: %q", ErrConnectionClosed, interruptServer)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	for _, tc := range []struct {
		name        string
		manager     *Manager
		ctx         context.Context
		err         error
		interrupted bool
	}{
		{"connection closed under a live caller", &Manager{}, context.Background(), closedConnection, true},
		{"caller is done", &Manager{}, canceled, closedConnection, false},
		{"manager is closed", &Manager{closed: true}, context.Background(), closedConnection, false},
		{"manager closed error in the chain", &Manager{}, context.Background(), errors.Join(ErrClosed, closedConnection), false},
		{"call failed", &Manager{}, context.Background(), fmt.Errorf("%w: %q: call timed out", ErrCallFailed, interruptServer), false},
		{"connect failed", &Manager{}, context.Background(), fmt.Errorf("%w: server %q", ErrConnectFailed, interruptServer), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.manager.callFailure(tc.ctx, tc.err)
			got := markersOf(err)
			if !got.unavailable || got.interrupted != tc.interrupted || got.hasFeedback != tc.interrupted {
				t.Fatalf("markers = %+v, want an unavailable tool, interrupted=%t", got, tc.interrupted)
			}
			if !errors.Is(err, tc.err) {
				t.Fatalf("classified error %v does not wrap %v", err, tc.err)
			}
		})
	}
}
