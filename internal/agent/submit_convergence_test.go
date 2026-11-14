package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

// nudgedRequest reports whether req ends with the periodic convergence prompt.
func nudgedRequest(req *llm.Request) bool {
	last := req.Messages[len(req.Messages)-1]
	return last.Role == llm.RoleUser && len(last.Parts) == 1 && last.Parts[0].Text == submitNudgePrompt
}

// transcriptContains reports whether the conversation carries text, either as
// an assistant part or inside a tool result.
func transcriptContains(result *RunResult, text string) bool {
	for _, message := range result.Transcript {
		for _, part := range message.Parts {
			if part.Text == text {
				return true
			}
			if part.ToolResult != nil && strings.Contains(part.ToolResult.Content, text) {
				return true
			}
		}
	}
	return false
}

// Without a request-count limit the loop must still converge the model toward
// submit_result: after a fixed number of turns it appends one convergence
// prompt without hard-limiting the run.
func TestRunNudgesSubmitWhenNoTurnLimit(t *testing.T) {
	replies := make([]*llm.Response, 0, submitNudgeTurns+1)
	for i := 0; i < submitNudgeTurns; i++ {
		replies = append(replies, toolResponse(llm.StopToolUse, call(fmt.Sprintf("c%d", i+1), "echo", `{}`)))
	}
	replies = append(replies, toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`)))
	client := &fakeClient{replies: replies}
	result, err := Run(context.Background(), testSpec(client, echoTool{name: "echo"}))
	if err != nil || result == nil || result.Turns != submitNudgeTurns+1 {
		t.Fatalf("err=%v result=%+v, want submit on turn %d", err, result, submitNudgeTurns+1)
	}
	reqs := client.requests()
	if len(reqs) != submitNudgeTurns+1 {
		t.Fatalf("requests = %d, want %d", len(reqs), submitNudgeTurns+1)
	}
	for i := 0; i < submitNudgeTurns; i++ {
		if nudgedRequest(reqs[i]) {
			t.Fatalf("request %d nudged before the period elapsed", i+1)
		}
	}
	if !nudgedRequest(reqs[submitNudgeTurns]) {
		t.Fatalf("request %d did not carry the convergence prompt: %+v", submitNudgeTurns+1,
			reqs[submitNudgeTurns].Messages[len(reqs[submitNudgeTurns].Messages)-1])
	}
}

// A lane whose context grows quickly must converge on accumulated tokens even
// before the turn period elapses.
func TestRunNudgesSubmitAfterTokenThreshold(t *testing.T) {
	first := toolResponse(llm.StopToolUse, call("c1", "echo", `{}`))
	first.Usage = llm.Usage{InputTokens: submitNudgeTokens, OutputTokens: 1}
	client := &fakeClient{replies: []*llm.Response{
		first,
		toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`)),
	}}
	result, err := Run(context.Background(), testSpec(client, echoTool{name: "echo"}))
	if err != nil || result == nil || result.Turns != 2 {
		t.Fatalf("err=%v result=%+v, want submit on turn 2", err, result)
	}
	reqs := client.requests()
	if nudgedRequest(reqs[0]) {
		t.Fatal("the first request was nudged")
	}
	if !nudgedRequest(reqs[1]) {
		t.Fatal("the token threshold did not cause a convergence prompt")
	}
}

// Near the workflow deadline only submit_result may run: an ignored prompt must
// not turn into more exploration, and the run must still be able to submit.
func TestRunNearDeadlineExecutesOnlySubmit(t *testing.T) {
	start := time.Now()
	executed := 0
	client := &steppingClient{clock: start, step: time.Minute, ignore: 1}
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(10*time.Minute))
	defer cancel()
	spec := testSpec(client, echoTool{name: "echo", fn: func(context.Context, json.RawMessage) (Result, error) {
		executed++
		return Result{Content: "ok"}, nil
	}})
	spec.Now = client.now
	result, err := Run(ctx, spec)
	if err != nil || result == nil {
		t.Fatalf("err=%v result=%+v, want a submitted result", err, result)
	}
	// Turn 10 starts below the two-slowest-turn reserve. The model ignores the
	// prompt there, so its call is refused and turn 11 submits.
	if result.Turns != 11 {
		t.Fatalf("turns = %d, want 11", result.Turns)
	}
	if executed != 9 {
		t.Fatalf("executed %d tool calls, want only the 9 pre-deadline turns", executed)
	}
	if !transcriptContains(result, "only "+SubmitToolName+" is executed now") {
		t.Fatal("the ignored deadline prompt did not refuse the extra tool call")
	}
}

