package server

import (
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestWorkspaceIgnoreIndexStatusIsDegraded(t *testing.T) {
	ws := &store.Workspace{State: "ready", BaseSHA: "same", IndexSHA: "same", LastError: "index: blocked by ignore policy"}
	if got := workspaceIndexStatus(ws); got != "degraded" {
		t.Fatalf("index status = %q", got)
	}
	if got := workspaceLastError(ws); got != "index: blocked by ignore policy" {
		t.Fatalf("last error = %q", got)
	}
	ws.LastError = "index: secret raw error"
	if got := workspaceLastError(ws); got != store.WorkspaceIndexFailed {
		t.Fatalf("raw error leaked = %q", got)
	}
}
