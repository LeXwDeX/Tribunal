package review

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/agent"
)

func TestIgnoredRenameDestinationIsNotReadableFromSandbox(t *testing.T) {
	head, base := t.TempDir(), t.TempDir()
	for _, root := range []string{head, base} {
		if err := os.WriteFile(filepath.Join(root, "public.txt"), []byte("CANARY_RENAMED\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "allowed.txt"), []byte("NONDIFF_ALLOWED\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := validDossier(t)
	d.HeadDir, d.BaseDir = head, base
	d.Ignored = []string{"public.txt"}
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{"private/**"}}
	e := newTestExecutor(t, ExecutorDeps{Dossier: d})
	sb, err := e.sandboxFor(nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sb.Close()
	for _, tool := range agent.BuiltinTools(sb) {
		if tool.Def().Name != "read_file" && tool.Def().Name != "grep" {
			continue
		}
		for _, root := range []string{"head", "base"} {
			input := `{"root":"` + root + `","path":"public.txt"}`
			if tool.Def().Name == "grep" {
				input = `{"root":"` + root + `","pattern":"CANARY"}`
			}
			res, err := tool.Call(context.Background(), json.RawMessage(input))
			if strings.Contains(res.Content, "CANARY_RENAMED") || (tool.Def().Name == "read_file" && err == nil) {
				t.Errorf("%s %s leaked %q, err=%v", root, tool.Def().Name, res.Content, err)
			}
			if tool.Def().Name == "read_file" {
				res, err = tool.Call(context.Background(), json.RawMessage(`{"root":"`+root+`","path":"allowed.txt"}`))
				if err != nil || !strings.Contains(res.Content, "NONDIFF_ALLOWED") {
					t.Errorf("allowed = %q %v", res.Content, err)
				}
			}
		}
	}
}
