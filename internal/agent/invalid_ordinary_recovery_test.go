package agent

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

// A reasoning gateway can corrupt the arguments of an ordinary exploration
// call just like those of a prompted submit (#605). These tests cover the
// recovery of such turns; the prompted submit turns stay in
// invalid_submit_recovery_test.go.

// recordingEcho is an echo tool that remembers the inputs it ran with.
func recordingEcho() (echoTool, *[]string) {
	var inputs []string
	return echoTool{name: "echo", fn: func(_ context.Context, input json.RawMessage) (Result, error) {
		inputs = append(inputs, string(input))
		return Result{Content: "ok"}, nil
	}}, &inputs
}

func messageRoles(messages []llm.Message) []llm.Role {
	roles := make([]llm.Role, 0, len(messages))
	for _, msg := range messages {
		roles = append(roles, msg.Role)
	}
	return roles
}

func TestRunRecoversFromInvalidCallOnOrdinaryTurn(t *testing.T) {
	// The corrupted call shares its turn with a valid one: the whole batch is
	// rejected, so neither may run.
	rejected := toolResponse(llm.StopToolUse,
		call("bad", "echo", corruptSubmitInput),
		call("mate", "echo", `{"value":"batch-mate"}`))
	rejected.Message.Parts = append([]llm.Part{
		{Type: llm.PartText, Text: "draft"},
		{Type: llm.PartReasoning, Reasoning: &llm.Reasoning{Protocol: "openai-responses", Raw: json.RawMessage(`{"type":"reasoning","id":"rs_2","summary":[]}`)}},
	}, rejected.Message.Parts...)
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("e1", "echo", `{"value":"first"}`)),
		rejected,
		toolResponse(llm.StopToolUse, call("e2", "echo", `{"value":"second"}`)),
		toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
	}}
	echo, inputs := recordingEcho()
	result, err := Run(context.Background(), testSpec(client, echo))
	if err != nil {
		t.Fatalf("Run: %v, want the run to survive one corrupted ordinary call", err)
	}
	if result.Turns != 4 {
		t.Fatalf("turns = %d, want 4 (explore, rejected, resend, submit)", result.Turns)
	}
	if want := []string{`{"value":"first"}`, `{"value":"second"}`}; !slices.Equal(*inputs, want) {
		t.Fatalf("echo inputs = %q, want %q: the rejected batch must never run", *inputs, want)
	}

	retry := client.requests()[2]
	if hasSubmitPrompt(retry) || client.callMetas()[2].SubmitPrompt != "" {
		t.Fatal("an ordinary turn must resume as an ordinary turn, without the submit prompt")
	}
	last := retry.Messages[len(retry.Messages)-1]
	if last.Role != llm.RoleUser || len(last.Parts) != 1 || last.Parts[0].Text != invalidToolCallResendFeedback(llm.ToolCallInvalidJSON) {
		t.Fatalf("last message = %+v, want the fixed resend feedback", last)
	}
	kept := retry.Messages[len(retry.Messages)-2]
	if kept.Role != llm.RoleAssistant || len(kept.Parts) != 1 || kept.Parts[0].Text != "draft" {
		t.Fatalf("message before the feedback = %+v, want only the assistant text of the rejected turn", kept)
	}
	for _, msg := range retry.Messages {
		for _, part := range msg.Parts {
			if part.Type == llm.PartReasoning {
				t.Fatal("reasoning of the rejected turn must not be replayed")
			}
			if part.ToolCall != nil && (part.ToolCall.ID == "" || part.ToolCall.ID == "bad" || part.ToolCall.ID == "mate") {
				t.Fatal("a rejected tool call must not be replayed")
			}
			replayed := part.Text
			if part.ToolCall != nil {
				replayed += string(part.ToolCall.Input)
			}
			if strings.Contains(replayed, "PROVIDER-SECRET-TEXT") || strings.Contains(replayed, "batch-mate") {
				t.Fatal("provider-controlled tool input must not reach the next request")
			}
		}
	}
	wantRoles := []llm.Role{llm.RoleUser, llm.RoleAssistant, llm.RoleTool, llm.RoleAssistant, llm.RoleUser,
		llm.RoleAssistant, llm.RoleTool, llm.RoleAssistant, llm.RoleTool}
	if got := messageRoles(result.Transcript); !slices.Equal(got, wantRoles) {
		t.Fatalf("transcript roles = %v, want %v", got, wantRoles)
	}
}

