package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/llm"
)

type fakeClient struct {
	mu      sync.Mutex
	replies []*llm.Response
	errs    []error
	reqs    []*llm.Request
	metas   []llm.CallMeta
}

func (f *fakeClient) Generate(ctx context.Context, req *llm.Request) (*llm.Response, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	f.metas = append(f.metas, llm.CallMetaFromContext(ctx))
	index := len(f.reqs) - 1
	var reply *llm.Response
	var err error
	if index < len(f.replies) {
		reply = f.replies[index]
	}
	if index < len(f.errs) {
		err = f.errs[index]
	}
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if reply == nil {
		return nil, errors.New("fake: no reply")
	}
	return reply, nil
}

func (f *fakeClient) Info() llm.ProviderInfo { return llm.ProviderInfo{Name: "fake"} }

func (f *fakeClient) requests() []*llm.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*llm.Request(nil), f.reqs...)
}

func (f *fakeClient) callMetas() []llm.CallMeta {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]llm.CallMeta(nil), f.metas...)
}

type echoTool struct {
	name string
	fn   func(ctx context.Context, input json.RawMessage) (Result, error)
}

func (t echoTool) Def() llm.ToolDef {
	return llm.ToolDef{Name: t.name, Description: "test tool",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"value":{"type":"string"}}}`)}
}

func (t echoTool) Call(ctx context.Context, input json.RawMessage) (Result, error) {
	if t.fn != nil {
		return t.fn(ctx, input)
	}
	return Result{Content: string(input)}, nil
}

func assistant(calls ...*llm.ToolCall) llm.Message {
	msg := llm.Message{Role: llm.RoleAssistant}
	for _, call := range calls {
		copy := *call
		msg.Parts = append(msg.Parts, llm.Part{Type: llm.PartToolCall, ToolCall: &copy})
	}
	return msg
}

func call(id, name, input string) *llm.ToolCall {
	return &llm.ToolCall{ID: id, Name: name, Input: json.RawMessage(input)}
}

func toolResponse(stop llm.StopReason, calls ...*llm.ToolCall) *llm.Response {
	return &llm.Response{Message: assistant(calls...), StopReason: stop}
}

func decodeObject(raw json.RawMessage) (any, error) {
	var out map[string]any
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func testSpec(client llm.Client, tools ...Tool) RunSpec {
	return RunSpec{
		Client: client,
		Prompt: "review this",
		Tools:  tools,
		Submit: SubmitSpec{Schema: json.RawMessage(`{"type":"object"}`), Decode: decodeObject},
	}
}

func TestRunExecutesParallelToolsAndPreservesReasoning(t *testing.T) {
	reasoning := llm.Part{Type: llm.PartReasoning, Reasoning: &llm.Reasoning{
		Protocol: "anthropic",
		Raw:      json.RawMessage(`{"type":"thinking","thinking":"check","signature":"signed"}`),
	}}
	first := *toolResponse(llm.StopToolUse,
		call("c1", "echo", `{"value":"a"}`),
		call("c2", "missing", `{}`),
		call("c3", "echo", `{"value":"c"}`),
	)
	first.Message.Parts = append([]llm.Part{reasoning}, first.Message.Parts...)
	second := toolResponse(llm.StopToolUse, call("c4", SubmitToolName, `{"answer":"ok"}`))
	second.Usage = llm.Usage{InputTokens: 10, OutputTokens: 4, ReasoningTokens: 2}

	client := &fakeClient{replies: []*llm.Response{&first, second}}
	result, err := Run(context.Background(), testSpec(client, echoTool{name: "echo"}, echoTool{name: "echo2", fn: func(context.Context, json.RawMessage) (Result, error) {
		return Result{Content: "second"}, nil
	}}))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Turns != 2 {
		t.Fatalf("turns = %d, want 2", result.Turns)
	}
	got, ok := result.Output.(map[string]any)
	if !ok || got["answer"] != "ok" {
		t.Fatalf("output = %#v", result.Output)
	}
	if result.Usage.InputTokens != 10 || result.Usage.OutputTokens != 4 || result.Usage.ReasoningTokens != 2 {
		t.Fatalf("usage = %#v", result.Usage)
	}
	if len(result.Transcript) != 5 {
		t.Fatalf("transcript length = %d, want 5", len(result.Transcript))
	}
	raw := string(result.Transcript[1].Parts[0].Reasoning.Raw)
	if raw != `{"type":"thinking","thinking":"check","signature":"signed"}` {
		t.Fatalf("reasoning raw = %s", raw)
	}
	toolMsg := result.Transcript[2]
	if toolMsg.Role != llm.RoleTool || len(toolMsg.Parts) != 3 {
		t.Fatalf("tool message = %#v", toolMsg)
	}
	ids := []string{}
	for _, part := range toolMsg.Parts {
		if part.Type != llm.PartToolResult || part.ToolResult == nil {
			t.Fatalf("bad tool result part %#v", part)
		}
		ids = append(ids, part.ToolResult.CallID)
	}
	if strings.Join(ids, ",") != "c1,c2,c3" {
		t.Fatalf("tool result ids = %v", ids)
	}
	if !toolMsg.Parts[1].ToolResult.IsError {
		t.Fatalf("unknown tool result must be an error: %#v", toolMsg.Parts[1])
	}
	if !strings.Contains(toolMsg.Parts[0].ToolResult.Content, "nonce=") ||
		!strings.Contains(toolMsg.Parts[0].ToolResult.Content, "Untrusted tool output") {
		t.Fatalf("tool result lacks untrusted wrapper: %q", toolMsg.Parts[0].ToolResult.Content)
	}

	reqs := client.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	names := make([]string, 0, len(reqs[0].Tools))
	for _, def := range reqs[0].Tools {
		names = append(names, def.Name)
	}
	if strings.Join(names, ",") != "echo,echo2,submit_result" {
		t.Fatalf("tool names = %v", names)
	}
	if reqs[1].ToolChoice != "" {
		t.Fatalf("tool choice = %q on a non-final turn", reqs[1].ToolChoice)
	}
}

func TestRunSubmitValidationRetries(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{"answer":""}`)),
		toolResponse(llm.StopToolUse, call("s2", SubmitToolName, `{"answer":""}`)),
		toolResponse(llm.StopToolUse, call("s3", SubmitToolName, `{"answer":"ok"}`)),
	}}
	spec := testSpec(client)
	spec.Submit.Decode = func(raw json.RawMessage) (any, error) {
		var in struct {
			Answer string `json:"answer"`
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			return nil, err
		}
		if in.Answer == "" {
			return nil, errors.New("answer is required")
		}
		return in, nil
	}
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Turns != 3 {
		t.Fatalf("turns = %d, want 3", result.Turns)
	}
	for _, turn := range []int{1, 2} {
		part := result.Transcript[turn*2].Parts[0]
		if !part.ToolResult.IsError || !strings.Contains(part.ToolResult.Content, "answer is required") {
			t.Fatalf("turn %d feedback = %#v", turn, part.ToolResult)
		}
	}
}

