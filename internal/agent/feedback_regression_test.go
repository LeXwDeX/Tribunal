package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

const feedbackCanary = "feedback-sensitive-canary"

func TestFeedbackRegressionMCPErrorResultIsNotEchoed(t *testing.T) {
	for _, returnErr := range []bool{false, true} {
		t.Run(fmt.Sprint("go_error=", returnErr), func(t *testing.T) {
			client := &fakeClient{replies: []*llm.Response{
				toolResponse(llm.StopToolUse, call("mcp", "mcp__server__tool", `{}`)),
				toolResponse(llm.StopToolUse, call("submit", SubmitToolName, `{}`)),
			}}
			tool := echoTool{name: "mcp__server__tool", fn: func(context.Context, json.RawMessage) (Result, error) {
				result := Result{Content: "server failed: " + feedbackCanary, IsError: true}
				if returnErr {
					return result, errors.New(feedbackCanary)
				}
				return result, nil
			}}
			result, err := Run(context.Background(), testSpec(client, tool))
			if err != nil {
				t.Fatal(err)
			}
			got := client.requests()[1].Messages[2].Parts[0].ToolResult
			if got == nil || !got.IsError || !strings.Contains(got.Content, "tool execution failed") || strings.Contains(got.Content, feedbackCanary) {
				t.Fatalf("unsafe tool feedback: %+v", got)
			}
			if result.Transcript[2].Parts[0].ToolResult.Content != got.Content {
				t.Fatal("transcript and model feedback differ")
			}
		})
	}
}

func TestFeedbackRegressionBuiltinErrorsCanBeRepaired(t *testing.T) {
	sb, _, _ := newTestSandbox(t)
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse,
			call("missing", "read_file", `{}`),
			call("range", "read_file", `{"path":"main.go","start_line":-1}`),
			call("regexp", "grep", `{"pattern":"[`+feedbackCanary+`"}`),
			call("glob", "glob", `{"pattern":"../`+feedbackCanary+`"}`),
			call("type", "read_file", `{"path":"main.go","start_line":"`+feedbackCanary+`"}`),
			call("missing-file", "read_file", `{"path":"`+feedbackCanary+`"}`),
			call("bad-root", "read_file", `{"path":"main.go","root":"`+feedbackCanary+`"}`),
			call("unsafe-path", "read_file", `{"path":"../`+feedbackCanary+`"}`),
			call("git-max", "git_log", `{"max":-2}`),
			call("blame-path", "git_blame", `{}`),
			call("blame-range", "git_blame", `{"path":"main.go","start_line":9,"end_line":2}`),
			call("blame-limit", "git_blame", `{"path":"main.go","start_line":1,"end_line":999}`),
			call("git-root", "git_blame", `{"path":"main.go","root":"`+feedbackCanary+`","start_line":1,"end_line":1}`),
			call("unknown", feedbackCanary, `{}`)),
		toolResponse(llm.StopToolUse, call("read", "read_file", `{"path":"main.go"}`)),
		toolResponse(llm.StopToolUse, call("submit", SubmitToolName, `{}`)),
	}}
	result, err := Run(context.Background(), testSpec(client, BuiltinTools(sb)...))
	if err != nil {
		t.Fatal(err)
	}
	feedback := client.requests()[1].Messages[2].Parts
	for i, want := range []string{"path", "start_line", "pattern", "pattern", "start_line", "path", "root", "path", "max", "path", "end_line", "end_line", "root", "unknown tool"} {
		got := feedback[i].ToolResult
		if got == nil || !got.IsError || !strings.Contains(got.Content, want) || strings.Contains(got.Content, feedbackCanary) {
			t.Errorf("feedback %d = %+v, want safe field/category %q", i, got, want)
		}
	}
	if !strings.Contains(feedback[0].ToolResult.Content, "required") || !strings.Contains(feedback[1].ToolResult.Content, "range") || !strings.Contains(feedback[4].ToolResult.Content, "type") {
		t.Fatal("feedback omitted actionable error categories")
	}
	if !strings.Contains(feedback[5].ToolResult.Content, "accessible") || !strings.Contains(feedback[6].ToolResult.Content, "root") || !strings.Contains(feedback[7].ToolResult.Content, "sandbox") {
		t.Fatal("feedback omitted actionable sandbox error categories")
	}
	if !strings.Contains(feedback[8].ToolResult.Content, "range") || !strings.Contains(feedback[9].ToolResult.Content, "required") || !strings.Contains(feedback[10].ToolResult.Content, "start_line") || !strings.Contains(feedback[11].ToolResult.Content, "400") {
		t.Fatal("feedback omitted actionable git input categories")
	}
	read := result.Transcript[4].Parts[0].ToolResult
	if read.IsError || !strings.Contains(read.Content, "package head") {
		t.Fatalf("corrected read failed: %+v", read)
	}
}

