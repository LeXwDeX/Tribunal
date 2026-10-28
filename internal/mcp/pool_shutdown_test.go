package mcp

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var errTestClose = errors.New("private transport close detail")

type failingCloseTransport struct{ sdk.Transport }

func (transport failingCloseTransport) Connect(ctx context.Context) (sdk.Connection, error) {
	conn, err := transport.Transport.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return failingCloseConnection{Connection: conn}, nil
}

type failingCloseConnection struct{ sdk.Connection }

func (conn failingCloseConnection) Close() error {
	if err := conn.Connection.Close(); err != nil {
		return err
	}
	return errTestClose
}

type testRoundTripFunc func(*http.Request) (*http.Response, error)

func (fn testRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func connectedMemorySession(t *testing.T) (*sdk.ClientSession, *sdk.ServerSession, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	serverTransport, clientTransport := sdk.NewInMemoryTransports()
	server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1"}, nil)
	serverSession, err := server.Connect(ctx, serverTransport, nil)
	if err != nil {
		cancel()
		t.Fatalf("server Connect: %v", err)
	}
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
	session, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		cancel()
		_ = serverSession.Close()
		t.Fatalf("client Connect: %v", err)
	}
	return session, serverSession, cancel
}

func TestServerEntryCloseWaitsForReady(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, serverSession, cancel := connectedMemorySession(t)
		defer cancel()
		defer serverSession.Close()
		defer session.Close()

		// Model the handoff after Connect succeeds, before start publishes session.
		entry := &serverEntry{ready: make(chan struct{})}
		closed := make(chan struct{})
		var closeErr error
		go func() {
			closeErr = entry.close()
			close(closed)
		}()
		sessionDone := make(chan error, 1)
		go func() { sessionDone <- session.Wait() }()
		synctest.Wait()
		time.Sleep(6 * time.Second)
		synctest.Wait()
		select {
		case <-closed:
			t.Error("entry.close returned before ready")
		default:
		}

		entry.mu.Lock()
		entry.session = session
		entry.connected = true
		entry.mu.Unlock()
		close(entry.ready)
		const concurrentClosers = 8
		otherCloses := make(chan error, concurrentClosers)
		for range concurrentClosers {
			go func() { otherCloses <- entry.close() }()
		}
		synctest.Wait()
		select {
		case <-closed:
			if closeErr != nil {
				t.Errorf("entry.close: %v", closeErr)
			}
		default:
			t.Error("entry.close did not return after ready")
		}
		select {
		case err := <-sessionDone:
			if err != nil {
				t.Errorf("session.Wait: %v", err)
			}
		default:
			t.Error("session was not closed after ready")
		}
		for range concurrentClosers {
			select {
			case err := <-otherCloses:
				if err != nil {
					t.Errorf("concurrent entry.close: %v", err)
				}
			default:
				t.Error("concurrent entry.close did not return")
			}
		}
	})
}

func TestManagerCloseWaitsForPublishedSession(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		session, serverSession, cancelSession := connectedMemorySession(t)
		defer cancelSession()
		defer serverSession.Close()
		defer session.Close()

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &Manager{ctx: ctx, cancel: cancel, entries: make(map[entryKey]*serverEntry)}
		key := entryKey{server: "remote"}
		entry := &serverEntry{m: m, key: key, ready: make(chan struct{})}
		m.entries[key] = entry
		publish := make(chan struct{})
		published := false
		defer func() {
			if !published {
				close(publish)
			}
		}()

		// This publisher represents start after Connect has returned a session.
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			<-publish
			entry.mu.Lock()
			entry.session = session
			entry.connected = true
			entry.mu.Unlock()
			close(entry.ready)
			m.wg.Add(1)
			go entry.watch(session)
		}()
		closed := make(chan error, 1)
		go func() { closed <- m.Close() }()
		synctest.Wait()
		time.Sleep(6 * time.Second)
		synctest.Wait()
		select {
		case err := <-closed:
			t.Errorf("Manager.Close returned before publication: %v", err)
		default:
		}
		close(publish)
		published = true
		synctest.Wait()
		select {
		case err := <-closed:
			if err != nil {
				t.Errorf("Manager.Close: %v", err)
			}
		default:
			t.Error("Manager.Close did not return after publication")
		}
	})
}

func TestManagerCloseReportsSessionCloseError(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		serverTransport, clientTransport := sdk.NewInMemoryTransports()
		server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1"}, nil)
		serverSession, err := server.Connect(ctx, serverTransport, nil)
		if err != nil {
			t.Fatalf("server Connect: %v", err)
		}
		defer serverSession.Close()
		client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
		session, err := client.Connect(ctx, failingCloseTransport{Transport: clientTransport}, nil)
		if err != nil {
			t.Fatalf("client Connect: %v", err)
		}
		defer session.Close()

		managerCtx, managerCancel := context.WithCancel(context.Background())
		defer managerCancel()
		m := &Manager{ctx: managerCtx, cancel: managerCancel, entries: make(map[entryKey]*serverEntry)}
		key := entryKey{server: "broken"}
		entry := &serverEntry{m: m, key: key, ready: make(chan struct{}), session: session, connected: true}
		m.entries[key] = entry
		close(entry.ready)
		closed := make(chan error, 1)
		go func() { closed <- m.Close() }()
		synctest.Wait()
		select {
		case err := <-closed:
			if err == nil || err.Error() != "mcp: close failed for server(s): broken" {
				t.Errorf("Manager.Close = %v, want sanitized close failure", err)
			}
			if err := entry.close(); !errors.Is(err, errTestClose) {
				t.Errorf("repeated entry.close = %v, want original close error", err)
			}
		default:
			t.Error("Manager.Close did not return for an already published session")
		}
	})
}

