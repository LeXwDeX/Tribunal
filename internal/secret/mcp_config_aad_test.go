package secret

import (
	"bytes"
	"testing"
)

func TestMCPConfigAAD(t *testing.T) {
	id := int64(12)
	platform, err := MCPConfigAAD("platform", nil, "a/12/b")
	if err != nil {
		t.Fatal(err)
	}
	project, err := MCPConfigAAD("project", &id, "a/12/b")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(platform, project) {
		t.Fatal("scope collision")
	}
	for _, tc := range []struct {
		scope   string
		project *int64
		server  string
	}{
		{"platform", &id, "a"}, {"project", nil, "a"}, {"other", nil, "a"}, {"platform", nil, ""},
	} {
		if _, err := MCPConfigAAD(tc.scope, tc.project, tc.server); err == nil {
			t.Fatal("invalid identity accepted")
		}
	}
	id = 0
	if _, err := MCPConfigAAD("project", &id, "a"); err == nil {
		t.Fatal("invalid project accepted")
	}
}
