package config

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"
)

var codebaseMemoryReadTools = []string{
	"list_projects",
	"index_status",
	"search_graph",
	"trace_path",
	"get_code_snippet",
	"query_graph",
	"get_graph_schema",
	"get_file_outline",
	"get_architecture",
	"search_code",
	"check_index_coverage",
}

// wantCodebaseMemoryTools is the complete shipped authorization matrix. The
// review blocks get the read-only tool set, verify and onboarding get only the
// tools their prompts need, and reasoning/synthesis get no graph tools.
var wantCodebaseMemoryTools = map[string][]string{
	"code-review": codebaseMemoryReadTools,
	"review":      codebaseMemoryReadTools,
	"verify":      {"get_code_snippet", "search_graph", "trace_path", "search_code"},
	"onboarding":  {"get_architecture", "get_graph_schema"},
}

// codebaseMemoryForbiddenTools change or delete indexes and must never be
// granted by a shipped default. The graph-overlay helpers are not part of the
// shipped authorization matrix either.
var codebaseMemoryForbiddenTools = []string{
	"index_repository",
	"delete_project",
	"ingest_traces",
	"manage_adr",
	"detect_changes",
	"compare_graphs",
}

func TestShippedConfigsEnableCodebaseMemoryByDefault(t *testing.T) {
	for _, path := range []string{"../../config/server.example.yaml", dockerConfigPath} {
		t.Run(path, func(t *testing.T) {
			setDockerConfigEnv(t)
			cfg := loadConfig(t, path)
			if err := cfg.Validate(); err != nil {
				t.Fatalf("Validate = %v", err)
			}
			if !reflect.DeepEqual(cfg.DefaultExtensions.Skills, wantDefaultSkills) {
				t.Errorf("default skills = %+v, want %+v", cfg.DefaultExtensions.Skills, wantDefaultSkills)
			}
			if len(cfg.DefaultExtensions.MCPServers) != 1 || cfg.DefaultExtensions.MCPServers[0].Name != "codebase-memory" {
				t.Fatalf("default MCP servers = %+v, want codebase-memory", cfg.DefaultExtensions.MCPServers)
			}
			spec := cfg.DefaultExtensions.MCPServers[0]
			raw, err := spec.ConfigJSON()
			if err != nil {
				t.Fatal(err)
			}
			var server struct {
				Command string   `json:"command"`
				Args    []string `json:"args"`
				XReview struct {
					IndexCommand []string `json:"index_command"`
				} `json:"x-review"`
			}
			if err := json.Unmarshal(raw, &server); err != nil {
				t.Fatal(err)
			}
			if server.Command != "/usr/local/bin/tribunal-codebase-memory-mcp" {
				t.Errorf("command = %q, want the isolating wrapper", server.Command)
			}
			if !slices.Equal(server.Args, []string{"--tool-profile=analysis"}) {
				t.Errorf("args = %q, want only the read-only analysis profile", server.Args)
			}
			index := server.XReview.IndexCommand
			if len(index) < 3 || index[0] != server.Command || index[1] != "cli" ||
				!slices.Contains(index, "index_repository") || !strings.Contains(strings.Join(index, " "), "{base_dir}") {
				t.Errorf("index_command = %q, want the wrapper CLI indexing {base_dir}", index)
			}
			blocks := make([]string, 0, len(spec.Tools))
			for block := range spec.Tools {
				blocks = append(blocks, block)
			}
			slices.Sort(blocks)
			wantBlocks := []string{"code-review", "onboarding", "review", "verify"}
			if !slices.Equal(blocks, wantBlocks) {
				t.Errorf("authorized blocks = %q, want %q", blocks, wantBlocks)
			}
			for block, want := range wantCodebaseMemoryTools {
				if got := spec.Tools[block]; !slices.Equal(got, want) {
					t.Errorf("block %s tools = %q, want %q", block, got, want)
				}
			}
			for block, tools := range spec.Tools {
				for _, tool := range tools {
					if slices.Contains(codebaseMemoryForbiddenTools, tool) {
						t.Errorf("block %s grants forbidden tool %s", block, tool)
					}
				}
			}
		})
	}
}

func TestValidateDefaultMCPServers(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"invalid name", "default_extensions:\n  mcp_servers:\n    - name: \"a__b\"\n      config: { command: /bin/true }\n", "default_extensions.mcp_servers[0].name"},
		{"empty config", "default_extensions:\n  mcp_servers:\n    - name: graph\n", "default_extensions.mcp_servers[0].config"},
		{"unknown block", "default_extensions:\n  mcp_servers:\n    - name: graph\n      config: { command: /bin/true }\n      tools: { plan: [search] }\n", "default_extensions.mcp_servers[0].tools.plan"},
		{"qualified tool", "default_extensions:\n  mcp_servers:\n    - name: graph\n      config: { command: /bin/true }\n      tools: { review: [mcp__graph__search] }\n", "default_extensions.mcp_servers[0].tools.review[0]"},
		{"duplicate", "default_extensions:\n  mcp_servers:\n    - { name: graph, config: { command: /bin/true } }\n    - { name: graph, config: { command: /bin/true } }\n", "default_extensions.mcp_servers[1].name"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setRequiredEnv(t)
			cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab+tt.body))
			requireErrorContains(t, cfg.Validate(), tt.want)
		})
	}
	setRequiredEnv(t)
	cfg := loadConfig(t, writeConfig(t, "config.yaml", validGitLab+
		"default_extensions:\n  mcp_servers:\n    - name: graph\n      config: { command: /bin/true, args: [\"--x\"] }\n      tools: { review: [search_graph] }\n"))
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate(valid default) = %v", err)
	}
	raw, err := cfg.DefaultExtensions.MCPServers[0].ConfigJSON()
	if err != nil || string(raw) != `{"args":["--x"],"command":"/bin/true"}` {
		t.Fatalf("ConfigJSON = %s, %v; want canonical JSON", raw, err)
	}
}