func TestRunStopsAfterSubmitRetryBudget(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{"bad":1}`)),
		toolResponse(llm.StopToolUse, call("s2", SubmitToolName, `{"bad":2}`)),
		toolResponse(llm.StopToolUse, call("s3", SubmitToolName, `{"bad":3}`)),
	}}
	spec := testSpec(client)
	spec.Submit.Decode = func(json.RawMessage) (any, error) { return nil, errors.New("nope") }
	_, err := Run(context.Background(), spec)
	if !errors.Is(err, ErrInvalidSubmit) {
		t.Fatalf("error = %v, want ErrInvalidSubmit", err)
	}
	if got := len(client.requests()); got != 3 {
		t.Fatalf("requests = %d, want 3", got)
	}
}

func TestRunAsksForSubmitOnce(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		{Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "done"}}}, StopReason: llm.StopEndTurn},
		{Message: llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "still done"}}}, StopReason: llm.StopEndTurn},
	}}
	_, err := Run(context.Background(), testSpec(client))
	if !errors.Is(err, ErrNoSubmit) {
		t.Fatalf("error = %v, want ErrNoSubmit", err)
	}
	reqs := client.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want 2", len(reqs))
	}
	if got := reqs[1].Messages[len(reqs[1].Messages)-1].Parts[0].Text; !strings.Contains(got, SubmitToolName) {
		t.Fatalf("reminder = %q", got)
	}
}

func TestRunForcesSubmitOnLastTurn(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{"ok":true}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo"})
	spec.MaxTurns = 2
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Turns != 2 {
		t.Fatalf("turns = %d", result.Turns)
	}
	reqs := client.requests()
	if reqs[1].ToolChoice != SubmitToolName {
		t.Fatalf("tool choice = %q, want %q", reqs[1].ToolChoice, SubmitToolName)
	}
}

func TestRunFallsBackWhenForcedSubmitIsRejected(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		nil,
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{"ok":true}`)),
	}, errs: []error{
		&llm.Error{Kind: llm.ErrBadRequest, Status: 400},
	}}
	spec := testSpec(client)
	spec.MaxTurns = 1
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Turns != 1 {
		t.Fatalf("turns = %d, want 1", result.Turns)
	}
	requests := client.requests()
	if len(requests) != 2 {
		t.Fatalf("requests = %d, want forced request plus fallback", len(requests))
	}
	if requests[0].ToolChoice != SubmitToolName || requests[1].ToolChoice != "" {
		t.Fatalf("tool choices = %q, %q", requests[0].ToolChoice, requests[1].ToolChoice)
	}
}

