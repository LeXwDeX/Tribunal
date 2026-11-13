package dag

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	defaultMaxNodes   = 30
	defaultMaxExtends = 1
)

var (
	nodeIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	pathPattern   = regexp.MustCompile(`^[A-Za-z0-9_-]+(\.[A-Za-z0-9_-]+)*$`)
)

// NormalizeBudget applies documented defaults to a zero budget.
func NormalizeBudget(b Budget) Budget {
	if b.MaxNodes == 0 {
		b.MaxNodes = defaultMaxNodes
	}
	if b.MaxExtends == 0 {
		b.MaxExtends = defaultMaxExtends
	}
	return b
}

// DecodeGraph decodes one JSON graph reject unknown fields and trailing data.
func DecodeGraph(r io.Reader) (GraphSpec, error) {
	var spec GraphSpec
	if r == nil {
		return spec, &GraphError{Problem: "nil graph reader"}
	}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&spec); err != nil {
		return spec, fmt.Errorf("%w: decode json: %v", ErrInvalidGraph, err)
	}
	if err := ensureEOF(dec); err != nil {
		return spec, fmt.Errorf("%w: %v", ErrInvalidGraph, err)
	}
	return spec, nil
}

// DecodeGraphBytes is the byte-slice form of DecodeGraph.
func DecodeGraphBytes(data []byte) (GraphSpec, error) {
	return DecodeGraph(bytes.NewReader(data))
}

func ensureEOF(dec *json.Decoder) error {
	var extra json.RawMessage
	err := dec.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("trailing json value")
	}
	return fmt.Errorf("trailing data: %v", err)
}

func strictUnmarshal(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	return ensureEOF(dec)
}

// ValidateGraph checks identifiers, dependencies, cycles, budgets, input
// mappings, and condition references. It is the only graph validator.
func ValidateGraph(spec GraphSpec) error {
	if len(spec.Nodes) == 0 {
		return &GraphError{Field: "nodes", Problem: "at least one node is required"}
	}
	if err := validateBudget(spec.Budget, len(spec.Nodes)); err != nil {
		return err
	}

	byID := make(map[string]NodeSpec, len(spec.Nodes))
	for i, node := range spec.Nodes {
		field := fmt.Sprintf("nodes[%d]", i)
		if !nodeIDPattern.MatchString(node.ID) {
			return &GraphError{Field: field + ".id", Problem: fmt.Sprintf("invalid node id %q", node.ID)}
		}
		if strings.Contains(node.ID, ".output.") {
			return &GraphError{Field: field + ".id", Problem: "node id may not contain .output."}
		}
		if _, exists := byID[node.ID]; exists {
			return &GraphError{Field: field + ".id", Problem: fmt.Sprintf("duplicate node id %q", node.ID)}
		}
		if node.Kind == "" {
			return &GraphError{Field: field + ".kind", Problem: "kind is required"}
		}
		// A negative timeout is valid: it disables the node-level deadline
		// and leaves only the workflow deadline (if any).
		if len(node.Params) > 0 && !json.Valid(node.Params) {
			return &GraphError{Field: field + ".params", Problem: "must be valid JSON"}
		}
		byID[node.ID] = node
	}

	for i, node := range spec.Nodes {
		field := fmt.Sprintf("nodes[%d]", i)
		seenDeps := make(map[string]bool, len(node.DependsOn))
		for j, dep := range node.DependsOn {
			if dep == node.ID {
				return &GraphError{Field: fmt.Sprintf("%s.depends_on[%d]", field, j), Problem: "self dependency"}
			}
			if _, ok := byID[dep]; !ok {
				return &GraphError{Field: fmt.Sprintf("%s.depends_on[%d]", field, j), Problem: fmt.Sprintf("missing node %q", dep)}
			}
			if seenDeps[dep] {
				return &GraphError{Field: fmt.Sprintf("%s.depends_on[%d]", field, j), Problem: fmt.Sprintf("duplicate dependency %q", dep)}
			}
			seenDeps[dep] = true
		}
	}

	if err := validateAcyclic(spec.Nodes, byID); err != nil {
		return err
	}

	ids := make(map[string]bool, len(byID))
	for id := range byID {
		ids[id] = true
	}
	for i, node := range spec.Nodes {
		field := fmt.Sprintf("nodes[%d]", i)
		reachable := reachableDependencies(node, byID)
		if node.Condition != "" {
			if _, err := parseCondition(node.Condition, ids, reachable); err != nil {
				return &GraphError{Field: field + ".condition", Problem: err.Error()}
			}
		}
		for name, ref := range node.InputMapping {
			if strings.TrimSpace(name) == "" {
				return &GraphError{Field: field + ".input_mapping", Problem: "empty input name"}
			}
			dep, path, err := splitOutputRef(ref, ids)
			if err != nil {
				return &GraphError{Field: fmt.Sprintf("%s.input_mapping[%q]", field, name), Problem: err.Error()}
			}
			if !reachable[dep] {
				return &GraphError{Field: fmt.Sprintf("%s.input_mapping[%q]", field, name), Problem: fmt.Sprintf("node %q is not a reachable dependency", dep)}
			}
			if !pathPattern.MatchString(path) {
				return &GraphError{Field: fmt.Sprintf("%s.input_mapping[%q]", field, name), Problem: fmt.Sprintf("invalid output path %q", path)}
			}
		}
	}
	return nil
}

