package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/app"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store"
	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/store/sqlite"
)

func multipartBody(t *testing.T, fields map[string]string, fileField, fileName string, fileData []byte) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
	}
	part, err := writer.CreateFormFile(fileField, fileName)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write(fileData); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

func skillZip(t *testing.T, manifest string, includeManifest bool) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	name := "skill/SKILL.md"
	if !includeManifest {
		name = "skill/README.md"
	}
	entry, err := writer.Create(name)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := entry.Write([]byte(manifest)); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buffer.Bytes()
}

const testSkillManifest = "---\nname: go-review\ndescription: Review Go changes\n---\n\n# Body\n"

func seedMCPServer(env *m6Env) {
	env.extensions.servers = append(env.extensions.servers, MCPServer{
		ID:      "mcp-1",
		Scope:   "platform",
		Name:    "github",
		Enabled: true,
		Config: json.RawMessage(`{
			"command":"npx",
			"env":{"API_KEY":"super-secret"},
			"headers":{"Authorization":"Bearer tok","X-Trace":"keep"}
		}`),
		CreatedAt: time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC),
	})
}

func TestMCPServersMaskSecretsOnRead(t *testing.T) {
	env := newM6Env(t, m6Options{})
	seedMCPServer(env)
	client := env.loginAs("code-admin")

	response := client.request(http.MethodGet, "/api/v1/mcp-servers?scope=platform", nil, nil)
	assertStatus(t, response, http.StatusOK)
	text := string(response.body)
	for _, secret := range []string{"super-secret", "Bearer tok"} {
		if strings.Contains(text, secret) {
			t.Fatalf("list leaked %q: %s", secret, text)
		}
	}
	body := env.decode(t, response)
	servers := body["servers"].([]any)
	if len(servers) != 1 {
		t.Fatalf("servers = %v", servers)
	}
	config := servers[0].(map[string]any)["config"].(map[string]any)
	envValues := config["env"].(map[string]any)
	if envValues["API_KEY"] != maskedSecret {
		t.Fatalf("env = %v", envValues)
	}
	headers := config["headers"].(map[string]any)
	if headers["Authorization"] != maskedSecret {
		t.Fatalf("authorization header = %v", headers)
	}
	if headers["X-Trace"] != "keep" {
		t.Fatalf("non-sensitive header was masked: %v", headers)
	}
}

func TestMCPUpdateRestoresMaskedPlaceholders(t *testing.T) {
	env := newM6Env(t, m6Options{})
	seedMCPServer(env)
	client := env.loginAs("code-admin")

	response := client.json(http.MethodPut, "/api/v1/mcp-servers/mcp-1", map[string]any{
		"config": map[string]any{
			"command": "npx",
			"env":     map[string]any{"API_KEY": maskedSecret},
			"headers": map[string]any{"Authorization": maskedSecret, "X-Trace": "next"},
		},
	})
	assertStatus(t, response, http.StatusOK)
	if len(env.extensions.updated) != 1 {
		t.Fatalf("updates = %v", env.extensions.updated)
	}
	var sent map[string]any
	if err := json.Unmarshal(env.extensions.updated[0].Config, &sent); err != nil {
		t.Fatalf("decode saved config: %v", err)
	}
	envValues, _ := sent["env"].(map[string]any)
	if envValues["API_KEY"] != "super-secret" {
		t.Fatalf("masked env value was persisted: %v", sent["env"])
	}
	headers, _ := sent["headers"].(map[string]any)
	if headers["Authorization"] != "Bearer tok" {
		t.Fatalf("masked header value was persisted: %v", sent["headers"])
	}
	if headers["X-Trace"] != "next" {
		t.Fatalf("headers = %v", sent["headers"])
	}
	if strings.Contains(string(env.extensions.updated[0].Config), maskedSecret) {
		t.Fatalf("placeholder persisted: %s", env.extensions.updated[0].Config)
	}
}