func TestRunClampsTinyToolOutputLimit(t *testing.T) {
	long := strings.Repeat("x", 5000)
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo", fn: func(context.Context, json.RawMessage) (Result, error) {
		return Result{Content: long}, nil
	}})
	spec.MaxToolOutput = 1
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	content := result.Transcript[2].Parts[0].ToolResult.Content
	if runeLen(content) > minToolOutputChars {
		t.Fatalf("content length = %d, want <= %d", runeLen(content), minToolOutputChars)
	}
	if !strings.Contains(content, "[truncated: ") {
		t.Fatalf("missing truncation marker: %q", content)
	}
}

func TestRunTruncatesAndWrapsToolOutput(t *testing.T) {
	long := strings.Repeat("x", 5000)
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo", fn: func(context.Context, json.RawMessage) (Result, error) {
		return Result{Content: long}, nil
	}})
	spec.MaxToolOutput = 300
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	content := result.Transcript[2].Parts[0].ToolResult.Content
	if !strings.Contains(content, "[truncated: ") || !strings.Contains(content, "chars omitted]") {
		t.Fatalf("missing truncation marker: %q", content)
	}
	if !strings.Contains(content, "nonce=") {
		t.Fatalf("missing nonce wrapper: %q", content)
	}
	if strings.Contains(content, strings.Repeat("x", 301)) {
		t.Fatalf("content was not bounded: %d bytes", len(content))
	}
	if len([]rune(content)) > 600 {
		t.Fatalf("wrapped content too large: %d runes", len([]rune(content)))
	}
}

func TestRunToolErrorBecomesResult(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo", fn: func(context.Context, json.RawMessage) (Result, error) {
		return Result{}, errors.New("boom")
	}})
	result, err := Run(context.Background(), spec)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	part := result.Transcript[2].Parts[0].ToolResult
	if !part.IsError || !strings.Contains(part.Content, "boom") {
		t.Fatalf("tool error result = %#v", part)
	}
}

