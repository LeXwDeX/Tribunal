package agent

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

func TestUsageIncompleteCustomClientNegativeRetainsKnownUsage(t *testing.T) {
	first := toolResponse(llm.StopToolUse, call("c", "echo", `{}`))
	first.Usage.InputTokens = 5
	second := toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
	second.Usage = llm.Usage{InputTokens: -3, OutputTokens: 2}
	client := &fakeClient{replies: []*llm.Response{first, second}}
	result, err := Run(context.Background(), testSpec(client, echoTool{name: "echo"}))
	if !errors.Is(err, ErrUnsafeResponse) || result == nil || result.Turns != 2 || result.Usage != (llm.Usage{InputTokens: 5, OutputTokens: 2, Incomplete: true}) || len(client.requests()) != 2 {
		t.Fatalf("result=%+v err=%v calls=%d", result, err, len(client.requests()))
	}
}

func TestUsageIncompleteForcedFallbackAndGuard(t *testing.T) {
	for _, guard := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "guard"}[guard], func(t *testing.T) {
			final := toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
			final.Usage.InputTokens = 2
			client := &fakeClient{errs: []error{&llm.Error{Kind: llm.ErrBadRequest, Usage: llm.Usage{InputTokens: math.MaxInt64 - 1, Incomplete: true}}}, replies: []*llm.Response{nil, final}}
			spec := testSpec(client)
			spec.MaxTurns = 1
			stopped := errors.New("stop")
			spec.BeforeTurn = func(_ context.Context, usage llm.Usage) error {
				if guard && usage.Incomplete {
					return stopped
				}
				return nil
			}
			result, err := Run(context.Background(), spec)
			want, calls := int64(math.MaxInt64), 2
			if guard {
				want, calls = math.MaxInt64-1, 1
			}
			if errors.Is(err, stopped) != guard || (!guard && err != nil) || result == nil || result.Usage.InputTokens != want || !result.Usage.Incomplete || len(client.requests()) != calls {
				t.Fatalf("result=%+v err=%v calls=%d", result, err, len(client.requests()))
			}
		})
	}
}

func TestUsageIncompleteZeroIsNotDropped(t *testing.T) {
	client := &fakeClient{errs: []error{&llm.Error{Kind: llm.ErrUnsafeResponse, Usage: llm.Usage{Incomplete: true}}}}
	result, err := Run(context.Background(), testSpec(client))
	if !errors.Is(err, ErrUnsafeResponse) || result == nil || result.Turns != 0 || !result.Usage.Incomplete {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

type dualUsageClient struct{}

func (dualUsageClient) Info() llm.ProviderInfo { return llm.ProviderInfo{Name: "dual"} }
func (dualUsageClient) Generate(context.Context, *llm.Request) (*llm.Response, error) {
	resp := toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
	resp.Usage = llm.Usage{InputTokens: 3, OutputTokens: 1}
	return resp, &llm.Error{Kind: llm.ErrUnsafeResponse, Usage: llm.Usage{InputTokens: 5, OutputTokens: -2}}
}

func TestUsageIncompleteResponseAndErrorUseKnownLowerBound(t *testing.T) {
	result, err := Run(context.Background(), testSpec(dualUsageClient{}))
	if !errors.Is(err, ErrUnsafeResponse) || result == nil || result.Turns != 1 || result.Output != nil || result.Usage != (llm.Usage{InputTokens: 5, OutputTokens: 1, Incomplete: true}) {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestUsageIncompleteResponseStopsBeforeNextModelRequest(t *testing.T) {
	for _, usage := range []llm.Usage{
		{Incomplete: true},
		{InputTokens: 5, Incomplete: true},
	} {
		first := toolResponse(llm.StopEndTurn)
		first.Usage = usage
		second := toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`))
		client := &fakeClient{replies: []*llm.Response{first, second}}
		spec := testSpec(client)
		spec.BeforeTurn = func(_ context.Context, used llm.Usage) error {
			if used.Incomplete {
				return &llm.Error{Kind: llm.ErrUnsafeResponse, Err: errors.New("unknown usage")}
			}
			return nil
		}
		result, err := Run(context.Background(), spec)
		if err == nil || result == nil || result.Usage != usage || len(client.requests()) != 1 {
			t.Fatalf("usage=%+v result=%+v err=%v calls=%d", usage, result, err, len(client.requests()))
		}
	}
}