type feedbackRegressionForged struct{ panicOnFeedback bool }

func (feedbackRegressionForged) Error() string { return feedbackCanary }
func (e feedbackRegressionForged) AgentValidationFeedback() (string, string) {
	if e.panicOnFeedback {
		panic(feedbackCanary)
	}
	return feedbackCanary, feedbackCanary
}

type feedbackRegressionForgedIndex struct{}

func (feedbackRegressionForgedIndex) Error() string { return "numeric-secret-843" }
func (feedbackRegressionForgedIndex) AgentValidationFeedback() (string, string) {
	return "findings[843].severity", "value"
}

func TestFeedbackRegressionUntrustedFeedbackAndPanic(t *testing.T) {
	for _, panicOnFeedback := range []bool{false, true} {
		t.Run(fmt.Sprint("panic=", panicOnFeedback), func(t *testing.T) {
			client := &fakeClient{replies: []*llm.Response{
				toolResponse(llm.StopToolUse, call("bad", SubmitToolName, `{}`)),
				toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{}`)),
			}}
			spec := testSpec(client)
			attempts := 0
			spec.Submit.Decode = func(json.RawMessage) (any, error) {
				attempts++
				if attempts == 1 {
					return nil, feedbackRegressionForged{panicOnFeedback}
				}
				return "ok", nil
			}
			result, err := Run(context.Background(), spec)
			if err != nil || result.Output != "ok" {
				t.Fatalf("recovery failed: result=%+v err=%v", result, err)
			}
			got := client.requests()[1].Messages[2].Parts[0].ToolResult
			if !got.IsError || strings.Contains(got.Content, feedbackCanary) {
				t.Fatalf("unsafe forged feedback: %+v", got)
			}
		})
	}
}

func TestFeedbackRegressionUntrustedFieldIndexIsNotEchoed(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("bad", SubmitToolName, `{}`)),
		toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client)
	attempts := 0
	spec.Submit.Decode = func(json.RawMessage) (any, error) {
		attempts++
		if attempts == 1 {
			return nil, feedbackRegressionForgedIndex{}
		}
		return "ok", nil
	}
	if _, err := Run(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	got := client.requests()[1].Messages[2].Parts[0].ToolResult
	if !got.IsError || strings.Contains(got.Content, "843") || !strings.Contains(got.Content, "findings[].severity") {
		t.Fatalf("unsafe numeric field feedback: %+v", got)
	}
}

func TestFeedbackRegressionSubmitDecoderPanicDoesNotLeak(t *testing.T) {
	client := &fakeClient{replies: []*llm.Response{
		toolResponse(llm.StopToolUse, call("bad", SubmitToolName, `{"secret":"`+feedbackCanary+`"}`)),
		toolResponse(llm.StopToolUse, call("ok", SubmitToolName, `{}`)),
	}}
	spec := testSpec(client)
	attempts := 0
	spec.Submit.Decode = func(raw json.RawMessage) (any, error) {
		attempts++
		if attempts == 1 {
			panic(string(raw))
		}
		return "ok", nil
	}
	result, err := Run(context.Background(), spec)
	if err != nil || result.Output != "ok" {
		t.Fatalf("decoder panic recovery failed: result=%+v err=%v", result, err)
	}
	got := client.requests()[1].Messages[2].Parts[0].ToolResult
	if got == nil || !got.IsError || strings.Contains(got.Content, feedbackCanary) {
		t.Fatalf("decoder panic leaked: %+v", got)
	}
}