func TestMCPServerTestUsesUnmaskedConfig(t *testing.T) {
	env := newM6Env(t, m6Options{})
	seedMCPServer(env)
	env.mcp.tools = []MCPTool{{Name: "ping", Description: "ping tool"}}
	client := env.loginAs("code-admin")

	response := client.request(http.MethodPost, "/api/v1/mcp-servers/mcp-1/test", nil, nil)
	assertStatus(t, response, http.StatusOK)
	tools := env.decode(t, response)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "ping" {
		t.Fatalf("tools = %v", tools)
	}
	if len(env.mcp.servers) != 1 || !strings.Contains(string(env.mcp.servers[0].Config), "super-secret") {
		t.Fatalf("tester did not receive the stored config: %v", env.mcp.servers)
	}

	env.mcp.err = io.ErrUnexpectedEOF
	failed := client.request(http.MethodPost, "/api/v1/mcp-servers/mcp-1/test", nil, nil)
	assertStatus(t, failed, http.StatusInternalServerError)
	if body := env.decode(t, failed)["error"].(map[string]any); body["code"] != "internal" {
		t.Fatalf("error = %v", body)
	}
}

func TestMCPImportDocument(t *testing.T) {
	env := newM6Env(t, m6Options{})
	env.grant(7, 42, store.ProjectAccessManage)
	client := env.loginAs("code-admin")

	body := []byte(`{"mcpServers":{"alpha":{"command":"npx"},"beta":{"url":"https://mcp.example.com"}}}`)
	response := client.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=project&project_id=42", body,
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, response, http.StatusCreated)
	servers := env.decode(t, response)["servers"].([]any)
	if len(servers) != 2 {
		t.Fatalf("imported = %v", servers)
	}
	if len(env.extensions.imports) != 1 {
		t.Fatalf("imports = %v", env.extensions.imports)
	}
	imported := env.extensions.imports[0]
	if imported.Scope != "project" || imported.ProjectID == nil || *imported.ProjectID != 42 {
		t.Fatalf("import scope = %+v", imported)
	}
	if string(imported.Servers["alpha"]) != `{"command":"npx"}` {
		t.Fatalf("imported config = %s", imported.Servers["alpha"])
	}

	missingProject := client.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=project", body,
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, missingProject, http.StatusBadRequest)

	wrapper := []byte(`{"servers":{"alpha":{"command":"npx"}}}`)
	rejected := client.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform", wrapper,
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, rejected, http.StatusBadRequest)

	badConfig := []byte(`{"mcpServers":{"alpha":[]}}`)
	bad := client.request(http.MethodPost, "/api/v1/mcp-servers/import?scope=platform", badConfig,
		map[string]string{"Content-Type": "application/json"})
	assertStatus(t, bad, http.StatusBadRequest)
}

func TestSkillUploadValidation(t *testing.T) {
	env := newM6Env(t, m6Options{})
	env.grant(7, 42, store.ProjectAccessManage)
	client := env.loginAs("code-admin")

	noFile, contentType := multipartBody(t, map[string]string{"scope": "platform"}, "file", "x.zip", nil)
	missing := client.request(http.MethodPost, "/api/v1/skills", noFile, map[string]string{"Content-Type": contentType})
	assertStatus(t, missing, http.StatusBadRequest)

	notZip, contentType := multipartBody(t, map[string]string{"scope": "platform"}, "file", "x.zip", []byte("not a zip"))
	invalid := client.request(http.MethodPost, "/api/v1/skills", notZip, map[string]string{"Content-Type": contentType})
	assertStatus(t, invalid, http.StatusBadRequest)

	noManifest, contentType := multipartBody(t, map[string]string{"scope": "platform"}, "file", "x.zip",
		skillZip(t, "hello", false))
	missingManifest := client.request(http.MethodPost, "/api/v1/skills", noManifest, map[string]string{"Content-Type": contentType})
	assertStatus(t, missingManifest, http.StatusBadRequest)

	archive, contentType := multipartBody(t, map[string]string{"scope": "platform"}, "file", "x.zip",
		skillZip(t, testSkillManifest, true))
	created := client.request(http.MethodPost, "/api/v1/skills", archive, map[string]string{"Content-Type": contentType})
	assertStatus(t, created, http.StatusCreated)
	body := env.decode(t, created)
	if body["name"] != "go-review" || body["description"] != "Review Go changes" {
		t.Fatalf("skill = %v", body)
	}
	if len(env.extensions.uploads) != 1 || env.extensions.uploads[0].Name != "go-review" ||
		len(env.extensions.uploads[0].Data) == 0 {
		t.Fatalf("uploads = %+v", env.extensions.uploads)
	}

	projectArchive, contentType := multipartBody(t, map[string]string{"scope": "project", "project_id": "42"},
		"file", "x.zip", skillZip(t, testSkillManifest, true))
	projectCreated := client.request(http.MethodPost, "/api/v1/skills", projectArchive,
		map[string]string{"Content-Type": contentType})
	assertStatus(t, projectCreated, http.StatusCreated)
	if env.extensions.uploads[1].ProjectID == nil || *env.extensions.uploads[1].ProjectID != 42 {
		t.Fatalf("project upload = %+v", env.extensions.uploads[1])
	}

	missingScope, contentType := multipartBody(t, map[string]string{}, "file", "x.zip", skillZip(t, testSkillManifest, true))
	noScope := client.request(http.MethodPost, "/api/v1/skills", missingScope, map[string]string{"Content-Type": contentType})
	assertStatus(t, noScope, http.StatusBadRequest)
}