func TestRunFailsClosedOnceOrdinaryRecoveriesAreSpent(t *testing.T) {
	// The old contract was to fail at the first corrupted ordinary call; now
	// only a provider that keeps corrupting its calls ends the run.
	replies := []*llm.Response{toolResponse(llm.StopToolUse, call("e1", "echo", `{}`))}
	for i := 0; i <= maxOrdinaryInvalidRecoveries; i++ {
		replies = append(replies, toolResponse(llm.StopToolUse, call("bad"+strconv.Itoa(i), "echo", corruptSubmitInput)))
	}
	replies = append(replies, toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)))
	client := &fakeClient{replies: replies}
	echo, inputs := recordingEcho()
	result, err := Run(context.Background(), testSpec(client, echo))
	if !errors.Is(err, ErrUnsafeResponse) || strings.Contains(err.Error(), "PROVIDER-SECRET-TEXT") {
		t.Fatalf("err = %v, want unsafe once the recoveries are spent", err)
	}
	wantRequests := 1 + maxOrdinaryInvalidRecoveries + 1
	if got := len(client.requests()); got != wantRequests {
		t.Fatalf("requests = %d, want %d (one explore, %d recovered, one fatal)", got, wantRequests, maxOrdinaryInvalidRecoveries)
	}
	if len(*inputs) != 1 {
		t.Fatalf("echo ran %d times, want only the valid first call", len(*inputs))
	}
	if result == nil || !result.Partial || result.Turns != wantRequests {
		t.Fatalf("result = %+v, want a partial result with %d turns", result, wantRequests)
	}
	raw, marshalErr := json.Marshal(result.Transcript)
	if marshalErr != nil || strings.Contains(string(raw), "PROVIDER-SECRET-TEXT") {
		t.Fatalf("transcript leaks the rejected input or cannot be serialized: %v", marshalErr)
	}
}

func TestRunOrdinaryRecoverySizesContextWithoutRejectedOutput(t *testing.T) {
	// The rejected output is not replayed, so it must not count toward the
	// context estimate: a huge rejected turn would otherwise turn the resend
	// into a context-prompted, submit-only turn.
	withUsage := func(resp *llm.Response, in, out int64) *llm.Response {
		resp.Usage = llm.Usage{InputTokens: in, OutputTokens: out}
		return resp
	}
	client := &scriptedClient{window: 100000, replies: []*llm.Response{
		withUsage(toolResponse(llm.StopToolUse, call("e1", "echo", `{}`)), 1000, 10),
		withUsage(toolResponse(llm.StopToolUse, call("bad", "echo", corruptSubmitInput)), 1000, 90000),
		withUsage(toolResponse(llm.StopToolUse, call("e2", "echo", `{}`)), 1000, 10),
		withUsage(toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)), 1000, 10),
	}}
	echo, inputs := recordingEcho()
	if _, err := Run(context.Background(), testSpec(client, echo)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if hasSubmitPrompt(client.reqs[2]) {
		t.Fatal("the resend turn must not become a submit turn because of the rejected output")
	}
	for _, msg := range client.reqs[2].Messages {
		for _, part := range msg.Parts {
			if part.ToolResult != nil && part.ToolResult.Content == compactedToolResult {
				t.Fatal("the resend turn must not compact because of the rejected output")
			}
		}
	}
	if len(*inputs) != 2 {
		t.Fatalf("echo ran %d times, want the explore call and the resent call", len(*inputs))
	}
}

