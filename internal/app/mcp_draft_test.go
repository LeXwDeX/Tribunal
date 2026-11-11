package app

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/mcp"
)

type recordingMCPSession struct {
	infos    []mcp.ToolInfo
	listErr  error
	closeErr error
	closed   bool
}

func (s *recordingMCPSession) ListTools(context.Context, string) ([]mcp.ToolInfo, error) {
	return s.infos, s.listErr
}

func (s *recordingMCPSession) Close() error {
	s.closed = true
	return s.closeErr
}

func captureMCPTest(t *testing.T, service *ExtensionService, session mcpTestSession, captured *mcp.Options) {
	t.Helper()
	service.newMCPTest = func(opts mcp.Options) (mcpTestSession, error) {
		*captured = opts
		return session, nil
	}
}

func TestExtensionServiceTestMCPDraftDoesNotPersist(t *testing.T) {
	ctx := context.Background()
	service, st := newExtensionFixture(t)
	session := &recordingMCPSession{infos: []mcp.ToolInfo{
		{Name: "echo", Description: "echo text", Bindable: true},
		{Name: "write", ReadOnly: false, Destructive: true, Bindable: false, Annotated: true},
	}}
	var captured mcp.Options
	captureMCPTest(t, service, session, &captured)

	result, err := service.TestMCPDraft(ctx, MCPDraftInput{
		Scope: "platform", Name: "draft",
		Config: json.RawMessage(`{"url":"https://mcp.example.invalid"}`),
	})
	if err != nil {
		t.Fatalf("TestMCPDraft: %v", err)
	}
	if result.ToolCount != 2 || len(result.Tools) != 2 || result.Truncated {
		t.Fatalf("result = %+v", result)
	}
	if got := result.Tools[0]; got.Name != "echo" || !got.Bindable || got.Annotated {
		t.Fatalf("unannotated tool = %+v", got)
	}
	if got := result.Tools[1]; got.Name != "write" || got.Bindable || !got.Destructive || !got.Annotated {
		t.Fatalf("annotated tool = %+v", got)
	}
	if !session.closed {
		t.Fatal("test session was not closed")
	}
	if _, ok := captured.Platform.MCPServers["draft"]; !ok {
		t.Fatalf("draft config was not passed to the manager: %+v", captured.Platform)
	}
	rows, err := st.ListMCPServers(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("draft test persisted rows: %+v", rows)
	}
}

