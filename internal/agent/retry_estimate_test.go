package agent

import (
	"context"
	"testing"
	"time"
)

func TestRunUsesInheritedTurnEstimateOnFirstRequest(t *testing.T) {
	start := time.Now()
	client := &steppingClient{clock: start, step: time.Second}
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(90*time.Second))
	defer cancel()
	spec := testSpec(client, echoTool{name: "echo"})
	spec.Now = client.now
	spec.TurnEstimate = time.Minute
	result, err := Run(ctx, spec)
	// 90s left is below two inherited one-minute turns, so the first request
	// already asks for submit_result.
	if err != nil || result == nil || result.Turns != 1 || !hasSubmitPrompt(client.reqs[0]) {
		t.Fatalf("err=%v requests=%d, want submit on the first request", err, len(client.reqs))
	}
	if result.LongestTurn != time.Minute {
		t.Fatalf("longest turn = %s, want the inherited 1m", result.LongestTurn)
	}
}

func TestRunReportsLongestTurnOnFailure(t *testing.T) {
	start := time.Now()
	client := &steppingClient{clock: start, step: 3 * time.Second}
	spec := testSpec(client, echoTool{name: "echo"})
	spec.Now = client.now
	spec.MaxTurns = 2
	client.ignore = 1 // ignore the last-turn prompt, so the run fails
	result, err := Run(context.Background(), spec)
	if err == nil || result == nil {
		t.Fatalf("err=%v result=%v, want a partial failure", err, result)
	}
	if result.LongestTurn != 3*time.Second {
		t.Fatalf("longest turn = %s, want 3s", result.LongestTurn)
	}
}

func TestRunInheritedEstimateWithoutDeadlineDoesNotPrompt(t *testing.T) {
	client := &steppingClient{clock: time.Now(), step: time.Second}
	spec := testSpec(client, echoTool{name: "echo"})
	spec.Now = client.now
	spec.TurnEstimate = time.Hour
	spec.MaxTurns = 3
	result, err := Run(context.Background(), spec)
	if err != nil || result.Turns != 3 || hasSubmitPrompt(client.reqs[0]) || hasSubmitPrompt(client.reqs[1]) {
		t.Fatalf("err=%v, want the prompt only on the last turn without a deadline", err)
	}
}
