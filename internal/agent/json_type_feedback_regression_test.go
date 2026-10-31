package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestJSONTypeFeedbackArrayFields(t *testing.T) {
	const canary = "json-type-secret-canary"
	tests := []struct {
		name, field, want string
		typ               reflect.Type
	}{
		{"findings without index", "findings.line", "findings[].line", reflect.TypeOf(0)},
		{"findings with index", "findings.7.line", "findings[].line", reflect.TypeOf(0)},
		{"large accepted index", "findings.843.line", "findings[].line", reflect.TypeOf(0)},
		{"packages without index", "packages.provider", "packages[].provider", reflect.TypeOf("")},
		{"nested arrays without indices", "packages.blocks", "packages[].blocks[]", reflect.TypeOf("")},
		{"nested arrays with indices", "packages.2.blocks.8", "packages[].blocks[]", reflect.TypeOf("")},
		{"nested array with mixed indices", "packages.2.blocks", "packages[].blocks[]", reflect.TypeOf("")},
		{"nested array itself", "packages.blocks", "packages[].blocks", reflect.TypeOf([]string{})},
		{"array itself", "findings", "findings", reflect.TypeOf([]struct{}{})},
		{"array element", "findings", "findings[]", reflect.TypeOf(struct{}{})},
		{"root evidence array", "evidence.path", "evidence[].path", reflect.TypeOf("")},
		{"root evidence element", "evidence", "evidence[]", reflect.TypeOf(struct{}{})},
		{"root scalar evidence", "evidence", "evidence", reflect.TypeOf("")},
		{"scalar evidence", "findings.evidence", "findings[].evidence", reflect.TypeOf("")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("%s: %w", canary, &json.UnmarshalTypeError{Value: canary, Field: tt.field, Type: tt.typ})
			got := validationFeedback(err)
			want := tt.want + ": incorrect JSON type; check the schema"
			if got != want || strings.Contains(got, canary) {
				t.Fatalf("validationFeedback = %q, want %q", got, want)
			}
		})
	}
}

func TestJSONTypeFeedbackRejectsForgedFields(t *testing.T) {
	const canary = "json-type-secret-canary"
	for _, field := range []string{
		canary,
		"findings." + canary + ".line",
		"findings.123456789.line",
		"findings.-829.line",
		"findings[-829].line",
		"findings.01.line",
		"packages.999999.blocks.1",
		"findings[2]." + canary,
	} {
		t.Run(field, func(t *testing.T) {
			err := fmt.Errorf("%s: %w", canary, &json.UnmarshalTypeError{Value: canary, Field: field, Type: reflect.TypeOf(0)})
			if got, want := validationFeedback(err), "incorrect JSON type; check the schema"; got != want {
				t.Fatalf("validationFeedback = %q, want %q", got, want)
			}
		})
	}
}

func TestJSONTypeFeedbackWithoutErrorType(t *testing.T) {
	err := &json.UnmarshalTypeError{Field: "findings.line"}
	if got, want := validationFeedback(err), "findings[].line: incorrect JSON type; check the schema"; got != want {
		t.Fatalf("validationFeedback = %q, want %q", got, want)
	}
}

func TestJSONTypeFeedbackRealDecoder(t *testing.T) {
	const canary = "json-type-secret-canary"
	var out struct {
		Packages []struct {
			Blocks []string `json:"blocks"`
		} `json:"packages"`
		Findings []struct {
			Line int `json:"line"`
		} `json:"findings"`
	}
	for _, tt := range []struct {
		name, input, field string
	}{
		{"findings", `{"findings":[{"line":"` + canary + `"}]}`, "findings[].line"},
		{"packages", `{"packages":[{"blocks":["ok", 42]}]}`, "packages[].blocks[]"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := json.Unmarshal([]byte(tt.input), &out)
			if err == nil {
				t.Fatal("expected a JSON type error")
			}
			if got, want := validationFeedback(err), tt.field+": incorrect JSON type; check the schema"; got != want {
				t.Fatalf("validationFeedback = %q, want %q", got, want)
			}
		})
	}
}