func TestSkillDirectoryRegistration(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")

	relative := client.json(http.MethodPost, "/api/v1/skills",
		map[string]any{"scope": "platform", "dir": "skills/go"})
	assertStatus(t, relative, http.StatusBadRequest)

	dir := t.TempDir()
	empty := client.json(http.MethodPost, "/api/v1/skills",
		map[string]any{"scope": "platform", "dir": dir})
	assertStatus(t, empty, http.StatusBadRequest)

	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(testSkillManifest), 0o600); err != nil {
		t.Fatal(err)
	}
	created := client.json(http.MethodPost, "/api/v1/skills",
		map[string]any{"scope": "platform", "dir": dir})
	assertStatus(t, created, http.StatusCreated)
	if len(env.extensions.dirs) != 1 || env.extensions.dirs[0].Dir != dir {
		t.Fatalf("registered dirs = %+v", env.extensions.dirs)
	}

	deleted := client.request(http.MethodDelete, "/api/v1/skills/skill-dir", nil, nil)
	assertStatus(t, deleted, http.StatusNoContent)
}

func TestBlockBindingUpsert(t *testing.T) {
	env := newM6Env(t, m6Options{})
	client := env.loginAs("code-admin")

	response := client.json(http.MethodPut, "/api/v1/block-bindings/review", map[string]any{
		"scope":     "platform",
		"mcp_tools": []string{"mcp__github__search"},
		"skills":    []string{"go-review"},
	})
	assertStatus(t, response, http.StatusOK)
	body := env.decode(t, response)
	if body["block"] != "review" || body["scope"] != "platform" {
		t.Fatalf("binding = %v", body)
	}
	tools := body["mcp_tools"].([]any)
	if len(tools) != 1 || tools[0] != "mcp__github__search" {
		t.Fatalf("tools = %v", tools)
	}
	if len(env.extensions.bindings) != 1 {
		t.Fatalf("bindings = %v", env.extensions.bindings)
	}

	list := client.request(http.MethodGet, "/api/v1/block-bindings?scope=platform", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if len(env.decode(t, list)["bindings"].([]any)) != 1 {
		t.Fatalf("list = %s", list.body)
	}

	badBlock := client.json(http.MethodPut, "/api/v1/block-bindings/Review", map[string]any{
		"scope": "platform",
	})
	assertStatus(t, badBlock, http.StatusBadRequest)

	projectWithoutID := client.json(http.MethodPut, "/api/v1/block-bindings/verify", map[string]any{
		"scope": "project",
	})
	assertStatus(t, projectWithoutID, http.StatusBadRequest)

	platformWithProject := client.json(http.MethodPut, "/api/v1/block-bindings/verify", map[string]any{
		"scope": "platform", "project_id": 42,
	})
	assertStatus(t, platformWithProject, http.StatusBadRequest)
}

func newExtensionPaginationServer(t *testing.T) (*sqlite.Store, *Server) {
	t.Helper()
	st, err := sqlite.Open(context.Background(), filepath.Join(t.TempDir(), "extensions.db"))
	if err != nil {
		t.Fatalf("sqlite.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	service, err := app.NewExtensionService(app.ExtensionServiceConfig{
		Store: st, Workspace: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewExtensionService: %v", err)
	}
	return st, &Server{extensions: NewAppExtensions(service), access: paginationAccess{}, logger: newTestLogger(io.Discard)}
}

// paginationAccess grants the project read used by handler-level extension
// unit tests, which bypass the route guards.
type paginationAccess struct{}

func (paginationAccess) GetProjectAccess(context.Context, int64, int64) (string, error) {
	return store.ProjectAccessManage, nil
}
func (paginationAccess) ListUsers(context.Context, string, int) ([]store.User, string, error) {
	return nil, "", nil
}
func (paginationAccess) SetUserDisabled(context.Context, int64, bool) error { return nil }
func (paginationAccess) RevokeUserCredentials(context.Context, int64) error { return nil }
func (paginationAccess) ListProjectGrants(context.Context, int64, string, int) ([]store.ProjectGrant, string, error) {
	return nil, "", nil
}
func (paginationAccess) SetProjectGrant(context.Context, int64, int64, string) error { return nil }
func (paginationAccess) DeleteProjectGrant(context.Context, int64, int64) error      { return nil }

func extensionList(t *testing.T, handler http.HandlerFunc, path string) (int, map[string]any, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	ctx := context.WithValue(req.Context(), principalKey, principal{
		user: sessionUser{ID: 7, Username: "alice", Role: roleAdmin},
	})
	handler(rec, req.WithContext(ctx))
	var body map[string]any
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode list response: %v", err)
		}
	}
	return rec.Code, body, rec.Body.String()
}

