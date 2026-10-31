package app

import (
	"context"

	"github.com/LeXwDeX/tribunal/internal/workspace"
)

type outboundKey struct{}

type outboundBinding struct {
	workspace *workspace.Workspace
	token     workspace.OutboundToken
}

// bindOutbound never replaces a token from an earlier snapshot.
func bindOutbound(ctx context.Context, ws *workspace.Workspace) (context.Context, error) {
	if binding, ok := ctx.Value(outboundKey{}).(outboundBinding); ok {
		if binding.workspace != ws || ws == nil {
			return nil, workspace.ErrOutboundInvalidated
		}
		return ctx, nil
	}
	if ws == nil {
		return nil, workspace.ErrOutboundInvalidated
	}
	token, err := ws.OutboundToken()
	if err != nil {
		return nil, err
	}
	return context.WithValue(ctx, outboundKey{}, outboundBinding{workspace: ws, token: token}), nil
}

func sendOutbound(ctx context.Context, ws *workspace.Workspace, send func(context.Context) error) error {
	binding, ok := ctx.Value(outboundKey{}).(outboundBinding)
	if !ok || ws == nil || binding.workspace != ws {
		return workspace.ErrOutboundInvalidated
	}
	return ws.SendOutbound(ctx, binding.token, send)
}

func outboundSender(ws *workspace.Workspace) func(context.Context, func(context.Context) error) error {
	return func(ctx context.Context, send func(context.Context) error) error {
		return sendOutbound(ctx, ws, send)
	}
}