func TestRunOrdinaryRecoveryNeverOutlivesTheLastTurn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		maxTurns int
		replies  []*llm.Response
		requests int
		wantErr  bool
	}{
		{
			name:     "corrupted last turn is not recovered",
			maxTurns: 2,
			replies: []*llm.Response{
				toolResponse(llm.StopToolUse, call("e1", "echo", `{}`)),
				toolResponse(llm.StopToolUse, call("bad", "echo", corruptSubmitInput)),
				toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
			},
			requests: 2,
			wantErr:  true,
		},
		{
			name:     "recovery does not extend max_turns",
			maxTurns: 3,
			replies: []*llm.Response{
				toolResponse(llm.StopToolUse, call("bad1", "echo", corruptSubmitInput)),
				toolResponse(llm.StopToolUse, call("e1", "echo", `{}`)),
				toolResponse(llm.StopToolUse, call("bad2", SubmitToolName, corruptSubmitInput)),
				toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
			},
			requests: 3,
			wantErr:  true,
		},
		{
			name:     "recovered turn still leaves the last turn to submit",
			maxTurns: 3,
			replies: []*llm.Response{
				toolResponse(llm.StopToolUse, call("e1", "echo", `{}`)),
				toolResponse(llm.StopToolUse, call("bad", "echo", corruptSubmitInput)),
				toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
			},
			requests: 3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeClient{replies: tc.replies}
			spec := testSpec(client, echoTool{name: "echo"})
			spec.MaxTurns = tc.maxTurns
			result, err := Run(context.Background(), spec)
			if tc.wantErr != errors.Is(err, ErrUnsafeResponse) || (!tc.wantErr && err != nil) {
				t.Fatalf("err = %v, want unsafe=%v", err, tc.wantErr)
			}
			if got := len(client.requests()); got != tc.requests {
				t.Fatalf("requests = %d, want %d", got, tc.requests)
			}
			if !tc.wantErr {
				if result.Turns != tc.maxTurns || !hasSubmitPrompt(client.requests()[tc.maxTurns-1]) {
					t.Fatalf("turns = %d, want the last turn %d to carry the submit prompt", result.Turns, tc.maxTurns)
				}
			}
		})
	}
}

