package workspace

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type observedOutboundContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *observedOutboundContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func TestOutboundChangeCancelsAndDrainsBeforeWrite(t *testing.T) {
	manager := NewManager(t.TempDir(), nil)
	ws := manager.Get(42)
	other := manager.Get(43)
	old, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	canceled := make(chan struct{})
	release := make(chan struct{})
	releaseSend := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseSend)
	sendDone := make(chan error, 1)
	go func() {
		sendDone <- ws.SendOutbound(context.Background(), old, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil // A transport may ignore cancellation and report success.
		})
	}()
	<-started
	wrote := make(chan struct{})
	changeDone := make(chan error, 1)
	go func() {
		changeDone <- ws.ChangeOutbound(context.Background(), func(context.Context) error {
			close(wrote)
			return nil
		})
	}()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("outbound call was not canceled")
	}
	if _, err := ws.OutboundToken(); !errors.Is(err, ErrOutboundInvalidated) {
		t.Fatalf("token during change = %v", err)
	}
	called := false
	if err := ws.SendOutbound(context.Background(), old, func(context.Context) error { called = true; return nil }); !errors.Is(err, ErrOutboundInvalidated) || called {
		t.Fatalf("old send during change = %v, called=%t", err, called)
	}
	otherToken, err := other.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := other.SendOutbound(context.Background(), otherToken, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("other project blocked: %v", err)
	}
	select {
	case <-wrote:
		t.Fatal("policy write ran before the transport returned")
	default:
	}
	releaseSend()
	if err := <-sendDone; !errors.Is(err, ErrOutboundInvalidated) {
		t.Fatalf("canceled transport reported success: %v", err)
	}
	if err := <-changeDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-wrote:
	default:
		t.Fatal("policy write was not invoked")
	}
	if err := ws.SendOutbound(context.Background(), old, func(context.Context) error { return nil }); !errors.Is(err, ErrOutboundInvalidated) {
		t.Fatalf("old token after change = %v", err)
	}
	current, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.SendOutbound(context.Background(), current, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := other.SendOutbound(context.Background(), current, func(context.Context) error { return nil }); !errors.Is(err, ErrOutboundInvalidated) {
		t.Fatalf("cross-project token = %v", err)
	}
}

func TestOutboundChangeTimesOutWithoutWritingWhenSendIgnoresCancellation(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	releaseSend := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseSend)
	done := make(chan error, 1)
	go func() {
		done <- ws.SendOutbound(context.Background(), token, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	wrote := false
	err = ws.ChangeOutbound(ctx, func(context.Context) error { wrote = true; return nil })
	if !errors.Is(err, context.DeadlineExceeded) || wrote {
		t.Fatalf("timed-out policy write = %v, wrote=%t", err, wrote)
	}
	if err := ws.SendOutbound(context.Background(), token, func(context.Context) error { return nil }); !errors.Is(err, ErrOutboundInvalidated) {
		t.Fatalf("old token after failed change = %v", err)
	}
	releaseSend()
	if err := <-done; !errors.Is(err, ErrOutboundInvalidated) {
		t.Fatalf("canceled transport reported success: %v", err)
	}
}

func TestOutboundChangeHasBoundWithoutCallerDeadline(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	sendDone := make(chan struct{})
	t.Cleanup(func() { close(release); <-sendDone })
	go func() {
		defer close(sendDone)
		_ = ws.SendOutbound(context.Background(), token, func(context.Context) error {
			close(started)
			<-release
			return nil
		})
	}()
	<-started
	wrote := make(chan struct{}, 1)
	done := make(chan error, 1)
	go func() {
		done <- ws.ChangeOutbound(context.Background(), func(context.Context) error { wrote <- struct{}{}; return nil })
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("unbounded policy change = %v", err)
		}
	case <-time.After(outboundChangeWait + 2*time.Second):
		t.Fatal("policy change did not time out")
	}
	select {
	case <-wrote:
		t.Fatal("timed-out policy change wrote state")
	default:
	}
}

