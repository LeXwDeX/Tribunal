package mcp

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseValidConfig(t *testing.T) {
	t.Setenv("MCP_TEST_COMMAND", "mcp-helper")
	cfg, err := Parse([]byte(`{
		"mcpServers": {
			"local": {
				"command": "${MCP_TEST_COMMAND}",
				"args": ["--stdio"],
				"env": {"PLAIN": "value"},
				"x-review": {
					"index_command": ["mcp-helper", "cli", "{\"repo_path\":\"{base_dir}\"}"],
					"env_allowlist": ["PATH", "CUSTOM"],
					"home": "{workspace}/index/local",
					"call_timeout": "90s",
					"max_output_chars": 1234,
					"idle_timeout": "45s"
				}
			},
			"remote": {
				"type": "http",
				"url": "https://mcp.example.test/endpoint",
				"headers": {"Authorization": "Bearer ${MCP_TEST_COMMAND}"}
			}
		}
	}`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	local := cfg.MCPServers["local"]
	if local.Command != "mcp-helper" {
		t.Fatalf("command = %q", local.Command)
	}
	if local.XReview.CallTimeout != Duration(90*time.Second) {
		t.Fatalf("call timeout = %v", local.XReview.CallTimeout)
	}
	if local.XReview.IdleTimeout != Duration(45*time.Second) {
		t.Fatalf("idle timeout = %v", local.XReview.IdleTimeout)
	}
	if local.XReview.MaxOutputChars != 1234 {
		t.Fatalf("max output = %d", local.XReview.MaxOutputChars)
	}
	remote := cfg.MCPServers["remote"]
	if remote.XReview.CallTimeout != Duration(DefaultCallTimeout) {
		t.Fatalf("default call timeout = %v", remote.XReview.CallTimeout)
	}
	if remote.XReview.IdleTimeout != Duration(DefaultIdleTimeout) {
		t.Fatalf("default idle timeout = %v", remote.XReview.IdleTimeout)
	}
	if remote.XReview.MaxOutputChars != DefaultMaxOutputChars {
		t.Fatalf("default max output = %d", remote.XReview.MaxOutputChars)
	}
}

func TestParseRejectsInvalidDocuments(t *testing.T) {
	cases := []struct {
		name string
		data string
	}{
		{"empty", ""},
		{"missing servers", `{}`},
		{"null servers", `{"mcpServers":null}`},
		{"unknown top field", `{"mcpServers":{},"other":1}`},
		{"unknown server field", `{"mcpServers":{"a":{"command":"x","bogus":1}}}`},
		{"unknown review field", `{"mcpServers":{"a":{"command":"x","x-review":{"bogus":1}}}}`},
		{"trailing data", `{"mcpServers":{}} {}`},
		{"bad duration", `{"mcpServers":{"a":{"command":"x","x-review":{"call_timeout":"soon"}}}}`},
		{"numeric duration", `{"mcpServers":{"a":{"command":"x","x-review":{"call_timeout":60}}}}`},
		{"http credentials", `{"mcpServers":{"a":{"type":"http","url":"https://user:pass@example.test/x"}}}`},
		{"http fragment", `{"mcpServers":{"a":{"type":"http","url":"https://example.test/x#frag"}}}`},
		{"stdio headers", `{"mcpServers":{"a":{"command":"x","headers":{"X":"y"}}}}`},
		{"http env", `{"mcpServers":{"a":{"type":"http","url":"https://example.test/x","env":{"A":"b"}}}}`},
		{"bad name", `{"mcpServers":{"bad/name":{"command":"x"}}}`},
		{"double underscore name", `{"mcpServers":{"bad__name":{"command":"x"}}}`},
		{"missing command", `{"mcpServers":{"a":{"type":"stdio"}}}`},
		{"bad type", `{"mcpServers":{"a":{"type":"sse","url":"https://example.test/x"}}}`},
		{"call timeout too small", `{"mcpServers":{"a":{"command":"x","x-review":{"call_timeout":"1ns"}}}}`},
		{"idle timeout too small", `{"mcpServers":{"a":{"command":"x","x-review":{"idle_timeout":"1ns"}}}}`},
		{"output too large", `{"mcpServers":{"a":{"command":"x","x-review":{"max_output_chars":2000000}}}}`},
		{"empty index command", `{"mcpServers":{"a":{"command":"x","x-review":{"index_command":[""]}}}}`},
		{"explicit home", `{"mcpServers":{"a":{"command":"x","env":{"HOME":"/tmp"}}}}`},
		{"explicit loader", `{"mcpServers":{"a":{"command":"x","env":{"LD_PRELOAD":"/tmp/evil.so"}}}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.data))
			if err == nil {
				t.Fatal("Parse succeeded, want error")
			}
			if !errors.Is(err, ErrInvalidConfig) && !errors.Is(err, ErrUnsafeEnv) {
				t.Fatalf("error = %v, want ErrInvalidConfig", err)
			}
		})
	}
}

func TestParseOversizedDocument(t *testing.T) {
	data := strings.Repeat(" ", MaxConfigBytes+1)
	if _, err := Parse([]byte(data)); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("error = %v", err)
	}
}

func TestParseMissingEnvironmentDoesNotEchoValue(t *testing.T) {
	_, err := ParseWithEnv([]byte(`{"mcpServers":{"a":{"command":"${MCP_MISSING_NAME}","env":{"SECRET":"SUPER-SECRET-VALUE"}}}}`), func(name string) (string, bool) {
		return "", false
	})
	if err == nil {
		t.Fatal("Parse succeeded")
	}
	if !strings.Contains(err.Error(), "MCP_MISSING_NAME") {
		t.Fatalf("missing variable not named: %v", err)
	}
	if strings.Contains(err.Error(), "SUPER-SECRET-VALUE") {
		t.Fatalf("error leaked a configured value: %v", err)
	}
}

func TestConfigErrorsDoNotEchoSecrets(t *testing.T) {
	_, err := Parse([]byte(`{"mcpServers":{"a":{"command":"x","headers":{"Authorization":"Bearer TOPSECRETVALUE"}}}}`))
	if err == nil {
		t.Fatal("Parse succeeded")
	}
	if strings.Contains(err.Error(), "TOPSECRETVALUE") {
		t.Fatalf("error leaked a header value: %v", err)
	}
}

func TestParseServerDefaultsAndValidation(t *testing.T) {
	cfg, err := ParseServer([]byte(`{"command":"/bin/true"}`))
	if err != nil {
		t.Fatalf("ParseServer: %v", err)
	}
	if cfg.XReview.CallTimeout != Duration(DefaultCallTimeout) || cfg.XReview.IdleTimeout != Duration(DefaultIdleTimeout) {
		t.Fatalf("defaults = %+v", cfg.XReview)
	}
	if _, err := ParseServer([]byte(`{"url":"file:///tmp/socket"}`)); err == nil {
		t.Fatal("ParseServer accepted file URL")
	}
}

func TestMergeProjectOverridesByName(t *testing.T) {
	platform := Config{MCPServers: map[string]ServerConfig{
		"shared": {Command: "platform", Env: map[string]string{"A": "1"}},
		"keep":   {Command: "keep"},
	}}
	project := Config{MCPServers: map[string]ServerConfig{
		"shared": {Command: "project", Args: []string{"--project"}},
	}}
	merged := Merge(platform, project)
	if merged.MCPServers["shared"].Command != "project" {
		t.Fatalf("project server did not replace platform: %+v", merged.MCPServers["shared"])
	}
	if len(merged.MCPServers["shared"].Env) != 0 {
		t.Fatalf("nested env must not merge: %+v", merged.MCPServers["shared"])
	}
	if merged.MCPServers["keep"].Command != "keep" {
		t.Fatal("unrelated platform server was lost")
	}
	merged.MCPServers["keep"] = ServerConfig{Command: "mutated"}
	if platform.MCPServers["keep"].Command != "keep" {
		t.Fatal("Merge aliased the platform map")
	}
}

func TestFingerprintStableAcrossMapOrder(t *testing.T) {
	a := ServerConfig{Command: "x", Env: map[string]string{"A": "1", "B": "2"}}
	b := ServerConfig{Command: "x", Env: map[string]string{"B": "2", "A": "1"}}
	if fingerprint(a) != fingerprint(b) {
		t.Fatal("fingerprint depends on map iteration order")
	}
	c := ServerConfig{Command: "y", Env: a.Env}
	if fingerprint(a) == fingerprint(c) {
		t.Fatal("fingerprint ignored a configuration change")
	}
}

func TestExpandRejectsSensitiveEnvironmentReferences(t *testing.T) {
	lookup := func(name string) (string, bool) {
		switch name {
		case "MCP_TEST_COMMAND":
			return "mcp-helper", true
		case "GITLAB_TOKEN", "OPENAI_API_KEY", "CUSTOM_PROJECT_TOKEN":
			return "secret-value", true
		}
		return "", false
	}
	cases := []struct {
		name   string
		server string
		denied []string
		vars   []string
	}{
		{
			name:   "env value",
			server: `{"command":"x","env":{"TOKEN":"${GITLAB_TOKEN}"}}`,
			vars:   []string{"GITLAB_TOKEN"},
		},
		{
			name:   "header value",
			server: `{"type":"http","url":"https://example.test/x","headers":{"Authorization":"Bearer ${OPENAI_API_KEY}"}}`,
			vars:   []string{"OPENAI_API_KEY"},
		},
		{
			name:   "argument",
			server: `{"command":"x","args":["--key","${OPENAI_API_KEY}"]}`,
			vars:   []string{"OPENAI_API_KEY"},
		},
		{
			name:   "index command",
			server: `{"command":"x","x-review":{"index_command":["tool","${GITLAB_TOKEN}"]}}`,
			vars:   []string{"GITLAB_TOKEN"},
		},
		{
			name:   "denied name",
			server: `{"command":"x","args":["${CUSTOM_PROJECT_TOKEN}"]}`,
			denied: []string{"CUSTOM_PROJECT_TOKEN"},
			vars:   []string{"CUSTOM_PROJECT_TOKEN"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data := []byte(`{"mcpServers":{"a":` + tc.server + `}}`)
			_, err := ParseWithEnv(data, lookup, tc.denied...)
			if !errors.Is(err, ErrSensitiveEnv) {
				t.Fatalf("error = %v, want ErrSensitiveEnv", err)
			}
			if !errors.Is(err, ErrInvalidConfig) {
				t.Fatalf("error = %v, want ErrInvalidConfig wrapper", err)
			}
			for _, name := range tc.vars {
				if !strings.Contains(err.Error(), name) {
					t.Fatalf("error does not name %s: %v", name, err)
				}
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaked the secret value: %v", err)
			}
		})
	}
}

func TestParseExpandsNonSensitiveEnvironment(t *testing.T) {
	cfg, err := ParseWithEnv([]byte(`{
		"mcpServers": {
			"a": {"command":"x","args":["${MCP_TEST_COMMAND}"]},
			"b": {"type":"http","url":"https://example.test/${MCP_TEST_COMMAND}"}
		}
	}`), func(name string) (string, bool) {
		if name == "MCP_TEST_COMMAND" {
			return "mcp-helper", true
		}
		return "", false
	})
	if err != nil {
		t.Fatalf("ParseWithEnv: %v", err)
	}
	if got := cfg.MCPServers["a"].Args[0]; got != "mcp-helper" {
		t.Fatalf("argument = %q", got)
	}
	if got := cfg.MCPServers["b"].URL; got != "https://example.test/mcp-helper" {
		t.Fatalf("url = %q", got)
	}
}

func TestValidateServerJSONPreservesTemplates(t *testing.T) {
	data := []byte(`{"command":"${MCP_COMMAND}","env":{"SAFE":"${SAFE_VALUE}"}}`)
	if err := ValidateServerJSON("local", data); err != nil {
		t.Fatalf("ValidateServerJSON: %v", err)
	}
}

func TestValidateServerJSONRejectsInvalidEntries(t *testing.T) {
	cases := []struct {
		name    string
		server  string
		denied  []string
		wantErr error
	}{
		{name: "empty", server: ``, wantErr: ErrInvalidConfig},
		{name: "unknown field", server: `{"command":"x","bogus":1}`, wantErr: ErrInvalidConfig},
		{name: "unknown x-review field", server: `{"command":"x","x-review":{"bogus":1}}`, wantErr: ErrInvalidConfig},
		{name: "stdio without command", server: `{"type":"stdio"}`, wantErr: ErrInvalidConfig},
		{name: "http without url", server: `{"type":"http","url":""}`, wantErr: ErrInvalidConfig},
		{name: "sensitive template", server: `{"command":"x","args":["${OPENAI_API_KEY}"]}`, wantErr: ErrSensitiveEnv},
		{name: "denied template", server: `{"command":"x","env":{"CUSTOM":"${CUSTOM_DENIED_FLAG}"}}`, denied: []string{"CUSTOM_DENIED_FLAG"}, wantErr: ErrSensitiveEnv},
		{name: "denied variable", server: `{"command":"x","env":{"CUSTOM_DENIED_FLAG":"value"}}`, denied: []string{"CUSTOM_DENIED_FLAG"}, wantErr: ErrInvalidConfig},
		{name: "malformed template", server: `{"command":"x","args":["${BAD-NAME}"]}`, wantErr: ErrInvalidConfig},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateServerJSON("local", []byte(tc.server), tc.denied...)
			if err == nil {
				t.Fatal("ValidateServerJSON accepted an invalid entry")
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("error = %v, want %v", err, tc.wantErr)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("error leaked a secret value: %v", err)
			}
		})
	}
}

func TestValidateServerName(t *testing.T) {
	for _, name := range []string{"local", "remote-1", "a.b_c"} {
		if err := ValidateServerName(name); err != nil {
			t.Fatalf("ValidateServerName(%q): %v", name, err)
		}
	}
	for _, name := range []string{"", "bad/name", "bad__name", strings.Repeat("a", 65)} {
		if err := ValidateServerName(name); !errors.Is(err, ErrInvalidConfig) {
			t.Fatalf("ValidateServerName(%q) = %v", name, err)
		}
	}
}
