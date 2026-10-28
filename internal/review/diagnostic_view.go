package review

import (
	"encoding/json"
	"fmt"

	"github.com/LeXwDeX/tribunal/pkg/dag"
)

// SafeDiagnosticOutput preserves node output fields while hiding diagnostics
// copied by older versions from failed verify nodes. It does not edit history.
func SafeDiagnosticOutput(kind string, raw json.RawMessage, wf dag.WorkflowView) (json.RawMessage, error) {
	if kind != KindAdjudicate && kind != KindSynthesize || len(raw) == 0 || string(raw) == "null" {
		return raw, nil
	}
	failures, err := failedVerifyDiagnostics(wf)
	if err != nil {
		return nil, err
	}
	if len(failures) == 0 {
		return raw, nil
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil || doc == nil {
		return nil, fmt.Errorf("invalid diagnostic output")
	}
	if err := safeDiagnosticGroups(doc, failures); err != nil {
		return nil, err
	}
	if data, ok := doc["warnings"]; ok && string(data) != "null" {
		var warnings []string
		if err := json.Unmarshal(data, &warnings); err != nil {
			return nil, err
		}
		for i, warning := range warnings {
			for id, errors := range failures {
				for _, text := range errors {
					if warning == "verify failed for group "+id+": "+text {
						warnings[i] = "verify failed for group " + id + ": node failed"
					}
				}
			}
		}
		doc["warnings"], _ = json.Marshal(warnings)
	}
	if data, ok := doc["output"]; ok && string(data) != "null" {
		var output map[string]json.RawMessage
		if err := json.Unmarshal(data, &output); err != nil || output == nil {
			return nil, fmt.Errorf("invalid synthesis output")
		}
		if data, ok := output["decisions"]; ok && string(data) != "null" {
			var decisions []map[string]json.RawMessage
			if err := json.Unmarshal(data, &decisions); err != nil {
				return nil, err
			}
			for _, decision := range decisions {
				var id string
				if err := json.Unmarshal(decision["finding_id"], &id); err != nil {
					return nil, err
				}
				if err := safeDiagnosticReason(decision, "reason", failures[id], false); err != nil {
					return nil, err
				}
			}
			output["decisions"], _ = json.Marshal(decisions)
		}
		doc["output"], _ = json.Marshal(output)
	}
	return json.Marshal(doc)
}

func failedVerifyDiagnostics(wf dag.WorkflowView) (map[string][]string, error) {
	failures := make(map[string][]string)
	for _, node := range wf.Nodes {
		if node.Spec.Kind != KindVerify || node.Status != dag.NodeFailed || node.Error == "" {
			continue
		}
		var params struct {
			GroupID string `json:"group_id"`
		}
		if json.Unmarshal(node.Spec.Params, &params) != nil || params.GroupID == "" {
			return nil, fmt.Errorf("invalid failed verify parameters")
		}
		failures[params.GroupID] = append(failures[params.GroupID], node.Error)
	}
	return failures, nil
}

// SafeFindingDiagnostics applies the same historical diagnostic view to a
// projected finding without changing its body or stored representation.
func SafeFindingDiagnostics(id string, challenge json.RawMessage, reason string, wf dag.WorkflowView) (json.RawMessage, string, error) {
	failures, err := failedVerifyDiagnostics(wf)
	if err != nil {
		return nil, "", err
	}
	if len(failures[id]) == 0 {
		return challenge, reason, nil
	}
	if len(challenge) != 0 && string(challenge) != "null" {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(challenge, &object); err != nil || object == nil {
			return nil, "", fmt.Errorf("invalid challenge diagnostic")
		}
		if err := safeDiagnosticReason(object, "Reason", failures[id], true); err != nil {
			return nil, "", err
		}
		challenge, _ = json.Marshal(object)
	}
	reasonRaw, _ := json.Marshal(reason)
	value := map[string]json.RawMessage{"Reason": reasonRaw}
	if err := safeDiagnosticReason(value, "Reason", failures[id], false); err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(value["Reason"], &reason); err != nil {
		return nil, "", err
	}
	return challenge, reason, nil
}

func safeDiagnosticGroups(doc map[string]json.RawMessage, failures map[string][]string) error {
	data, ok := doc["groups"]
	if !ok || string(data) == "null" {
		return nil
	}
	var groups []map[string]json.RawMessage
	if err := json.Unmarshal(data, &groups); err != nil {
		return err
	}
	for _, group := range groups {
		var id string
		if err := json.Unmarshal(group["ID"], &id); err != nil {
			return err
		}
		for _, field := range []string{"Challenge", "Decision"} {
			data, ok := group[field]
			if !ok || string(data) == "null" {
				continue
			}
			var value map[string]json.RawMessage
			if err := json.Unmarshal(data, &value); err != nil || value == nil {
				return fmt.Errorf("invalid group diagnostic")
			}
			if err := safeDiagnosticReason(value, "Reason", failures[id], field == "Challenge"); err != nil {
				return err
			}
			group[field], _ = json.Marshal(value)
		}
	}
	doc["groups"], _ = json.Marshal(groups)
	return nil
}

func safeDiagnosticReason(value map[string]json.RawMessage, field string, failures []string, challenge bool) error {
	data, ok := value[field]
	if !ok || string(data) == "null" {
		return nil
	}
	var reason string
	if err := json.Unmarshal(data, &reason); err != nil {
		return err
	}
	for _, text := range failures {
		if challenge && reason == text {
			reason = "node failed"
			break
		}
		if reason == "challenge failed: "+text {
			reason = "challenge failed: node failed"
			break
		}
		for _, policy := range []string{"mark", "drop", "judge"} {
			generated := "challenge failed: " + text + "; on_uncertain=" + policy
			if reason == generated {
				reason = "challenge failed: node failed; on_uncertain=" + policy
				break
			}
			if reason == truncateRunes(generated, MaxReasonRunes) && len([]rune(generated)) > MaxReasonRunes {
				reason = "challenge failed: node failed"
				break
			}
		}
	}
	value[field], _ = json.Marshal(reason)
	return nil
}
