package server

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/skill"
	"github.com/LeXwDeX/tribunal/internal/store"
)

// skillZipFiles builds one archive from path -> content.
func skillZipFiles(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	return buffer.Bytes()
}

func symlinkSkillZip(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("skill/SKILL.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(testSkillManifest)); err != nil {
		t.Fatal(err)
	}
	header := &zip.FileHeader{Name: "skill/link.md"}
	header.SetMode(os.ModeSymlink | 0o777)
	link, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := link.Write([]byte("SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func duplicateSkillZip(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for i := 0; i < 2; i++ {
		entry, err := writer.Create("skill/SKILL.md")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(testSkillManifest)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// corruptStoredSkillZip flips one payload byte in a stored entry so reading
// it fails CRC validation while the archive structure stays valid.
func corruptStoredSkillZip(t *testing.T) []byte {
	t.Helper()
	payload := strings.Repeat("a", 512)
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range map[string]string{
		"skill/SKILL.md": testSkillManifest,
		"skill/data.txt": payload,
	} {
		entry, err := writer.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatalf("zip create: %v", err)
		}
		if _, err := entry.Write([]byte(content)); err != nil {
			t.Fatalf("zip write: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	data := buffer.Bytes()
	index := bytes.Index(data, []byte(payload))
	if index < 0 {
		t.Fatal("stored payload not found in archive")
	}
	data[index+7] ^= 0xff
	return data
}

type countingReader struct {
	reader io.Reader
	read   int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.reader.Read(p)
	c.read += n
	return n, err
}

func uploadBody(t *testing.T, fields map[string]string, data []byte) ([]byte, string) {
	t.Helper()
	return multipartBody(t, fields, "file", "skill.zip", data)
}

func multipartFieldsBody(t *testing.T, fields map[string]string) ([]byte, string) {
	t.Helper()
	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			t.Fatalf("WriteField: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart: %v", err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

func assertErrorCode(t *testing.T, response apiResponse, want string) {
	t.Helper()
	body := decodeMap(t, response.body)
	errBody, ok := body["error"].(map[string]any)
	if !ok || errBody["code"] != want {
		t.Fatalf("error = %v, want code %q", body["error"], want)
	}
}

func wiringAnonymousClient(env *wiringEnv) *m6Client {
	return &m6Client{t: env.t, base: env.server.URL, client: &http.Client{}}
}

func TestSkillUploadAuthorizesBeforeReadingBody(t *testing.T) {
	ctx := context.Background()
	st, srv := newExtensionPaginationServer(t)
	// A viewer must be rejected without the server reading or buffering the
	// body. A one-megabyte body would otherwise spill to a temporary file.
	body := &countingReader{reader: strings.NewReader(strings.Repeat("x", 1<<20))}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/skills", body)
	request.Header.Set("Content-Type", "multipart/form-data; boundary=missing")
	request = request.WithContext(context.WithValue(request.Context(), principalKey,
		principal{user: sessionUser{ID: 8, Username: "bob", Role: roleViewer}}))
	recorder := httptest.NewRecorder()
	srv.handleUploadSkillOrRegisterDirectory(recorder, request)
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	if body.read != 0 {
		t.Fatalf("handler read %d body bytes before authorization", body.read)
	}
	rows, err := st.ListSkills(ctx, store.ScopePlatform, nil)
	if err != nil || len(rows) != 0 {
		t.Fatalf("unauthorized upload persisted rows: %+v, %v", rows, err)
	}
}

func TestSkillUploadAuthorizationAndSuccess(t *testing.T) {
	env := newWiringEnv(t)
	archive, contentType := uploadBody(t, map[string]string{"scope": "platform"},
		skillZip(t, testSkillManifest, true))
	post := func(client *m6Client) apiResponse {
		return client.request(http.MethodPost, "/api/v1/skills", archive, map[string]string{"Content-Type": contentType})
	}

	assertStatus(t, post(wiringAnonymousClient(env)), http.StatusUnauthorized)
	assertStatus(t, post(env.loginAs("code-viewer")), http.StatusForbidden)
	assertStatus(t, post(env.loginAs("code-admin").withoutCSRF()), http.StatusForbidden)

	created := post(env.loginAs("code-admin"))
	assertStatus(t, created, http.StatusCreated)
	row := decodeMap(t, created.body)
	if row["name"] != "go-review" || row["description"] != "Review Go changes" ||
		row["scope"] != "platform" || row["enabled"] != true || row["builtin"] != false {
		t.Fatalf("created skill = %v", row)
	}
	managedDir, _ := row["dir"].(string)
	if !strings.HasPrefix(managedDir, filepath.Join(env.manager.Root(), "skills")) {
		t.Fatalf("dir = %q, want below the managed skills root", managedDir)
	}
	if _, err := os.Stat(filepath.Join(managedDir, "SKILL.md")); err != nil {
		t.Fatalf("uploaded manifest: %v", err)
	}
	loader, err := skill.New(skill.Options{PlatformDirs: []string{filepath.Dir(managedDir)}})
	if err != nil {
		t.Fatalf("skill.New: %v", err)
	}
	names := make([]string, 0)
	for _, item := range loader.Catalogue() {
		names = append(names, item.Name)
	}
	if len(names) != 1 || names[0] != "go-review" {
		t.Fatalf("catalogue = %v", names)
	}

	assertStatus(t, post(env.loginAs("code-admin")), http.StatusConflict)

	projectArchive, projectType := uploadBody(t,
		map[string]string{"scope": "project", "project_id": "42"},
		skillZip(t, "---\nname: project-review\ndescription: Project review\n---\n", true))
	projectCreated := env.business().request(http.MethodPost, "/api/v1/skills", projectArchive,
		map[string]string{"Content-Type": projectType})
	assertStatus(t, projectCreated, http.StatusCreated)
	if row := decodeMap(t, projectCreated.body); row["scope"] != "project" || row["project_id"] != float64(42) {
		t.Fatalf("project skill = %v", row)
	}

	for name, body := range map[string]struct {
		fields map[string]string
		data   []byte
	}{
		"missing scope":    {fields: map[string]string{}, data: skillZip(t, testSkillManifest, true)},
		"bad project id":   {fields: map[string]string{"scope": "project", "project_id": "nope"}, data: skillZip(t, testSkillManifest, true)},
		"empty file":       {fields: map[string]string{"scope": "platform"}, data: nil},
		"not a zip":        {fields: map[string]string{"scope": "platform"}, data: []byte("not a zip")},
		"missing manifest": {fields: map[string]string{"scope": "platform"}, data: skillZip(t, "hello", false)},
		"invalid manifest": {fields: map[string]string{"scope": "platform"}, data: skillZip(t, "---\nname: bad__name\ndescription: x\n---\n", true)},
	} {
		t.Run(name, func(t *testing.T) {
			invalid, invalidType := uploadBody(t, body.fields, body.data)
			response := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/skills", invalid,
				map[string]string{"Content-Type": invalidType})
			assertStatus(t, response, http.StatusBadRequest)
			assertErrorCode(t, response, "validation_failed")
		})
	}

	noFile, noFileType := multipartFieldsBody(t, map[string]string{"scope": "platform"})
	missingFile := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/skills", noFile,
		map[string]string{"Content-Type": noFileType})
	assertStatus(t, missingFile, http.StatusBadRequest)
	assertErrorCode(t, missingFile, "validation_failed")
}

func TestSkillUploadRejectsUnsafeArchivesAtHTTP(t *testing.T) {
	env := newWiringEnv(t)
	longDescription := "---\nname: go-review\ndescription: " + strings.Repeat("x", 4097) + "\n---\n"
	cases := map[string][]byte{
		"traversal": skillZipFiles(t, map[string]string{
			"skill/SKILL.md":         testSkillManifest,
			"skill/../../../evil.go": "package evil",
		}),
		"multiple manifests": skillZipFiles(t, map[string]string{
			"a/SKILL.md": testSkillManifest,
			"b/SKILL.md": testSkillManifest,
		}),
		"duplicate paths": duplicateSkillZip(t),
		"nul path": skillZipFiles(t, map[string]string{
			"skill/SKILL.md":    testSkillManifest,
			"skill/bad\x00name": "x",
		}),
		"overlong path": skillZipFiles(t, map[string]string{
			"skill/SKILL.md":                    testSkillManifest,
			"skill/" + strings.Repeat("a", 512): "x",
		}),
		"file directory conflict": skillZipFiles(t, map[string]string{
			"skill/SKILL.md":   testSkillManifest,
			"skill/x":          "file",
			"skill/x/child.md": "child",
		}),
		"symlink":          symlinkSkillZip(t),
		"bad yaml":         skillZip(t, "---\nname: go-review\ndescription: \"unterminated\n---\n", true),
		"long description": skillZip(t, longDescription, true),
		"corrupt checksum": corruptStoredSkillZip(t),
	}
	for name, archive := range cases {
		t.Run(name, func(t *testing.T) {
			body, contentType := uploadBody(t, map[string]string{"scope": "platform"}, archive)
			response := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/skills", body,
				map[string]string{"Content-Type": contentType})
			assertStatus(t, response, http.StatusBadRequest)
			assertErrorCode(t, response, "validation_failed")
			if text := string(response.body); strings.Contains(text, "\\xff") || strings.Contains(text, "unterminated") {
				t.Fatalf("response leaked manifest content: %s", text)
			}
		})
	}

	oversized := make([]byte, skillUploadMax+1)
	body, contentType := uploadBody(t, map[string]string{"scope": "platform"}, oversized)
	response := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/skills", body,
		map[string]string{"Content-Type": contentType})
	assertStatus(t, response, http.StatusBadRequest)
	assertErrorCode(t, response, "validation_failed")

	list := env.loginAs("code-admin").request(http.MethodGet, "/api/v1/skills?scope=platform&limit=200", nil, nil)
	assertStatus(t, list, http.StatusOK)
	if skills := decodeMap(t, list.body)["skills"].([]any); len(skills) != 0 {
		t.Fatalf("invalid uploads persisted rows: %v", skills)
	}
}

func TestSkillDirectoryRegistrationHTTP(t *testing.T) {
	env := newWiringEnv(t)
	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: security\ndescription: Security review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(source, "references"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "references", "check.md"), []byte("check"), 0o600); err != nil {
		t.Fatal(err)
	}

	register := func(client *m6Client, body map[string]any) apiResponse {
		return client.json(http.MethodPost, "/api/v1/skills", body)
	}
	assertStatus(t, register(wiringAnonymousClient(env), map[string]any{"scope": "platform", "dir": source}), http.StatusUnauthorized)
	assertStatus(t, register(env.loginAs("code-viewer"), map[string]any{"scope": "platform", "dir": source}), http.StatusForbidden)

	created := register(env.loginAs("code-admin"), map[string]any{"scope": "platform", "dir": source})
	assertStatus(t, created, http.StatusCreated)
	row := decodeMap(t, created.body)
	managedDir, _ := row["dir"].(string)
	if row["name"] != "security" || !strings.HasPrefix(managedDir, filepath.Join(env.manager.Root(), "skills")) {
		t.Fatalf("registered skill = %v", row)
	}
	if managedDir == source {
		t.Fatal("registered dir still points at the source")
	}
	if _, err := os.Stat(filepath.Join(managedDir, "references", "check.md")); err != nil {
		t.Fatalf("copied resource: %v", err)
	}
	if _, err := os.Stat(filepath.Join(source, "SKILL.md")); err != nil {
		t.Fatalf("source was removed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(source, "references", "check.md"), []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	copied, err := os.ReadFile(filepath.Join(managedDir, "references", "check.md"))
	if err != nil || string(copied) != "check" {
		t.Fatalf("registered copy changed with the source: %q, %v", copied, err)
	}

	relative := register(env.loginAs("code-admin"), map[string]any{"scope": "platform", "dir": "skills/go"})
	assertStatus(t, relative, http.StatusBadRequest)
	assertErrorCode(t, relative, "validation_failed")

	linkParent := t.TempDir()
	link := filepath.Join(linkParent, "linked")
	if err := os.Symlink(source, link); err != nil {
		t.Fatal(err)
	}
	symlinked := register(env.loginAs("code-admin"), map[string]any{"scope": "platform", "dir": link})
	assertStatus(t, symlinked, http.StatusBadRequest)
	assertErrorCode(t, symlinked, "validation_failed")
	if text := string(symlinked.body); strings.Contains(text, linkParent) {
		t.Fatalf("response leaked the absolute path: %s", text)
	}

	noManifest := register(env.loginAs("code-admin"), map[string]any{"scope": "platform", "dir": t.TempDir()})
	assertStatus(t, noManifest, http.StatusBadRequest)
	assertErrorCode(t, noManifest, "validation_failed")

	invalidSource := t.TempDir()
	if err := os.WriteFile(filepath.Join(invalidSource, "SKILL.md"),
		[]byte("---\nname: Bad Name\ndescription: review\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	invalid := register(env.loginAs("code-admin"), map[string]any{"scope": "platform", "dir": invalidSource})
	assertStatus(t, invalid, http.StatusBadRequest)
	assertErrorCode(t, invalid, "validation_failed")
	if text := string(invalid.body); strings.Contains(text, invalidSource) {
		t.Fatalf("response leaked the absolute path: %s", text)
	}

	unknown := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/skills",
		[]byte(`{"scope":"platform","dir":"`+source+`","bogus":true}`), map[string]string{"Content-Type": "application/json"})
	assertStatus(t, unknown, http.StatusBadRequest)
	assertErrorCode(t, unknown, "validation_failed")
}

func TestSkillCreationRejectsBuiltinNameConflictAtHTTP(t *testing.T) {
	env := newWiringEnv(t)
	if _, err := env.db.CreateSkill(context.Background(), store.Skill{
		Scope: "platform", Name: "taken", Description: "built-in", Enabled: true, Builtin: "default:1",
	}); err != nil {
		t.Fatal(err)
	}
	body, contentType := uploadBody(t, map[string]string{"scope": "platform"},
		skillZip(t, "---\nname: taken\ndescription: shadow\n---\n", true))
	response := env.loginAs("code-admin").request(http.MethodPost, "/api/v1/skills", body,
		map[string]string{"Content-Type": contentType})
	assertStatus(t, response, http.StatusConflict)
	assertErrorCode(t, response, "conflict")

	source := t.TempDir()
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"),
		[]byte("---\nname: taken\ndescription: shadow\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registered := env.loginAs("code-admin").json(http.MethodPost, "/api/v1/skills",
		map[string]any{"scope": "platform", "dir": source})
	assertStatus(t, registered, http.StatusConflict)
	assertErrorCode(t, registered, "conflict")
}

func TestSkillCreationHandlerRequiresService(t *testing.T) {
	server := &Server{}
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(`{"scope":"platform","dir":"/tmp"}`))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	server.handleUploadSkillOrRegisterDirectory(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
}

func TestSkillWriteAuditOmitsContent(t *testing.T) {
	_, srv := newExtensionPaginationServer(t)
	var logs bytes.Buffer
	srv.logger = newTestLogger(&logs)
	principalContext := func(request *http.Request) *http.Request {
		return request.WithContext(context.WithValue(request.Context(), principalKey,
			principal{user: sessionUser{ID: 7, Username: "alice", Role: roleAdmin}}))
	}

	archive := skillZip(t, "---\nname: audited-skill\ndescription: audit-description-canary\n---\naudit-body-canary\n", true)
	body, contentType := uploadBody(t, map[string]string{"scope": "platform"}, archive)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewReader(body))
	request.Header.Set("Content-Type", contentType)
	recorder := httptest.NewRecorder()
	srv.handleUploadSkillOrRegisterDirectory(recorder, principalContext(request))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	sum := sha256.Sum256(archive)
	text := logs.String()
	for _, want := range []string{
		`"event":"platform_audit"`, `"action":"upload"`, `"name":"audited-skill"`,
		`"size_bytes":`, `"sha256":"` + hex.EncodeToString(sum[:]) + `"`, `"actor_id":7`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("audit log missing %q: %s", want, text)
		}
	}
	for _, secret := range []string{"audit-description-canary", "audit-body-canary"} {
		if strings.Contains(text, secret) {
			t.Fatalf("audit log leaked %q: %s", secret, text)
		}
	}

	logs.Reset()
	source := t.TempDir()
	manifest := []byte("---\nname: registered-audit\ndescription: dir-description-canary\n---\ndir-body-canary\n")
	if err := os.WriteFile(filepath.Join(source, "SKILL.md"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"scope": "platform", "dir": source})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodPost, "/api/v1/skills", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	srv.handleUploadSkillOrRegisterDirectory(recorder, principalContext(request))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("register status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	manifestSum := sha256.Sum256(manifest)
	text = logs.String()
	for _, want := range []string{
		`"event":"platform_audit"`, `"action":"register_dir"`, `"name":"registered-audit"`,
		`"manifest_sha256":"` + hex.EncodeToString(manifestSum[:]) + `"`,
		`"manifest_size_bytes":` + strconv.Itoa(len(manifest)),
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("audit log missing %q: %s", want, text)
		}
	}
	for _, secret := range []string{"dir-description-canary", "dir-body-canary", source} {
		if strings.Contains(text, secret) {
			t.Fatalf("audit log leaked %q: %s", secret, text)
		}
	}
}
