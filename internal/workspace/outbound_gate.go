package workspace

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrOutboundInvalidated means the policy or the identity of a send changed.
var ErrOutboundInvalidated = errors.New("workspace: outbound identity invalidated")

const outboundChangeWait = 5 * time.Second

type outboundEpoch struct{ id uint64 }
type outboundLease struct{ id uint64 }

// This process-local gate fences policy writes and bound sends. Runtime and
// routes must stay disabled until durable provenance is checked after recovery.
type outboundGate struct {
	mu       sync.Mutex
	epoch    *outboundEpoch
	changing bool
	changed  chan struct{}
	active   map[*outboundLease]context.CancelCauseFunc
	writer   chan struct{}
	nextID   uint64
}

func (g *outboundGate) init() {
	if g.changed == nil {
		g.changed = make(chan struct{})
		g.nextID++
		g.epoch = &outboundEpoch{id: g.nextID}
		g.active = make(map[*outboundLease]context.CancelCauseFunc)
		g.writer = make(chan struct{}, 1)
	}
}

func (g *outboundGate) notify() {
	close(g.changed)
	g.changed = make(chan struct{})
}

// OutboundToken identifies one project policy epoch. It is not durable
// provenance. Capture it before reading policy and do not renew it for output
// derived from an older snapshot.
type OutboundToken struct {
	gate  *outboundGate
	epoch *outboundEpoch
}

// OutboundToken returns a token for collecting a new policy-bound snapshot.
// It fails rather than waits during a change, so a caller holding a workspace
// content lock cannot deadlock a policy writer.
func (w *Workspace) OutboundToken() (OutboundToken, error) {
	if w == nil || w.ProjectID <= 0 {
		return OutboundToken{}, ErrOutboundInvalidated
	}
	g := &w.outbound
	g.mu.Lock()
	defer g.mu.Unlock()
	g.init()
	if g.changing {
		return OutboundToken{}, ErrOutboundInvalidated
	}
	return OutboundToken{gate: g, epoch: g.epoch}, nil
}

// SendOutbound holds a lease until send returns, even if send ignores context
// cancellation. Writers cannot commit while a send is still in progress.
func (w *Workspace) SendOutbound(ctx context.Context, token OutboundToken, send func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || w.ProjectID <= 0 || send == nil || token.gate != &w.outbound || token.epoch == nil {
		return ErrOutboundInvalidated
	}
	g := &w.outbound
	g.mu.Lock()
	g.init()
	if err := ctx.Err(); err != nil {
		g.mu.Unlock()
		return err
	}
	if g.changing || g.epoch.id != token.epoch.id {
		g.mu.Unlock()
		return ErrOutboundInvalidated
	}
	callCtx, cancel := context.WithCancelCause(ctx)
	g.nextID++
	lease := &outboundLease{id: g.nextID}
	g.active[lease] = cancel
	g.mu.Unlock()
	defer func() {
		cancel(nil)
		g.mu.Lock()
		delete(g.active, lease)
		g.notify()
		g.mu.Unlock()
	}()
	err := send(callCtx)
	if cause := context.Cause(callCtx); cause != nil {
		return errors.Join(err, cause)
	}
	return err
}

// ChangeOutbound cancels and drains active sends before running write. The
// caller must not acquire a workspace content lock from write. A failed or
// timed-out change still invalidates previously issued tokens. It cannot undo
// data sent before cancellation or writes accepted by a remote service.
func (w *Workspace) ChangeOutbound(ctx context.Context, write func(context.Context) error) error {
	return w.ChangeOutboundIf(ctx, nil, write)
}

// ChangeOutboundIf serializes the check with all other gate writers. A false
// check leaves active sends and existing tokens untouched.
func (w *Workspace) ChangeOutboundIf(ctx context.Context, check func(context.Context) (bool, error), write func(context.Context) error) error {
	if w == nil || w.ProjectID <= 0 || write == nil {
		return ErrOutboundInvalidated
	}
	waitCtx, cancel := context.WithTimeout(ctx, outboundChangeWait)
	defer cancel()
	g := &w.outbound
	g.mu.Lock()
	g.init()
	writer := g.writer
	g.mu.Unlock()
	select {
	case writer <- struct{}{}:
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
	defer func() { <-writer }()
	if err := waitCtx.Err(); err != nil {
		return err
	}
	if check != nil {
		change, err := check(waitCtx)
		if err != nil {
			return err
		}
		if err := waitCtx.Err(); err != nil {
			return err
		}
		if !change {
			return nil
		}
	}
	for {
		if err := waitCtx.Err(); err != nil {
			return err
		}
		g.mu.Lock()
		g.init()
		if err := waitCtx.Err(); err != nil {
			g.mu.Unlock()
			return err
		}
		if !g.changing {
			g.changing = true
			g.nextID++
			g.epoch = &outboundEpoch{id: g.nextID}
			for _, stop := range g.active {
				stop(ErrOutboundInvalidated)
			}
			g.notify()
			g.mu.Unlock()
			break
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-changed:
		}
	}
	defer func() {
		g.mu.Lock()
		g.changing = false
		g.notify()
		g.mu.Unlock()
	}()
	for {
		if err := waitCtx.Err(); err != nil {
			return err
		}
		g.mu.Lock()
		if len(g.active) == 0 {
			g.mu.Unlock()
			if err := waitCtx.Err(); err != nil {
				return err
			}
			return write(ctx)
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-changed:
		}
	}
}
