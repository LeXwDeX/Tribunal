package llm

import (
	"testing"

	anthropic "github.com/anthropics/anthropic-sdk-go"
)

func TestAnthropicInputIncludesCacheTokens(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		input, read, write, want int64
	}{
		{name: "no cache", input: 10, want: 10},
		{name: "cache read", input: 10, read: 2, want: 12},
		{name: "cache creation", input: 10, write: 3, want: 13},
		{name: "read and creation", input: 10, read: 2, write: 3, want: 15},
		{name: "only cached input", read: 2, write: 3, want: 5},
		{name: "no input", want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var message anthropic.Message
			message.Usage.InputTokens = tc.input
			message.Usage.OutputTokens = 5
			message.Usage.CacheReadInputTokens = tc.read
			message.Usage.CacheCreationInputTokens = tc.write
			message.Usage.OutputTokensDetails.ThinkingTokens = 4

			resp, err := anthropicResponse(&message, "model")
			if err != nil {
				t.Fatal(err)
			}
			want := Usage{InputTokens: tc.want, OutputTokens: 5, CacheReadTokens: tc.read, CacheWriteTokens: tc.write, ReasoningTokens: 4}
			if resp.Usage != want {
				t.Fatalf("usage = %+v, want %+v", resp.Usage, want)
			}
		})
	}
}