func TestOutboundChangeDoesNotTakeWorkspaceContentLock(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	unlock, err := ws.RLock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ws.ChangeOutbound(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("gate depended on content lock: %v", err)
	}
}

func TestOutboundConcurrentAndCanceledChangesDoNotWrite(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseWrite := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseWrite)
	done := make(chan error, 1)
	go func() {
		done <- ws.ChangeOutbound(context.Background(), func(context.Context) error {
			close(entered)
			<-release
			return nil
		})
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	called := false
	if err := ws.ChangeOutbound(ctx, func(context.Context) error { called = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatalf("canceled competing change = %v, called=%t", err, called)
	}
	if _, err := ws.OutboundToken(); !errors.Is(err, ErrOutboundInvalidated) {
		t.Fatalf("token during change = %v", err)
	}
	releaseWrite()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestOutboundSendHonorsParentCancellation(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- ws.SendOutbound(ctx, token, func(ctx context.Context) error {
			close(started)
			<-ctx.Done()
			return nil
		})
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled send = %v", err)
	}
	if err := ws.SendOutbound(context.Background(), token, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("parent cancellation invalidated other sends: %v", err)
	}
}

func TestOutboundSendCanceledBeforeAdmissionDoesNotCallTransport(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	parent, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &observedOutboundContext{Context: parent, checked: make(chan struct{})}
	ws.outbound.mu.Lock()
	done := make(chan error, 1)
	called := make(chan struct{}, 1)
	go func() {
		done <- ws.SendOutbound(ctx, token, func(context.Context) error {
			called <- struct{}{}
			return nil
		})
	}()
	<-ctx.checked
	cancel()
	ws.outbound.mu.Unlock()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled send = %v", err)
	}
	select {
	case <-called:
		t.Fatal("transport called after cancellation before admission")
	default:
	}
}

func TestOutboundChangeBeforeCanceledContextDoesNotInvalidate(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := ws.ChangeOutbound(ctx, func(context.Context) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("canceled change = %v, called=%t", err, called)
	}
	if err := ws.SendOutbound(context.Background(), token, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("unstarted change invalidated token: %v", err)
	}
}

func TestOutboundLeaseReleasedAfterPanic(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("transport panic did not propagate")
			}
		}()
		_ = ws.SendOutbound(context.Background(), token, func(context.Context) error { panic("transport") })
	}()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := ws.ChangeOutbound(ctx, func(context.Context) error { return nil }); err != nil {
		t.Fatalf("panic left a lease active: %v", err)
	}
}

func TestOutboundSimultaneousLeasesRemainDistinct(t *testing.T) {
	ws := NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	const sends = 3
	started := make(chan struct{}, sends)
	canceled := make(chan struct{}, sends)
	release := make(chan struct{})
	done := make(chan error, sends)
	for i := 0; i < sends; i++ {
		go func() {
			done <- ws.SendOutbound(context.Background(), token, func(ctx context.Context) error {
				started <- struct{}{}
				<-ctx.Done()
				canceled <- struct{}{}
				<-release
				return nil
			})
		}()
	}
	for i := 0; i < sends; i++ {
		<-started
	}
	ws.outbound.mu.Lock()
	active := len(ws.outbound.active)
	identities := make(map[uint64]bool, active)
	for lease := range ws.outbound.active {
		identities[lease.id] = true
	}
	ws.outbound.mu.Unlock()
	if active != sends || len(identities) != sends {
		t.Fatalf("active leases=%d distinct identities=%d", active, len(identities))
	}
	changeDone := make(chan error, 1)
	go func() {
		changeDone <- ws.ChangeOutbound(context.Background(), func(context.Context) error { return nil })
	}()
	for i := 0; i < sends; i++ {
		select {
		case <-canceled:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d simultaneous leases were canceled", i, sends)
		}
	}
	close(release)
	for i := 0; i < sends; i++ {
		if err := <-done; !errors.Is(err, ErrOutboundInvalidated) {
			t.Fatalf("send %d = %v", i, err)
		}
	}
	if err := <-changeDone; err != nil {
		t.Fatal(err)
	}
}
