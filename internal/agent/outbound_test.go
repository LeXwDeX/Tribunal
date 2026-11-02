package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
	"github.com/LeXwDeX/tribunal/internal/workspace"
)

func TestOutboundStaleBetweenTurnsRejectsNextGenerate(t *testing.T) {
	ws := workspace.NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	client := &fakeClient{replies: []*llm.Response{{Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "continue"}}}, StopReason: llm.StopEndTurn}}}
	spec := testSpec(client)
	spec.Outbound = func(ctx context.Context, send func(context.Context) error) error {
		return ws.SendOutbound(ctx, token, send)
	}
	turn := 0
	spec.BeforeTurn = func(ctx context.Context, _ llm.Usage) error {
		turn++
		if turn == 2 {
			return ws.ChangeOutbound(ctx, func(context.Context) error { return nil })
		}
		return nil
	}
	result, err := Run(context.Background(), spec)
	if !errors.Is(err, workspace.ErrOutboundInvalidated) || len(client.requests()) != 1 || result == nil || result.Turns != 1 {
		t.Fatalf("run = %+v, %v, model requests=%d", result, err, len(client.requests()))
	}
}

func TestOutboundToolCancellationCannotAcceptSameTurnSubmit(t *testing.T) {
	ws := workspace.NewManager(t.TempDir(), nil).Get(42)
	token, err := ws.OutboundToken()
	if err != nil {
		t.Fatal(err)
	}
	started, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unblock)
	tool := echoTool{name: "remote", fn: func(ctx context.Context, _ json.RawMessage) (Result, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		<-release
		return Result{Content: "ignored cancellation"}, nil
	}}
	client := &fakeClient{replies: []*llm.Response{toolResponse(llm.StopToolUse, call("1", "remote", `{}`), call("2", SubmitToolName, `{"answer":"ok"}`))}}
	spec := testSpec(client, tool)
	spec.Outbound = func(ctx context.Context, send func(context.Context) error) error {
		return ws.SendOutbound(ctx, token, send)
	}
	done := make(chan error, 1)
	go func() { _, err := Run(context.Background(), spec); done <- err }()
	t.Cleanup(func() { unblock(); <-done })
	<-started
	changed := make(chan error, 1)
	go func() { changed <- ws.ChangeOutbound(context.Background(), func(context.Context) error { return nil }) }()
	select {
	case <-canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("tool was not canceled")
	}
	unblock()
	if err := <-changed; err != nil {
		t.Fatal(err)
	}
	err = <-done
	done <- err
	if !errors.Is(err, workspace.ErrOutboundInvalidated) || len(client.requests()) != 1 {
		t.Fatalf("run = %v, model requests=%d", err, len(client.requests()))
	}
}