func TestRunCancellationDoesNotCallGenerateAgain(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo", fn: func(context.Context, json.RawMessage) (Result, error) {
		cancel()
		return Result{}, context.Canceled
	}})
	_, err := Run(ctx, spec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if got := len(client.requests()); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestRunRejectsDuplicateToolNames(t *testing.T) {
	client := &fakeClient{}
	spec := testSpec(client, echoTool{name: "echo"}, echoTool{name: "echo"})
	_, err := Run(context.Background(), spec)
	if err == nil || !strings.Contains(err.Error(), "duplicate tool") {
		t.Fatalf("error = %v", err)
	}
	if got := len(client.requests()); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestRunCallMetaKeepsWorkflowAndSetsTurn(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{}`)),
	}}
	ctx := llm.WithCallMeta(context.Background(), llm.CallMeta{WorkflowID: "wf", NodeID: "n1"})
	if _, err := Run(ctx, testSpec(client, echoTool{name: "echo"})); err != nil {
		t.Fatalf("Run: %v", err)
	}
	metas := client.callMetas()
	if len(metas) != 2 || metas[0].Turn != 1 || metas[1].Turn != 2 {
		t.Fatalf("metas = %#v", metas)
	}
	for _, meta := range metas {
		if meta.WorkflowID != "wf" || meta.NodeID != "n1" {
			t.Fatalf("meta lost workflow context: %#v", meta)
		}
	}
}

func TestRunRejectsDuplicateCallIDs(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse,
			call("same", "echo", `{}`),
			call("same", SubmitToolName, `{}`)),
	}}
	_, err := Run(context.Background(), testSpec(client, echoTool{name: "echo"}))
	if err == nil || !strings.Contains(err.Error(), "duplicate tool call id") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunGenerateErrorIsReturned(t *testing.T) {
	client := &fakeClient{errs: []error{fmt.Errorf("provider down")}}
	_, err := Run(context.Background(), testSpec(client))
	if err == nil || !strings.Contains(err.Error(), "provider down") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunDuplicateSubmitOnlyFirstIsApplied(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse,
			call("s1", SubmitToolName, `{"answer":"first"}`),
			call("s2", SubmitToolName, `{"answer":"second"}`)),
	}}
	result, err := Run(context.Background(), testSpec(client))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	parts := result.Transcript[2].Parts
	if len(parts) != 2 {
		t.Fatalf("results = %d, want 2", len(parts))
	}
	if parts[0].ToolResult.IsError {
		t.Fatalf("first submit rejected: %#v", parts[0].ToolResult)
	}
	if !parts[1].ToolResult.IsError || !strings.Contains(parts[1].ToolResult.Content, "already accepted") {
		t.Fatalf("second submit = %#v", parts[1].ToolResult)
	}
	if got := result.Output.(map[string]any)["answer"]; got != "first" {
		t.Fatalf("output = %v", result.Output)
	}
}

func TestRunReservedToolNameAndInvalidSchema(t *testing.T) {
	client := &fakeClient{}
	if _, err := Run(context.Background(), testSpec(client, echoTool{name: SubmitToolName})); err == nil {
		t.Fatal("reserved tool name accepted")
	}
	spec := testSpec(client)
	spec.Submit.Schema = json.RawMessage(`{`)
	if _, err := Run(context.Background(), spec); err == nil {
		t.Fatal("invalid submit schema accepted")
	}
}

func TestRunPassesSamplingAndLimits(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{}`)),
	}}
	temp := 0.2
	spec := testSpec(client)
	spec.MaxTokens = 1234
	spec.Temperature = &temp
	spec.Reasoning = &llm.ReasoningOpts{Effort: "high"}
	if _, err := Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}
	req := client.requests()[0]
	if req.MaxTokens != 1234 || req.Temperature == nil || *req.Temperature != 0.2 {
		t.Fatalf("request sampling = %#v", req)
	}
	if req.Reasoning == nil || req.Reasoning.Effort != "high" {
		t.Fatalf("reasoning = %#v", req.Reasoning)
	}
}

