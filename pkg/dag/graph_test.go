package dag

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func baseSpec(nodes ...NodeSpec) GraphSpec {
	return GraphSpec{Nodes: nodes}
}

func node(id string, deps ...string) NodeSpec {
	return NodeSpec{ID: id, Kind: "test", Required: true, DependsOn: deps}
}

func TestDecodeGraphStrict(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{name: "valid", input: `{"nodes":[{"id":"a","kind":"x","required":true}]}`},
		{name: "unknown top field", input: `{"nodes":[{"id":"a","kind":"x"}],"extra":1}`, wantErr: true},
		{name: "unknown node field", input: `{"nodes":[{"id":"a","kind":"x","bogus":true}]}`, wantErr: true},
		{name: "trailing object", input: `{"nodes":[{"id":"a","kind":"x"}]}{"nodes":[]}`, wantErr: true},
		{name: "trailing garbage", input: `{"nodes":[{"id":"a","kind":"x"}]}xyz`, wantErr: true},
		{name: "empty", input: ``, wantErr: true},
		{name: "null", input: `null`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := DecodeGraph(strings.NewReader(tc.input))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got %+v", spec)
				}
				if !errors.Is(err, ErrInvalidGraph) {
					t.Fatalf("error %v does not wrap ErrInvalidGraph", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
	if _, err := DecodeGraph(nil); err == nil {
		t.Fatal("nil reader must fail")
	}
}

func TestDecodeGraphBytes(t *testing.T) {
	if _, err := DecodeGraphBytes([]byte(`{"nodes":[{"id":"a","kind":"x"}]}`)); err != nil {
		t.Fatal(err)
	}
}

func TestValidateGraphValid(t *testing.T) {
	spec := GraphSpec{
		Nodes: []NodeSpec{
			node("plan"),
			node("rv.std.p1", "plan"),
			node("gather", "rv.std.p1"),
		},
		Budget: Budget{MaxNodes: 10, MaxExtends: 2},
	}
	if err := ValidateGraph(spec); err != nil {
		t.Fatal(err)
	}
}

func TestValidateGraphErrors(t *testing.T) {
	diamond := []NodeSpec{node("a"), node("b", "a"), node("c", "a"), node("d", "b", "c")}
	tests := []struct {
		name string
		spec GraphSpec
	}{
		{name: "no nodes", spec: baseSpec()},
		{name: "negative max nodes", spec: GraphSpec{Nodes: diamond, Budget: Budget{MaxNodes: -1}}},
		{name: "negative max extends", spec: GraphSpec{Nodes: diamond, Budget: Budget{MaxExtends: -1}}},
		{name: "negative deadline", spec: GraphSpec{Nodes: diamond, Budget: Budget{Deadline: -time.Second}}},
		{name: "negative max tokens", spec: GraphSpec{Nodes: diamond, Budget: Budget{MaxTokens: -1}}},
		{name: "node limit", spec: GraphSpec{Nodes: diamond, Budget: Budget{MaxNodes: 2}}},
		{name: "empty id", spec: baseSpec(node(""))},
		{name: "bad id start", spec: baseSpec(node("-a"))},
		{name: "bad id space", spec: baseSpec(node("a b"))},
		{name: "reserved id", spec: baseSpec(node("a.output.b"))},
		{name: "duplicate id", spec: baseSpec(node("a"), node("a"))},
		{name: "empty kind", spec: baseSpec(NodeSpec{ID: "a"})},
		{name: "invalid params", spec: baseSpec(NodeSpec{ID: "a", Kind: "x", Params: []byte("{")})},
		{name: "missing dep", spec: baseSpec(node("a", "ghost"))},
		{name: "self dep", spec: baseSpec(node("a", "a"))},
		{name: "duplicate dep", spec: baseSpec(node("a"), node("b", "a", "a"))},
		{name: "cycle", spec: baseSpec(node("a", "c"), node("b", "a"), node("c", "b"))},
		{name: "bad condition", spec: baseSpec(node("a"), NodeSpec{ID: "b", Kind: "x", DependsOn: []string{"a"}, Condition: "a.output.x"})},
		{name: "unknown condition node", spec: baseSpec(node("a"), NodeSpec{ID: "b", Kind: "x", Condition: "ghost.output.x == 1"})},
		{name: "condition unreachable", spec: baseSpec(node("a"), node("b"), NodeSpec{ID: "c", Kind: "x", DependsOn: []string{"a"}, Condition: "b.output.x == 1"})},
		{name: "empty input name", spec: baseSpec(node("a"), NodeSpec{ID: "b", Kind: "x", DependsOn: []string{"a"}, InputMapping: map[string]string{"": "a.output.x"}})},
		{name: "bad input ref", spec: baseSpec(node("a"), NodeSpec{ID: "b", Kind: "x", DependsOn: []string{"a"}, InputMapping: map[string]string{"x": "a.out.x"}})},
		{name: "unknown input node", spec: baseSpec(node("a"), NodeSpec{ID: "b", Kind: "x", DependsOn: []string{"a"}, InputMapping: map[string]string{"x": "ghost.output.x"}})},
		{name: "input unreachable", spec: baseSpec(node("a"), node("b"), NodeSpec{ID: "c", Kind: "x", DependsOn: []string{"a"}, InputMapping: map[string]string{"x": "b.output.x"}})},
		{name: "bad output path", spec: baseSpec(node("a"), NodeSpec{ID: "b", Kind: "x", DependsOn: []string{"a"}, InputMapping: map[string]string{"x": "a.output."}})},
		{name: "bad path segment", spec: baseSpec(node("a"), NodeSpec{ID: "b", Kind: "x", DependsOn: []string{"a"}, InputMapping: map[string]string{"x": "a.output.a..b"}})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateGraph(tc.spec)
			if err == nil {
				t.Fatal("want error")
			}
			if !errors.Is(err, ErrInvalidGraph) && !errors.Is(err, ErrBudgetExceeded) {
				t.Fatalf("error %v has no sentinel", err)
			}
		})
	}
}

func TestSplitOutputRefUsesLongestKnownNode(t *testing.T) {
	ids := map[string]bool{"a": true, "rv": true, "rv.std.p1": true}
	nodeID, path, err := splitOutputRef("rv.std.p1.output.findings", ids)
	if err != nil {
		t.Fatal(err)
	}
	if nodeID != "rv.std.p1" || path != "findings" {
		t.Fatalf("got %q %q", nodeID, path)
	}
	if _, _, err := splitOutputRef("rv.output.findings", ids); err != nil {
		t.Fatal(err)
	}
	if _, _, err := splitOutputRef("rv.std.p1.output", ids); err == nil {
		t.Fatal("empty path must fail")
	}
	if _, _, err := splitOutputRef("nope.output.x", ids); err == nil {
		t.Fatal("unknown node must fail")
	}
}

func TestConditionPrecedenceAndLiterals(t *testing.T) {
	wf := WorkflowView{Nodes: map[string]NodeView{
		"a": {ID: "a", Status: NodeCompleted, Output: []byte(`{"x":1,"name":"ok"}`)},
		"b": {ID: "b", Status: NodeCompleted, Output: []byte(`{"x":2}`)},
		"c": {ID: "c", Status: NodeCompleted, Output: []byte(`{"x":3}`)},
	}}
	tests := []struct {
		expr string
		want bool
	}{
		{"a.output.x == 1", true},
		{"a.output.x != 1", false},
		{"a.output.x == 1.0", true},
		{`a.output.name == "ok"`, true},
		{`a.output.name != "no"`, true},
		{"a.output.x == 1 && b.output.x == 2", true},
		{"a.output.x == 9 && b.output.x == 2", false},
		{"a.output.x == 9 || b.output.x == 2", true},
		// && binds tighter: true || (false && false) == true
		{"a.output.x == 1 || b.output.x == 9 && c.output.x == 9", true},
		{"a.output.x == 9 || b.output.x == 9 && c.output.x == 3", false},
		{"a.output.missing == 1", false},
		{"a.output.missing != 1", false},
		{"a.output.x == [1,2]", false},
	}
	for _, tc := range tests {
		if got := evalCondition(tc.expr, wf); got != tc.want {
			t.Errorf("%q = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestLiteralEqualNestedAndInvalid(t *testing.T) {
	if !literalEqual(map[string]any{"a": []any{jsonNumber("1"), "x"}}, map[string]any{"a": []any{jsonNumber("1.0"), "x"}}) {
		t.Fatal("nested numeric equality failed")
	}
	if literalEqual(jsonNumber("abc"), jsonNumber("1")) {
		t.Fatal("invalid number must not be equal")
	}
	if literalEqual(jsonNumber("1"), "1") {
		t.Fatal("number and string must not be equal")
	}
	if literalEqual(nil, false) {
		t.Fatal("nil and false must not be equal")
	}
	if !literalEqual(nil, nil) {
		t.Fatal("nil == nil")
	}
}

func jsonNumber(s string) any {
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var n any
	_ = dec.Decode(&n)
	return n
}
