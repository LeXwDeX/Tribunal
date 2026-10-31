package llm

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// usageWireField maps one provider token field to the neutral Usage. Required
// fields are the provider's core contract: a missing core count makes the
// usage unknown, while optional detail fields may be absent.
type usageWireField struct {
	path     []string
	set      func(*Usage, int64)
	required bool
}

// wireUsage validates token counts in the provider's original JSON. The SDKs
// decode integers through gjson, which truncates fractions and saturates
// values outside int64. The converted fields cannot distinguish those values
// from real counts, so the raw payload is the only reliable boundary.
func wireUsage(raw string, fields []usageWireField) (Usage, bool) {
	return wireUsageFromResponse(raw, raw, fields)
}

// wireUsageFromResponse also distinguishes a provider response without a usage
// object from a directly constructed SDK value. An empty response payload is a
// test or caller-constructed value; a non-empty response without usage cannot
// prove any token count and is marked incomplete.
func wireUsageFromResponse(responseRaw, raw string, fields []usageWireField) (Usage, bool) {
	if raw == "" {
		incomplete := responseRaw != ""
		return Usage{Incomplete: incomplete}, true
	}
	root := make(map[string]json.RawMessage)
	if err := json.Unmarshal([]byte(raw), &root); err != nil {
		return Usage{Incomplete: true}, false
	}
	usage := Usage{}
	complete := true
	for _, field := range fields {
		value, found, valid := lookupWireValue(root, field.path)
		if !found {
			if field.required {
				usage.Incomplete = true
			}
			continue
		}
		if !valid {
			complete = false
			continue
		}
		number, ok := wireTokenCount(value)
		if !ok {
			complete = false
			continue
		}
		field.set(&usage, number)
	}
	if !complete {
		usage.Incomplete = true
	}
	return usage, complete
}

func lookupWireValue(root map[string]json.RawMessage, path []string) (json.RawMessage, bool, bool) {
	current := root
	for i, key := range path {
		value, ok := current[key]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, false, true
		}
		if i == len(path)-1 {
			return value, true, true
		}
		next := make(map[string]json.RawMessage)
		if err := json.Unmarshal(value, &next); err != nil {
			return nil, true, false
		}
		current = next
	}
	return nil, false, true
}

// wireTokenCount accepts only a nonnegative JSON integer that fits int64.
// Fractions, exponents, strings, booleans and overflowing values are invalid.
func wireTokenCount(raw json.RawMessage) (int64, bool) {
	value := bytes.TrimSpace(raw)
	if len(value) == 0 {
		return 0, false
	}
	start := 0
	if value[0] == '-' {
		start = 1
	}
	if start == len(value) {
		return 0, false
	}
	for _, b := range value[start:] {
		if b < '0' || b > '9' {
			return 0, false
		}
	}
	number, err := strconv.ParseInt(string(value), 10, 64)
	if err != nil || number < 0 {
		return 0, false
	}
	return number, true
}

var chatUsageFields = []usageWireField{
	{path: []string{"prompt_tokens"}, set: func(u *Usage, n int64) { u.InputTokens = n }, required: true},
	{path: []string{"completion_tokens"}, set: func(u *Usage, n int64) { u.OutputTokens = n }, required: true},
	{path: []string{"prompt_tokens_details", "cached_tokens"}, set: func(u *Usage, n int64) { u.CacheReadTokens = n }},
	{path: []string{"prompt_tokens_details", "cache_write_tokens"}, set: func(u *Usage, n int64) { u.CacheWriteTokens = n }},
	{path: []string{"completion_tokens_details", "reasoning_tokens"}, set: func(u *Usage, n int64) { u.ReasoningTokens = n }},
}

var responsesUsageFields = []usageWireField{
	{path: []string{"input_tokens"}, set: func(u *Usage, n int64) { u.InputTokens = n }, required: true},
	{path: []string{"output_tokens"}, set: func(u *Usage, n int64) { u.OutputTokens = n }, required: true},
	{path: []string{"input_tokens_details", "cached_tokens"}, set: func(u *Usage, n int64) { u.CacheReadTokens = n }},
	{path: []string{"input_tokens_details", "cache_write_tokens"}, set: func(u *Usage, n int64) { u.CacheWriteTokens = n }},
	{path: []string{"output_tokens_details", "reasoning_tokens"}, set: func(u *Usage, n int64) { u.ReasoningTokens = n }},
}

var anthropicUsageFields = []usageWireField{
	{path: []string{"input_tokens"}, set: func(u *Usage, n int64) { u.InputTokens = n }, required: true},
	{path: []string{"output_tokens"}, set: func(u *Usage, n int64) { u.OutputTokens = n }, required: true},
	{path: []string{"cache_read_input_tokens"}, set: func(u *Usage, n int64) { u.CacheReadTokens = n }},
	{path: []string{"cache_creation_input_tokens"}, set: func(u *Usage, n int64) { u.CacheWriteTokens = n }},
	{path: []string{"output_tokens_details", "thinking_tokens"}, set: func(u *Usage, n int64) { u.ReasoningTokens = n }},
}