type cancelAfterClient struct{ cancel context.CancelFunc }

func (c cancelAfterClient) Generate(context.Context, *llm.Request) (*llm.Response, error) {
	c.cancel()
	return toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{}`)), nil
}

func (cancelAfterClient) Info() llm.ProviderInfo { return llm.ProviderInfo{Name: "cancel"} }

func TestRunReturnsContextErrorAfterGenerate(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	spec := testSpec(cancelAfterClient{cancel: cancel})
	_, err := Run(ctx, spec)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestRunBeforeTurnStopsBeforeGenerating(t *testing.T) {
	stop := errors.New("budget exhausted")
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client)
	calls := 0
	spec.BeforeTurn = func(context.Context, llm.Usage) error {
		calls++
		return stop
	}
	_, err := Run(context.Background(), spec)
	if !errors.Is(err, stop) {
		t.Fatalf("error = %v, want stop error", err)
	}
	if calls != 1 {
		t.Fatalf("before-turn calls = %d, want 1", calls)
	}
	if got := len(client.requests()); got != 0 {
		t.Fatalf("model requests = %d, want 0", got)
	}
}

func TestRunBeforeTurnBlocksSecondModelCall(t *testing.T) {
	stop := errors.New("budget exhausted")
	first := toolResponse(llm.StopToolUse, call("c1", "echo", `{"value":"a"}`))
	first.Usage = llm.Usage{InputTokens: 3}
	client := &fakeClient{replies: []*llm.Response{first}}
	spec := testSpec(client, echoTool{name: "echo"})
	calls := 0
	spec.BeforeTurn = func(_ context.Context, usage llm.Usage) error {
		calls++
		if calls == 2 && usage.InputTokens == 3 {
			return stop
		}
		return nil
	}
	_, err := Run(context.Background(), spec)
	if !errors.Is(err, stop) {
		t.Fatalf("error = %v, want stop error", err)
	}
	if got := len(client.requests()); got != 1 {
		t.Fatalf("model requests = %d, want 1", got)
	}
}

type blockingClient struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
}

func (c *blockingClient) Generate(ctx context.Context, _ *llm.Request) (*llm.Response, error) {
	c.mu.Lock()
	c.calls++
	if c.calls == 1 {
		close(c.started)
	}
	c.mu.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (c *blockingClient) Info() llm.ProviderInfo { return llm.ProviderInfo{Name: "blocking"} }

func (c *blockingClient) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func TestRunCancelStopsInFlightGenerateWithoutAnotherRequest(t *testing.T) {
	client := &blockingClient{started: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, testSpec(client))
		done <- err
	}()
	select {
	case <-client.started:
	case <-time.After(2 * time.Second):
		t.Fatal("Generate was not called")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancellation")
	}
	if got := client.count(); got != 1 {
		t.Fatalf("model requests = %d, want 1", got)
	}
}

func TestRunReturnsUsageAfterTransportFailure(t *testing.T) {
	first := toolResponse(llm.StopToolUse, call("c1", "echo", `{"value":"a"}`))
	first.Usage = llm.Usage{InputTokens: 30, OutputTokens: 5}
	transient := &llm.Error{Kind: llm.ErrTransient}
	client := &fakeClient{
		replies: []*llm.Response{first},
		errs:    []error{nil, transient},
	}
	result, err := Run(context.Background(), testSpec(client, echoTool{name: "echo"}))
	if !errors.Is(err, transient) {
		t.Fatalf("error = %v, want transport error", err)
	}
	if result == nil {
		t.Fatal("failed Run dropped the usage from the successful tool turn")
	}
	if result.Usage.InputTokens != 30 || result.Usage.OutputTokens != 5 {
		t.Fatalf("usage = %+v, want the first turn usage", result.Usage)
	}
	if result.Turns != 1 {
		t.Fatalf("turns = %d, want 1", result.Turns)
	}
}