func validateBudget(b Budget, nodes int) error {
	if b.MaxNodes < 0 {
		return &GraphError{Field: "budget.max_nodes", Problem: "must not be negative"}
	}
	if b.MaxExtends < 0 {
		return &GraphError{Field: "budget.max_extends", Problem: "must not be negative"}
	}
	if b.Deadline < 0 {
		return &GraphError{Field: "budget.deadline", Problem: "must not be negative"}
	}
	if b.MaxTokens < 0 {
		return &GraphError{Field: "budget.max_tokens", Problem: "must not be negative"}
	}
	if b.MaxNodes > 0 && nodes > b.MaxNodes {
		return fmt.Errorf("%w: graph has %d nodes, max_nodes is %d", ErrBudgetExceeded, nodes, b.MaxNodes)
	}
	return nil
}

func validateAcyclic(nodes []NodeSpec, byID map[string]NodeSpec) error {
	indegree := make(map[string]int, len(nodes))
	next := make(map[string][]string, len(nodes))
	for _, node := range nodes {
		indegree[node.ID] = len(node.DependsOn)
		for _, dep := range node.DependsOn {
			next[dep] = append(next[dep], node.ID)
		}
	}
	queue := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if indegree[node.ID] == 0 {
			queue = append(queue, node.ID)
		}
	}
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		visited++
		for _, child := range next[id] {
			indegree[child]--
			if indegree[child] == 0 {
				queue = append(queue, child)
			}
		}
	}
	if visited != len(byID) {
		var cyclic []string
		for _, node := range nodes {
			if indegree[node.ID] > 0 {
				cyclic = append(cyclic, node.ID)
			}
		}
		return &GraphError{Field: "nodes", Problem: "dependency cycle involving " + strings.Join(cyclic, ", ")}
	}
	return nil
}

func reachableDependencies(node NodeSpec, byID map[string]NodeSpec) map[string]bool {
	seen := make(map[string]bool)
	stack := append([]string(nil), node.DependsOn...)
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if seen[id] {
			continue
		}
		seen[id] = true
		stack = append(stack, byID[id].DependsOn...)
	}
	return seen
}

// splitOutputRef parses <node>.output.<path>. Node IDs may contain dots, so
// every ".output." boundary is tried and the longest known node ID wins.
func splitOutputRef(ref string, ids map[string]bool) (string, string, error) {
	var (
		bestNode string
		bestPath string
		found    bool
	)
	start := 0
	for {
		idx := strings.Index(ref[start:], ".output.")
		if idx < 0 {
			break
		}
		idx += start
		node := ref[:idx]
		path := ref[idx+len(".output."):]
		if ids[node] && len(node) > len(bestNode) {
			bestNode, bestPath, found = node, path, true
		}
		start = idx + len(".output.")
	}
	if !found {
		return "", "", fmt.Errorf("reference %q must be <known-node>.output.<path>", ref)
	}
	if bestPath == "" {
		return "", "", fmt.Errorf("reference %q has an empty output path", ref)
	}
	return bestNode, bestPath, nil
}

// clone helpers keep projected views independent from stored bytes.
func cloneStrings(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func cloneSpec(spec NodeSpec) NodeSpec {
	spec.DependsOn = append([]string(nil), spec.DependsOn...)
	if spec.InputMapping != nil {
		spec.InputMapping = cloneStrings(spec.InputMapping)
	}
	spec.Params = append(json.RawMessage(nil), spec.Params...)
	return spec
}

func cloneGraph(spec GraphSpec) GraphSpec {
	out := GraphSpec{Budget: spec.Budget}
	out.Nodes = make([]NodeSpec, len(spec.Nodes))
	for i, node := range spec.Nodes {
		out.Nodes[i] = cloneSpec(node)
	}
	return out
}

func rawCopy(in json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), in...)
}