func TestExtensionListPagination(t *testing.T) {
	ctx := context.Background()
	st, srv := newExtensionPaginationServer(t)
	for _, id := range []int64{42, 43} {
		path := fmt.Sprintf("group/project-%d", id)
		if _, err := st.RegisterProject(ctx, store.Project{
			ID:                id,
			PathWithNamespace: path,
			WebURL:            "https://gitlab.example.com/" + path,
			DefaultBranch:     "main",
			TokenEnc:          []byte{1},
			WebhookSecretEnc:  []byte{2},
		}); err != nil {
			t.Fatalf("RegisterProject %d: %v", id, err)
		}
	}
	project42 := int64(42)
	project43 := int64(43)

	for _, name := range []string{"p1", "p2", "p3"} {
		if _, err := st.CreateMCPServer(ctx, store.MCPServer{
			Scope: "platform", Name: name, Config: json.RawMessage(`{"command":"x"}`),
		}); err != nil {
			t.Fatalf("create platform mcp %s: %v", name, err)
		}
	}
	for _, name := range []string{"42a", "42b"} {
		if _, err := st.CreateMCPServer(ctx, store.MCPServer{
			Scope: "project", ProjectID: &project42, Name: name, Config: json.RawMessage(`{"command":"x"}`),
		}); err != nil {
			t.Fatalf("create project mcp %s: %v", name, err)
		}
	}
	if _, err := st.CreateMCPServer(ctx, store.MCPServer{
		Scope: "project", ProjectID: &project43, Name: "43a", Config: json.RawMessage(`{"command":"x"}`),
	}); err != nil {
		t.Fatalf("create other mcp: %v", err)
	}
	for _, name := range []string{"s1", "s2", "s3"} {
		if _, err := st.CreateSkill(ctx, store.Skill{
			Scope: "platform", Name: name, Description: "d",
		}); err != nil {
			t.Fatalf("create platform skill %s: %v", name, err)
		}
	}
	for _, name := range []string{"s42a", "s42b"} {
		if _, err := st.CreateSkill(ctx, store.Skill{
			Scope: "project", ProjectID: &project42, Name: name, Description: "d",
		}); err != nil {
			t.Fatalf("create project skill %s: %v", name, err)
		}
	}
	for _, block := range []string{"a", "b", "c"} {
		if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{
			Scope: "platform", Block: block,
		}); err != nil {
			t.Fatalf("create platform binding %s: %v", block, err)
		}
	}
	for _, block := range []string{"a", "b"} {
		if _, err := st.UpsertBlockBinding(ctx, store.BlockBinding{
			Scope: "project", ProjectID: &project42, Block: block,
		}); err != nil {
			t.Fatalf("create project binding %s: %v", block, err)
		}
	}

	code, body, text := extensionList(t, srv.handleListMCPServers, "/api/v1/mcp-servers?scope=platform&limit=1")
	if code != http.StatusOK || len(body["servers"].([]any)) != 1 {
		t.Fatalf("mcp first page = %d %s", code, text)
	}
	firstCursor, _ := body["next_cursor"].(string)
	if firstCursor == "" {
		t.Fatalf("mcp first page has no cursor: %s", text)
	}
	code, _, _ = extensionList(t, srv.handleListMCPServers,
		"/api/v1/mcp-servers?scope=project&project_id=42&limit=1&cursor="+firstCursor)
	if code != http.StatusBadRequest {
		t.Fatalf("cross-scope mcp cursor = %d", code)
	}
	code, body, text = extensionList(t, srv.handleListMCPServers,
		"/api/v1/mcp-servers?scope=project&project_id=42&limit=1")
	if code != http.StatusOK {
		t.Fatalf("project mcp page = %d %s", code, text)
	}
	projectCursor, _ := body["next_cursor"].(string)
	if projectCursor == "" {
		t.Fatalf("project mcp page has no cursor: %s", text)
	}
	code, _, _ = extensionList(t, srv.handleListMCPServers,
		"/api/v1/mcp-servers?scope=project&project_id=43&limit=1&cursor="+projectCursor)
	if code != http.StatusBadRequest {
		t.Fatalf("cross-project mcp cursor = %d", code)
	}

	seen := map[string]bool{}
	cursor := ""
	for {
		path := "/api/v1/mcp-servers?scope=platform&limit=1"
		if cursor != "" {
			path += "&cursor=" + cursor
		}
		code, body, text := extensionList(t, srv.handleListMCPServers, path)
		if code != http.StatusOK {
			t.Fatalf("mcp page = %d %s", code, text)
		}
		for _, item := range body["servers"].([]any) {
			id := item.(map[string]any)["id"].(string)
			if seen[id] {
				t.Fatalf("duplicate mcp %s", id)
			}
			seen[id] = true
		}
		next, _ := body["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 3 {
		t.Fatalf("platform mcp count = %d", len(seen))
	}

	code, body, text = extensionList(t, srv.handleListSkills, "/api/v1/skills?scope=platform&limit=1")
	if code != http.StatusOK || len(body["skills"].([]any)) != 1 {
		t.Fatalf("skills first page = %d %s", code, text)
	}
	skillCursor, _ := body["next_cursor"].(string)
	if skillCursor == "" {
		t.Fatalf("skills first page has no cursor: %s", text)
	}
	code, _, _ = extensionList(t, srv.handleListSkills,
		"/api/v1/skills?scope=project&project_id=42&limit=1&cursor="+skillCursor)
	if code != http.StatusBadRequest {
		t.Fatalf("cross-scope skill cursor = %d", code)
	}

	code, body, text = extensionList(t, srv.handleListBlockBindings, "/api/v1/block-bindings?scope=platform&limit=1")
	if code != http.StatusOK || len(body["bindings"].([]any)) != 1 {
		t.Fatalf("bindings first page = %d %s", code, text)
	}
	bindingCursor, _ := body["next_cursor"].(string)
	if bindingCursor == "" {
		t.Fatalf("bindings first page has no cursor: %s", text)
	}
	code, _, _ = extensionList(t, srv.handleListBlockBindings,
		"/api/v1/block-bindings?scope=project&project_id=42&limit=1&cursor="+bindingCursor)
	if code != http.StatusBadRequest {
		t.Fatalf("cross-scope binding cursor = %d", code)
	}

	code, _, text = extensionList(t, srv.handleListMCPServers,
		"/api/v1/mcp-servers?scope=platform&cursor=not-a-cursor")
	if code != http.StatusBadRequest || !strings.Contains(text, "invalid cursor") {
		t.Fatalf("invalid mcp cursor = %d %s", code, text)
	}
}