func TestExtensionServiceTestMCPDraftRestoresStoredMaskedConfig(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	stored, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "stored", Enabled: true,
		Config: json.RawMessage(`{"url":"https://mcp.example.invalid","headers":{"Authorization":"Bearer stored-secret","X-Trace":"old"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var captured mcp.Options
	captureMCPTest(t, service, &recordingMCPSession{}, &captured)

	_, err = service.TestMCPDraft(ctx, MCPDraftInput{
		ID: stored.ID, Config: json.RawMessage(`{"url":"https://mcp.example.invalid","headers":{"Authorization":"****","X-Trace":"next"}}`),
	})
	if err != nil {
		t.Fatalf("TestMCPDraft: %v", err)
	}
	headers := captured.Platform.MCPServers["stored"].Headers
	if headers["Authorization"] != "Bearer stored-secret" || headers["X-Trace"] != "next" {
		t.Fatalf("headers = %+v", headers)
	}
}

func TestExtensionServiceTestMCPDraftRejectsMaskedSecretWithoutStoredValue(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	stored, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "platform", Name: "stored", Enabled: true,
		Config: json.RawMessage(`{"command":"npx"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	started := false
	service.newMCPTest = func(mcp.Options) (mcpTestSession, error) {
		started = true
		return &recordingMCPSession{}, nil
	}
	_, err = service.TestMCPDraft(ctx, MCPDraftInput{
		ID: stored.ID, Config: json.RawMessage(`{"command":"npx","env":{"NEW_KEY":"****"}}`),
	})
	var appErr *Error
	if !errors.As(err, &appErr) || appErr.Code != "validation_failed" || appErr.Details[0].Field != "config" {
		t.Fatalf("error = %v", err)
	}
	if started {
		t.Fatal("invalid masked config started a connection")
	}
}

func TestExtensionServiceTestMCPDraftUsesStoredProjectScope(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	projectID := int64(42)
	stored, err := service.CreateMCPServer(ctx, MCPServerInput{
		Scope: "project", ProjectID: &projectID, Name: "project-server", Enabled: true,
		Config: json.RawMessage(`{"url":"https://mcp.example.invalid"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	var captured mcp.Options
	captureMCPTest(t, service, &recordingMCPSession{}, &captured)
	_, err = service.TestMCPDraft(ctx, MCPDraftInput{
		ID: stored.ID, Scope: "platform", Config: json.RawMessage(`{"url":"https://mcp.example.invalid"}`),
	})
	if err != nil {
		t.Fatalf("TestMCPDraft: %v", err)
	}
	if !strings.Contains(captured.Workspace, "/42") {
		t.Fatalf("workspace = %q, want the project workspace", captured.Workspace)
	}
	if _, ok := captured.Platform.MCPServers["project-server"]; !ok {
		t.Fatalf("stored name was not used: %+v", captured.Platform)
	}
}

func TestExtensionServiceTestMCPServerTruncatesTools(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	infos := make([]mcp.ToolInfo, 0, maxTestTools+1)
	for i := 0; i < maxTestTools+1; i++ {
		infos = append(infos, mcp.ToolInfo{Name: "tool", Bindable: true})
	}
	var captured mcp.Options
	captureMCPTest(t, service, &recordingMCPSession{infos: infos}, &captured)
	result, err := service.TestMCPServer(ctx, MCPServer{
		Name: "remote", Config: json.RawMessage(`{"url":"https://mcp.example.invalid"}`),
	})
	if err != nil {
		t.Fatalf("TestMCPServer: %v", err)
	}
	if result.ToolCount != maxTestTools+1 || len(result.Tools) != maxTestTools || !result.Truncated {
		t.Fatalf("result count=%d tools=%d truncated=%t", result.ToolCount, len(result.Tools), result.Truncated)
	}
}

func TestExtensionServiceTestMCPServerClassesFailures(t *testing.T) {
	ctx := context.Background()
	service, _ := newExtensionFixture(t)
	config := json.RawMessage(`{"url":"https://mcp.example.invalid"}`)

	t.Run("invalid config", func(t *testing.T) {
		_, err := service.TestMCPServer(ctx, MCPServer{Name: "remote", Config: json.RawMessage(`{"url":"${MCP_DRAFT_MISSING}"}`)})
		if got := mcp.TestCategory(err); got != mcp.TestCategoryInvalidConfig {
			t.Fatalf("category = %q, err = %v", got, err)
		}
	})

	t.Run("list failure preserved", func(t *testing.T) {
		var captured mcp.Options
		captureMCPTest(t, service, &recordingMCPSession{
			listErr: mcp.NewTestError(mcp.TestCategoryListToolsTimeout, mcp.ErrListToolsTimeout),
		}, &captured)
		_, err := service.TestMCPServer(ctx, MCPServer{Name: "remote", Config: config})
		if got := mcp.TestCategory(err); got != mcp.TestCategoryListToolsTimeout {
			t.Fatalf("category = %q, err = %v", got, err)
		}
	})

	t.Run("cleanup failure", func(t *testing.T) {
		var captured mcp.Options
		captureMCPTest(t, service, &recordingMCPSession{closeErr: errors.New("private close detail")}, &captured)
		_, err := service.TestMCPServer(ctx, MCPServer{Name: "remote", Config: config})
		if got := mcp.TestCategory(err); got != mcp.TestCategoryCleanupFailed {
			t.Fatalf("category = %q, err = %v", got, err)
		}
		if strings.Contains(err.Error(), "private close detail") {
			t.Fatalf("cleanup error leaked detail: %v", err)
		}
	})
}