func TestRunFailsClosedOnProtocolViolationOnOrdinaryTurn(t *testing.T) {
	marker := func(kind string) llm.Part {
		return llm.Part{Type: llm.PartToolCall, ToolCall: &llm.ToolCall{Invalid: kind}}
	}
	response := func(stop llm.StopReason, parts ...llm.Part) *llm.Response {
		return &llm.Response{StopReason: stop, Message: llm.Message{Role: llm.RoleAssistant, Parts: parts}}
	}
	cases := map[string]*llm.Response{
		"unsupported item": response(llm.StopToolUse, marker(llm.ToolCallUnsupported)),
		"malformed list":   response(llm.StopToolUse, marker(llm.ToolCallMalformedList)),
		"missing call":     response(llm.StopToolUse, llm.Part{Type: llm.PartToolCall}),
		// A recoverable call must not mask a later protocol violation.
		"mixed batch": response(llm.StopToolUse,
			llm.Part{Type: llm.PartToolCall, ToolCall: call("bad", "echo", corruptSubmitInput)}, marker(llm.ToolCallUnsupported)),
		"valid call with end_turn":   toolResponse(llm.StopEndTurn, call("c", "echo", `{}`)),
		"corrupt call with end_turn": toolResponse(llm.StopEndTurn, call("bad", "echo", corruptSubmitInput)),
		"corrupt call with max_tokens": toolResponse(llm.StopMaxTokens,
			call("bad", "echo", corruptSubmitInput)),
		"tool_use without calls": toolResponse(llm.StopToolUse),
	}
	for name, reply := range cases {
		t.Run(name, func(t *testing.T) {
			client := &fakeClient{replies: []*llm.Response{
				toolResponse(llm.StopToolUse, call("e1", "echo", `{}`)),
				reply,
				toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{"done":true}`)),
			}}
			echo, inputs := recordingEcho()
			if _, err := Run(context.Background(), testSpec(client, echo)); !errors.Is(err, ErrUnsafeResponse) {
				t.Fatalf("err = %v, want unsafe", err)
			}
			if got := len(client.requests()); got != 2 {
				t.Fatalf("requests = %d, want 2: a protocol violation must not be answered with another request", got)
			}
			if len(*inputs) != 1 {
				t.Fatalf("echo ran %d times, want only the valid first call", len(*inputs))
			}
		})
	}
}

func TestRunOrdinaryRecoveriesDoNotConsumeThePromptedBudget(t *testing.T) {
	// Exploration and the prompted submit have separate recovery budgets: two
	// corrupted exploration calls leave the submit turn all its retries (#269),
	// and do not grant it any more.
	for _, tc := range []struct {
		name       string
		lastInput  string
		wantUnsafe bool
	}{
		{name: "submit turn keeps both retries", lastInput: `{"done":true}`},
		{name: "submit turn gets no extra retry", lastInput: corruptSubmitInput, wantUnsafe: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			next := 0
			reply := func(name, input string) *llm.Response {
				next++
				resp := toolResponse(llm.StopToolUse, call("c"+strconv.Itoa(next), name, input))
				resp.Usage = llm.Usage{InputTokens: 1000, OutputTokens: 10}
				return resp
			}
			client := &fakeClient{replies: []*llm.Response{
				reply("echo", corruptSubmitInput),         // ordinary turn: first ordinary recovery
				reply("echo", corruptSubmitInput),         // ordinary turn: second one; the budget prompt starts after it
				reply(SubmitToolName, corruptSubmitInput), // prompted turn: first prompted recovery
				reply(SubmitToolName, corruptSubmitInput), // prompted resubmit: second prompted recovery
				reply(SubmitToolName, tc.lastInput),       // prompted resubmit: accepted, or no recovery left
			}}
			result, err := Run(context.Background(), budgetSpec(client))
			if tc.wantUnsafe != errors.Is(err, ErrUnsafeResponse) || (!tc.wantUnsafe && err != nil) {
				t.Fatalf("err = %v, want unsafe=%v", err, tc.wantUnsafe)
			}
			if got := len(client.requests()); got != 5 {
				t.Fatalf("requests = %d, want 5 (two ordinary recoveries, two prompted ones, a last reply)", got)
			}
			if !tc.wantUnsafe && result.Turns != 5 {
				t.Fatalf("turns = %d, want a submit on turn 5", result.Turns)
			}
			for i, meta := range client.callMetas() {
				want := llm.SubmitPromptBudget
				if i < 2 {
					want = ""
				}
				if meta.SubmitPrompt != want {
					t.Fatalf("turn %d submit prompt = %q, want %q", i+1, meta.SubmitPrompt, want)
				}
			}
			// Each kind of turn is answered with its own feedback.
			reqs := client.requests()
			feedbackBeforeEnd := func(req *llm.Request, fromEnd int) string {
				return req.Messages[len(req.Messages)-1-fromEnd].Parts[0].Text
			}
			if got := feedbackBeforeEnd(reqs[1], 0); got != invalidToolCallResendFeedback(llm.ToolCallInvalidJSON) {
				t.Fatalf("feedback after the first rejection = %q, want the ordinary resend feedback", got)
			}
			if !hasSubmitPrompt(reqs[3]) || feedbackBeforeEnd(reqs[3], 1) != invalidToolCallFeedback(llm.ToolCallInvalidJSON) {
				t.Fatal("the prompted resubmit must carry the submit prompt after the submit feedback")
			}
		})
	}
}

func TestOrdinaryRecoveryFeedbackNamesOnlyTheCategory(t *testing.T) {
	for _, kind := range []string{llm.ToolCallInvalidJSON, llm.ToolCallNotObject, llm.ToolCallEmptyInput, llm.ToolCallNonString,
		llm.ToolCallEmptyID, llm.ToolCallEmptyName, llm.ToolCallDuplicateID} {
		text := invalidToolCallResendFeedback(kind)
		if !strings.Contains(text, "("+kind+")") {
			t.Fatalf("feedback for %s = %q, want the fixed category", kind, text)
		}
		// The submit-phase feedback forces submit_result; an exploring run must
		// stay free to call its other tools.
		if strings.Contains(text, SubmitToolName) || strings.Contains(text, "Do not call any other tool") {
			t.Fatalf("feedback for %s = %q, must not restrict the model to submit_result", kind, text)
		}
		if !strings.Contains(text, `\"`) || !strings.Contains(text, "「」") {
			t.Fatalf("feedback for %s = %q, must ask to escape quotes inside strings", kind, text)
		}
	}
	if invalidToolCallResendFeedback(llm.ToolCallInvalidJSON) == invalidToolCallFeedback(llm.ToolCallInvalidJSON) {
		t.Fatal("ordinary and submit-phase feedback must differ")
	}
}

// toolStopFor is the provider stop value that ends a turn with tool calls.
func toolStopFor(protocol string) string {
	switch protocol {
	case "openai-chat":
		return "tool_calls"
	case "openai-responses":
		return ""
	}
	return "tool_use"
}

func TestProviderSafetyRecoversFromInvalidToolInputOnOrdinaryTurn(t *testing.T) {
	// End to end through the real registry and each adapter: the rejected turn
	// is audited as unsafe, yet the response still reaches the loop, which
	// resends only fixed feedback.
	for _, protocol := range []string{"openai-chat", "openai-responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			stop := toolStopFor(protocol)
			// Anthropic carries the input as a JSON value, so truncated text
			// reaches the loop as a string rather than as invalid JSON.
			wantKind := llm.ToolCallInvalidJSON
			if protocol == "anthropic" {
				wantKind = llm.ToolCallNotObject
			}
			rejected := safetyResponse(t, protocol, stop, false, []safetyCall{
				{"bad", "echo", `{"value":"` + safetySecret, false},
				{"mate", "echo", `{"value":"` + safetySecret + `"}`, false},
			}, true)
			resend := safetyResponse(t, protocol, stop, false, []safetyCall{{"again", "echo", `{"value":"yes"}`, false}}, false)
			submit := safetyResponse(t, protocol, stop, false, []safetyCall{{"submit", SubmitToolName, `{}`, false}}, false)
			client, count, records, requests := safetyClient(t, protocol, rejected, resend, submit)
			result, tools, submits, err := safetyRun(client)
			if err != nil || tools != 1 || submits != 1 || count.Load() != 3 || len(*records) != 3 {
				t.Fatalf("err=%v tools=%d submits=%d requests=%d records=%d, want one recovered turn", err, tools, submits, count.Load(), len(*records))
			}
			if result.Turns != 3 {
				t.Fatalf("turns = %d, want 3", result.Turns)
			}
			audit := (*records)[0].Audit
			if audit == nil || audit.Response == nil || audit.Response.InvalidToolCall != wantKind {
				t.Fatalf("audit = %+v, want the rejected turn categorized as %s", audit, wantKind)
			}
			var failure *llm.Error
			if !errors.As((*records)[0].Err, &failure) || failure.Kind != llm.ErrUnsafeResponse {
				t.Fatalf("record error = %v, want the audit pre-classification unsafe_model_response", (*records)[0].Err)
			}
			if (*records)[1].Err != nil || (*records)[2].Err != nil {
				t.Fatalf("recovered turns must be audited as successes: %v, %v", (*records)[1].Err, (*records)[2].Err)
			}
			retry := (*requests)[1]
			for _, leaked := range []string{safetySecret, "encrypted-safe", "signature-safe", `"bad"`, `"mate"`} {
				if strings.Contains(retry, leaked) {
					t.Fatalf("the request after the rejection replays %q", leaked)
				}
			}
			if !strings.Contains(retry, wantKind) {
				t.Fatalf("the request after the rejection must name the fixed category %s", wantKind)
			}
			for _, record := range *records {
				raw, marshalErr := json.Marshal(record)
				if marshalErr != nil || strings.Contains(string(raw), safetySecret) {
					t.Fatalf("audit leaks tool arguments or cannot be serialized: %v", marshalErr)
				}
			}
		})
	}
}
