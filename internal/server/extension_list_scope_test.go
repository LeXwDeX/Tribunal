package server

import (
	"net/http"
	"strings"
	"testing"
)

// TestExtensionListScopeSelectorContract pins the shared list selector
// behavior for project_id. The shared validator now rejects a non-positive
// project_id for body writes; list query parsing already rejected it, and this
// test keeps both paths aligned and documented.
func TestExtensionListScopeSelectorContract(t *testing.T) {
	_, srv := newExtensionPaginationServer(t)
	lists := []struct {
		name    string
		path    string
		handler http.HandlerFunc
	}{
		{"mcp-servers", "/api/v1/mcp-servers", srv.handleListMCPServers},
		{"skills", "/api/v1/skills", srv.handleListSkills},
		{"block-bindings", "/api/v1/block-bindings", srv.handleListBlockBindings},
	}
	invalid := []struct {
		name  string
		query string
	}{
		{"zero project id", "?scope=project&project_id=0"},
		{"negative project id", "?scope=project&project_id=-1"},
		{"non-numeric project id", "?scope=project&project_id=nope"},
		{"missing project id", "?scope=project"},
		{"project id with platform scope", "?scope=platform&project_id=42"},
		{"project id without scope", "?project_id=42"},
		{"unknown scope", "?scope=global"},
	}
	for _, list := range lists {
		for _, tc := range invalid {
			t.Run(list.name+" "+tc.name, func(t *testing.T) {
				code, _, text := extensionList(t, list.handler, list.path+tc.query)
				if code != http.StatusBadRequest {
					t.Fatalf("status = %d, want 400: %s", code, text)
				}
				if !strings.Contains(text, `"code":"validation_failed"`) || !strings.Contains(text, `"field":"scope"`) {
					t.Fatalf("error = %s, want validation_failed on scope", text)
				}
			})
		}
	}
}