func TestManagerConcurrentCloseWaitsAndSharesResult(t *testing.T) {
	for _, tc := range []struct {
		name    string
		failing bool
		want    string
	}{
		{name: "success"},
		{name: "failure", failing: true, want: "mcp: close failed for server(s): alpha, zulu"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				defer cancel()
				managerCtx, managerCancel := context.WithCancel(context.Background())
				defer managerCancel()
				m := &Manager{
					ctx: managerCtx, cancel: managerCancel,
					entries: make(map[entryKey]*serverEntry),
					servers: map[string]ServerConfig{"alpha": {}},
				}
				var pending *serverEntry
				var pendingSession *sdk.ClientSession
				sessionDone := make(chan struct{}, 3)
				for _, name := range []string{"zulu", "good", "alpha"} {
					serverTransport, clientTransport := sdk.NewInMemoryTransports()
					server := sdk.NewServer(&sdk.Implementation{Name: "test-server", Version: "1"}, nil)
					serverSession, err := server.Connect(ctx, serverTransport, nil)
					if err != nil {
						t.Fatalf("server Connect: %v", err)
					}
					defer serverSession.Close()
					var transport sdk.Transport = clientTransport
					if tc.failing && name != "good" {
						transport = failingCloseTransport{Transport: transport}
					}
					client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1"}, nil)
					session, err := client.Connect(ctx, transport, nil)
					if err != nil {
						t.Fatalf("client Connect: %v", err)
					}
					defer session.Close()
					go func() { _ = session.Wait(); sessionDone <- struct{}{} }()
					key := entryKey{server: name}
					entry := &serverEntry{m: m, key: key, ready: make(chan struct{})}
					m.entries[key] = entry
					if name == "alpha" {
						pending, pendingSession = entry, session
					} else {
						entry.session = session
						entry.connected = true
						close(entry.ready)
					}
				}

				worker := make(chan struct{})
				m.wg.Add(1)
				go func() { <-worker; m.wg.Done() }()
				const closers = 4
				results := make([]chan error, closers)
				for i := range results {
					results[i] = make(chan error, 1)
				}
				go func() { results[0] <- m.Close() }()
				synctest.Wait()
				if len(results[0]) != 0 {
					t.Error("first Close returned before entry.ready")
				}
				if _, err := m.ListTools(ctx, "alpha"); !errors.Is(err, ErrClosed) {
					t.Errorf("ListTools during Close = %v, want ErrClosed", err)
				}
				for i := 1; i < closers; i++ {
					go func() { results[i] <- m.Close() }()
				}
				synctest.Wait()
				for i, result := range results {
					if len(result) != 0 {
						t.Errorf("Close %d returned before entry.ready", i)
					}
				}

				pending.mu.Lock()
				pending.session = pendingSession
				pending.connected = true
				pending.mu.Unlock()
				close(pending.ready)
				synctest.Wait()
				if len(sessionDone) != 3 {
					t.Errorf("closed sessions = %d, want 3", len(sessionDone))
				}
				for i, result := range results {
					if len(result) != 0 {
						t.Errorf("Close %d returned before manager waitgroup", i)
					}
				}
				close(worker)
				synctest.Wait()
				check := func(label string, err error) {
					t.Helper()
					if tc.want == "" {
						if err != nil {
							t.Errorf("%s = %v, want nil", label, err)
						}
					} else if err == nil || err.Error() != tc.want || errors.Is(err, errTestClose) {
						t.Errorf("%s = %v, want sanitized error %q", label, err, tc.want)
					}
				}
				for i, result := range results {
					select {
					case err := <-result:
						check("concurrent Close", err)
					default:
						t.Errorf("Close %d did not return after release", i)
					}
				}
				check("later Close", m.Close())
				check("repeated Close", m.Close())
			})
		})
	}
}

func TestManagerCloseAfterConnectFailure(t *testing.T) {
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project: Config{MCPServers: map[string]ServerConfig{
			"missing": {Command: "/definitely/not/an/mcp-command"},
		}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.cancel()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := m.ListTools(ctx, "missing"); err == nil {
		t.Error("ListTools succeeded for a missing command")
	}
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close after connect failure: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Error("Close did not return after connect failure")
	}
	if _, err := m.ListTools(ctx, "missing"); !errors.Is(err, ErrClosed) {
		t.Errorf("ListTools after Close = %v, want ErrClosed", err)
	}
}

func TestManagerCloseCancelsConnectingSession(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var firstRequest atomic.Bool
	transport := testRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !firstRequest.CompareAndSwap(false, true) {
			return nil, errors.New("test connection canceled")
		}
		started <- struct{}{}
		select {
		case <-req.Context().Done():
			return nil, req.Context().Err()
		case <-release:
			return nil, errors.New("test transport released")
		}
	})
	m, err := New(Options{
		Workspace: t.TempDir(),
		Project: Config{MCPServers: map[string]ServerConfig{
			"remote": {URL: "http://127.0.0.1:12345/mcp"},
		}},
		HTTPClient: &http.Client{Transport: transport},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer m.cancel()
	callCtx, cancelCall := context.WithCancel(context.Background())
	defer cancelCall()
	callDone := make(chan error, 1)
	go func() {
		_, err := m.ListTools(callCtx, "remote")
		callDone <- err
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Error("connect request did not start")
		return
	}
	closed := make(chan error, 1)
	go func() { closed <- m.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Errorf("Close during connect: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("Close did not return after canceling connect")
		unblock()
		cancelCall()
	}
	select {
	case err := <-callDone:
		if err == nil {
			t.Error("ListTools succeeded after connect was canceled")
		}
	case <-time.After(10 * time.Second):
		t.Error("ListTools did not return after connect was canceled")
	}
}