// An explicit positive max_turns keeps its hard bound, but a rejected submit on
// the last turn grants a couple of submit-only grace turns.
func TestRunGraceTurnsAfterRejectedSubmit(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s1", SubmitToolName, `{"ok":false}`)),
		toolResponse(llm.StopToolUse, call("s2", SubmitToolName, `{"ok":true}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo"})
	spec.Submit.Decode = func(raw json.RawMessage) (any, error) {
		var in struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal(raw, &in); err != nil || !in.OK {
			return nil, errors.New("ok is required")
		}
		return in, nil
	}
	spec.MaxTurns = 2
	result, err := Run(context.Background(), spec)
	if err != nil || result == nil || result.Turns != 3 {
		t.Fatalf("err=%v result=%+v, want a submit on grace turn 3", err, result)
	}
	if result.Partial {
		t.Fatal("a successful run was marked partial")
	}
	if !hasSubmitPrompt(client.requests()[2]) {
		t.Fatal("the grace turn did not carry the last-turn submit prompt")
	}
}

// Tool calls refused by a submit-only last turn also earn grace turns, so a
// chatty model still gets a chance to submit instead of failing immediately.
func TestRunGraceTurnsAfterRejectedToolCalls(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("c1", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("c2", "echo", `{}`)),
		toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client, echoTool{name: "echo"})
	spec.MaxTurns = 1
	result, err := Run(context.Background(), spec)
	if err != nil || result == nil || result.Turns != 3 {
		t.Fatalf("err=%v result=%+v, want a submit on the second grace turn", err, result)
	}
}

// A model that ends the last turn with prose instead of calling a tool is also
// a rejected last turn and earns a grace turn.
func TestRunGraceTurnAfterProseEnd(t *testing.T) {
	prose := &llm.Response{
		Message:    llm.Message{Role: llm.RoleAssistant, Parts: []llm.Part{{Type: llm.PartText, Text: "done"}}},
		StopReason: llm.StopEndTurn,
	}
	client := &fakeClient{replies: []*llm.Response{
		prose,
		toolResponse(llm.StopToolUse, call("s", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client)
	spec.MaxTurns = 1
	result, err := Run(context.Background(), spec)
	if err != nil || result == nil || result.Turns != 2 {
		t.Fatalf("err=%v result=%+v, want a submit on the grace turn", err, result)
	}
	if !strings.Contains(client.requests()[1].Messages[len(client.requests()[1].Messages)-1].Parts[0].Text, SubmitToolName) {
		t.Fatal("the grace turn did not repeat the submit requirement")
	}
	found := false
	for _, msg := range client.requests()[1].Messages {
		if msg.Role == llm.RoleUser && len(msg.Parts) == 1 && msg.Parts[0].Text == mustSubmitPrompt {
			found = true
		}
	}
	if !found {
		t.Fatal("the transcript does not carry the repeated submit requirement")
	}
}

// When the grace turns are exhausted too, the run still returns the state it
// collected (usage, turns, transcript) marked partial instead of nothing.
func TestRunGraceExhaustionReturnsPartialResult(t *testing.T) {
	invalid := func(id string) *llm.Response {
		reply := toolResponse(llm.StopToolUse, call(id, SubmitToolName, `{"bad":1}`))
		reply.Usage = llm.Usage{InputTokens: 100, OutputTokens: 10}
		return reply
	}
	client := &fakeClient{replies: []*llm.Response{invalid("s1"), invalid("s2"), invalid("s3")}}
	spec := testSpec(client)
	spec.Submit.Decode = func(json.RawMessage) (any, error) { return nil, errors.New("nope") }
	spec.MaxTurns = 1
	result, err := Run(context.Background(), spec)
	if !errors.Is(err, ErrInvalidSubmit) {
		t.Fatalf("error = %v, want ErrInvalidSubmit", err)
	}
	if result == nil || !result.Partial || result.Turns != 3 {
		t.Fatalf("result = %+v, want a partial result after 3 turns", result)
	}
	if result.Usage.InputTokens != 300 || result.Usage.OutputTokens != 30 {
		t.Fatalf("usage = %+v, want the collected 300/30", result.Usage)
	}
	if len(result.Transcript) == 0 {
		t.Fatal("the partial result lost the transcript")
	}
}
