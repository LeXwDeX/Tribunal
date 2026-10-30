package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

// bindingBody builds a block binding PUT body. expectedRevision is nil when
// the test intentionally omits the field.
func bindingBody(scope string, projectID *int64, tools, skills []string, expectedRevision any) map[string]any {
	body := map[string]any{"scope": scope, "mcp_tools": tools, "skills": skills}
	if projectID != nil {
		body["project_id"] = *projectID
	}
	if expectedRevision != nil {
		body["expected_revision"] = expectedRevision
	}
	return body
}

func blockBindingPath(block string) string {
	return "/api/v1/block-bindings/" + block
}

func blockBindingListPath(scope string, projectID *int64) string {
	path := "/api/v1/block-bindings?scope=" + scope
	if projectID != nil {
		path += fmt.Sprintf("&project_id=%d", *projectID)
	}
	return path
}

// findBlockBinding returns the row a paginated list returned for block. The
// tests in this file use a small number of rows, so the first page is enough.
func findBlockBinding(t *testing.T, client *m6Client, scope string, projectID *int64, block string) (map[string]any, bool) {
	t.Helper()
	response := client.request(http.MethodGet, blockBindingListPath(scope, projectID), nil, nil)
	assertStatus(t, response, http.StatusOK)
	body := decodeMap(t, response.body)
	rows, ok := body["bindings"].([]any)
	if !ok {
		t.Fatalf("bindings = %v", body["bindings"])
	}
	for _, value := range rows {
		row := value.(map[string]any)
		if row["block"] == block {
			return row, true
		}
	}
	return nil, false
}

func requireBlockBinding(t *testing.T, client *m6Client, scope string, projectID *int64, block string) map[string]any {
	t.Helper()
	row, ok := findBlockBinding(t, client, scope, projectID, block)
	if !ok {
		t.Fatalf("%s/%s binding is missing", scope, block)
	}
	if revision, _ := row["revision"].(string); !store.ValidBlockBindingRevision(revision) {
		t.Fatalf("%s/%s revision = %q, want a valid revision", scope, block, row["revision"])
	}
	return row
}

func bindingPUT(t *testing.T, client *m6Client, block string, body map[string]any) apiResponse {
	t.Helper()
	return client.json(http.MethodPut, blockBindingPath(block), body)
}

func bindingRevisionFromResponse(t *testing.T, response apiResponse) string {
	t.Helper()
	body := decodeMap(t, response.body)
	revision, _ := body["revision"].(string)
	if !store.ValidBlockBindingRevision(revision) {
		t.Fatalf("response revision = %q, body=%s", revision, response.body)
	}
	return revision
}

func skillsContain(row map[string]any, want string) bool {
	values, _ := row["skills"].([]any)
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func bindingPutOutcome(t *testing.T, client *m6Client, block string, body map[string]any) (int, string) {
	t.Helper()
	response := bindingPUT(t, client, block, body)
	if response.status != http.StatusOK {
		return response.status, ""
	}
	return response.status, bindingRevisionFromResponse(t, response)
}

func TestBlockBindingStaleRevisionDoesNotOverwrite(t *testing.T) {
	env, _, _ := newRestoreHTTPEnv(t, restoreCfg())
	client := env.bearerClient()

	created := bindingPUT(t, client, "review", bindingBody("platform", nil,
		[]string{"tool-a"}, []string{"skill-a"}, store.BlockBindingAbsentRevision))
	assertStatus(t, created, http.StatusOK)
	first := bindingRevisionFromResponse(t, created)
	if row := requireBlockBinding(t, client, "platform", nil, "review"); row["revision"] != first {
		t.Fatalf("list revision = %v, want %q", row["revision"], first)
	}

	updated := bindingPUT(t, client, "review", bindingBody("platform", nil,
		[]string{"tool-b"}, []string{"skill-b"}, first))
	assertStatus(t, updated, http.StatusOK)
	second := bindingRevisionFromResponse(t, updated)
	if second == first {
		t.Fatalf("revision did not change after a content update: %q", second)
	}

	stale := bindingPUT(t, client, "review", bindingBody("platform", nil,
		[]string{"tool-c"}, []string{"skill-c"}, first))
	assertStatus(t, stale, http.StatusConflict)
	if code := decodeMap(t, stale.body)["error"].(map[string]any)["code"]; code != "conflict" {
		t.Fatalf("stale error code = %v", code)
	}

	row := requireBlockBinding(t, client, "platform", nil, "review")
	if row["revision"] != second {
		t.Fatalf("revision after stale write = %v, want %q", row["revision"], second)
	}
	if got := row["skills"].([]any); len(got) != 1 || got[0] != "skill-b" {
		t.Fatalf("stale write changed skills: %v", got)
	}
	if got := row["mcp_tools"].([]any); len(got) != 1 || got[0] != "tool-b" {
		t.Fatalf("stale write changed tools: %v", got)
	}
}

func TestBlockBindingConcurrentWritersOnlyOneWins(t *testing.T) {
	env, _, _ := newRestoreHTTPEnv(t, restoreCfg())
	client := env.bearerClient()
	created := bindingPUT(t, client, "reasoner", bindingBody("platform", nil, nil, nil, store.BlockBindingAbsentRevision))
	assertStatus(t, created, http.StatusOK)
	base := bindingRevisionFromResponse(t, created)

	type outcome struct {
		status   int
		revision string
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	send := func(skill string) {
		<-start
		status, revision := bindingPutOutcome(t, client, "reasoner", bindingBody("platform", nil,
			[]string{"tool-" + skill}, []string{skill}, base))
		results <- outcome{status: status, revision: revision}
	}
	go send("one")
	go send("two")
	close(start)
	first, second := <-results, <-results

	statuses := map[int]int{first.status: 1}
	statuses[second.status]++
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent statuses = %v, want one 200 and one 409", statuses)
	}
	winner := first
	if first.status != http.StatusOK {
		winner = second
	}
	row := requireBlockBinding(t, client, "platform", nil, "reasoner")
	if row["revision"] != winner.revision {
		t.Fatalf("stored revision = %v, want winner %q", row["revision"], winner.revision)
	}
	skills := row["skills"].([]any)
	if len(skills) != 1 || (skills[0] != "one" && skills[0] != "two") {
		t.Fatalf("stored skills = %v", skills)
	}
}

func TestBlockBindingConcurrentCreatesOnlyOneWins(t *testing.T) {
	env, _, _ := newRestoreHTTPEnv(t, restoreCfg())
	client := env.bearerClient()

	type outcome struct{ status int }
	start := make(chan struct{})
	results := make(chan outcome, 2)
	for _, skill := range []string{"one", "two"} {
		skill := skill
		go func() {
			<-start
			response := bindingPUT(t, client, "onboarding", bindingBody("platform", nil,
				nil, []string{skill}, store.BlockBindingAbsentRevision))
			results <- outcome{status: response.status}
		}()
	}
	close(start)
	first, second := <-results, <-results
	statuses := map[int]int{first.status: 1}
	statuses[second.status]++
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("concurrent create statuses = %v, want one 200 and one 409", statuses)
	}
	row := requireBlockBinding(t, client, "platform", nil, "onboarding")
	if skills := row["skills"].([]any); len(skills) != 1 {
		t.Fatalf("stored skills = %v", skills)
	}
}

func TestBlockBindingExpectedRevisionValidation(t *testing.T) {
	env, _, _ := newRestoreHTTPEnv(t, restoreCfg())
	client := env.bearerClient()
	validHex := strings.Repeat("a", 64)

	cases := []struct {
		name string
		body map[string]any
	}{
		{name: "missing", body: bindingBody("platform", nil, nil, nil, nil)},
		{name: "null", body: map[string]any{"scope": "platform", "expected_revision": nil}},
		{name: "empty", body: bindingBody("platform", nil, nil, nil, "")},
		{name: "uppercase", body: bindingBody("platform", nil, nil, nil, strings.ToUpper(validHex))},
		{name: "short", body: bindingBody("platform", nil, nil, nil, validHex[:63])},
		{name: "long", body: bindingBody("platform", nil, nil, nil, validHex+"a")},
		{name: "not hex", body: bindingBody("platform", nil, nil, nil, strings.Repeat("g", 64))},
		{name: "surrounding space", body: bindingBody("platform", nil, nil, nil, " "+store.BlockBindingAbsentRevision)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response := bindingPUT(t, client, "synthesize", tc.body)
			assertStatus(t, response, http.StatusBadRequest)
			if code := decodeMap(t, response.body)["error"].(map[string]any)["code"]; code != "validation_failed" {
				t.Fatalf("error code = %v", code)
			}
			if _, ok := findBlockBinding(t, client, "platform", nil, "synthesize"); ok {
				t.Fatal("invalid expected_revision wrote a binding")
			}
		})
	}

	accepted := bindingPUT(t, client, "synthesize", bindingBody("platform", nil, nil, nil, store.BlockBindingAbsentRevision))
	assertStatus(t, accepted, http.StatusOK)
}

func TestBlockBindingRevisionIsScopedAndContentAddressed(t *testing.T) {
	env, _, _ := newRestoreHTTPEnv(t, restoreCfg())
	env.grant(7, 42, store.ProjectAccessManage)
	env.grant(7, 43, store.ProjectAccessManage)
	client := env.loginAs("code-admin")
	project42, project43 := int64(42), int64(43)

	write := bindingPUT(t, client, "review", bindingBody("project", &project42, nil, []string{"project-42"}, store.BlockBindingAbsentRevision))
	assertStatus(t, write, http.StatusOK)
	revision42 := bindingRevisionFromResponse(t, write)

	write43 := bindingPUT(t, client, "review", bindingBody("project", &project43, nil, []string{"project-43"}, store.BlockBindingAbsentRevision))
	assertStatus(t, write43, http.StatusOK)
	revision43 := bindingRevisionFromResponse(t, write43)
	if revision42 == revision43 {
		t.Fatalf("different project scopes share revision %q", revision42)
	}

	platform := bindingPUT(t, client, "review", bindingBody("platform", nil, nil, nil, store.BlockBindingAbsentRevision))
	assertStatus(t, platform, http.StatusOK)
	platformRevision := bindingRevisionFromResponse(t, platform)
	if platformRevision == revision42 || platformRevision == revision43 {
		t.Fatalf("platform and project revisions collide: %q", platformRevision)
	}

	// A valid revision from another scope is stale for this row.
	assertStatus(t, bindingPUT(t, client, "review",
		bindingBody("project", &project42, nil, []string{"wrong"}, revision43)), http.StatusConflict)
	assertStatus(t, bindingPUT(t, client, "review",
		bindingBody("project", &project42, nil, []string{"wrong"}, platformRevision)), http.StatusConflict)
	assertStatus(t, bindingPUT(t, client, "review",
		bindingBody("platform", nil, nil, []string{"wrong"}, revision42)), http.StatusConflict)

	row := requireBlockBinding(t, client, "project", &project42, "review")
	if row["revision"] != revision42 || row["skills"].([]any)[0] != "project-42" {
		t.Fatalf("project 42 row changed: %v", row)
	}
	row43 := requireBlockBinding(t, client, "project", &project43, "review")
	if row43["revision"] != revision43 || row43["skills"].([]any)[0] != "project-43" {
		t.Fatalf("project 43 row changed: %v", row43)
	}
}

func TestBlockBindingRevisionNormalizesNilButKeepsOrderAndDuplicates(t *testing.T) {
	env, _, _ := newRestoreHTTPEnv(t, restoreCfg())
	client := env.bearerClient()

	// An absent binding with omitted arrays is stored as empty arrays.
	empty := bindingPUT(t, client, "verify", map[string]any{
		"scope": "platform", "expected_revision": store.BlockBindingAbsentRevision,
	})
	assertStatus(t, empty, http.StatusOK)
	emptyRevision := bindingRevisionFromResponse(t, empty)
	row := requireBlockBinding(t, client, "platform", nil, "verify")
	if len(row["mcp_tools"].([]any)) != 0 || len(row["skills"].([]any)) != 0 {
		t.Fatalf("nil arrays were not normalized: %v", row)
	}

	explicitEmpty := bindingPUT(t, client, "verify", bindingBody("platform", nil, []string{}, []string{}, emptyRevision))
	assertStatus(t, explicitEmpty, http.StatusOK)
	if got := bindingRevisionFromResponse(t, explicitEmpty); got != emptyRevision {
		t.Fatalf("nil and empty arrays have different revisions: %q != %q", got, emptyRevision)
	}

	ordered := bindingPUT(t, client, "verify", bindingBody("platform", nil,
		[]string{"a", "b"}, []string{"s1", "s2"}, emptyRevision))
	assertStatus(t, ordered, http.StatusOK)
	orderedRevision := bindingRevisionFromResponse(t, ordered)

	reordered := bindingPUT(t, client, "verify", bindingBody("platform", nil,
		[]string{"b", "a"}, []string{"s2", "s1"}, orderedRevision))
	assertStatus(t, reordered, http.StatusOK)
	reorderedRevision := bindingRevisionFromResponse(t, reordered)
	if reorderedRevision == orderedRevision {
		t.Fatal("array order does not affect the revision")
	}

	duplicated := bindingPUT(t, client, "verify", bindingBody("platform", nil,
		[]string{"b", "a", "b"}, []string{"s2", "s1", "s2"}, reorderedRevision))
	assertStatus(t, duplicated, http.StatusOK)
	duplicatedRevision := bindingRevisionFromResponse(t, duplicated)
	if duplicatedRevision == reorderedRevision {
		t.Fatal("duplicate entries do not affect the revision")
	}

	row = requireBlockBinding(t, client, "platform", nil, "verify")
	if row["revision"] != duplicatedRevision {
		t.Fatalf("stored revision = %v, want %q", row["revision"], duplicatedRevision)
	}
	tools := row["mcp_tools"].([]any)
	if len(tools) != 3 || tools[0] != "b" || tools[1] != "a" || tools[2] != "b" {
		t.Fatalf("stored tools lost order or duplicates: %v", tools)
	}
}

func TestBlockBindingRevisionRejectsWriteAfterRestore(t *testing.T) {
	cfg := restoreCfg()
	env, st, service := newRestoreHTTPEnv(t, cfg)
	applyRestoreDefaults(t, service, st, cfg)
	client := env.bearerClient()

	initial := requireBlockBinding(t, client, "platform", nil, "review")
	initialRevision := initial["revision"].(string)
	if !skillsContain(initial, "code-review") {
		t.Fatalf("initial skills = %v", initial["skills"])
	}

	custom := bindingPUT(t, client, "review", bindingBody("platform", nil, nil, []string{"team-rules"}, initialRevision))
	assertStatus(t, custom, http.StatusOK)
	customRevision := bindingRevisionFromResponse(t, custom)
	if customRevision == initialRevision {
		t.Fatal("custom write did not change the revision")
	}

	preview := client.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults",
		map[string]any{"blocks": []string{"review"}, "dry_run": true})
	assertStatus(t, preview, http.StatusOK)
	token := decodeMap(t, preview.body)["preview_token"].(string)
	execute := client.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults",
		map[string]any{"blocks": []string{"review"}, "dry_run": false, "preview_token": token})
	assertStatus(t, execute, http.StatusOK)

	restored := requireBlockBinding(t, client, "platform", nil, "review")
	restoredRevision := restored["revision"].(string)
	if restoredRevision == customRevision || !skillsContain(restored, "team-rules") || !skillsContain(restored, "code-review") {
		t.Fatalf("restore did not add the default and keep the custom grant: %v", restored)
	}

	stale := bindingPUT(t, client, "review", bindingBody("platform", nil, nil, []string{"stale"}, customRevision))
	assertStatus(t, stale, http.StatusConflict)
	row := requireBlockBinding(t, client, "platform", nil, "review")
	if row["revision"] != restoredRevision || !skillsContain(row, "code-review") || skillsContain(row, "stale") {
		t.Fatalf("stale write changed the restored row: %v", row)
	}
}

func TestBlockBindingConcurrentRestoreAndWrite(t *testing.T) {
	cfg := restoreCfg()
	env, st, service := newRestoreHTTPEnv(t, cfg)
	applyRestoreDefaults(t, service, st, cfg)
	client := env.bearerClient()

	initial := requireBlockBinding(t, client, "platform", nil, "review")
	custom := bindingPUT(t, client, "review", bindingBody("platform", nil, nil, []string{"team-rules"}, initial["revision"].(string)))
	assertStatus(t, custom, http.StatusOK)
	baseRevision := bindingRevisionFromResponse(t, custom)
	preview := client.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults",
		map[string]any{"blocks": []string{"review"}, "dry_run": true})
	assertStatus(t, preview, http.StatusOK)
	token := decodeMap(t, preview.body)["preview_token"].(string)

	type outcome struct {
		status   int
		revision string
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	go func() {
		<-start
		status, revision := bindingPutOutcome(t, client, "review", bindingBody("platform", nil, nil, []string{"team"}, baseRevision))
		results <- outcome{status: status, revision: revision}
	}()
	go func() {
		<-start
		response := client.json(http.MethodPost, "/api/v1/block-bindings/restore-defaults",
			map[string]any{"blocks": []string{"review"}, "dry_run": false, "preview_token": token})
		results <- outcome{status: response.status}
	}()
	close(start)
	first, second := <-results, <-results
	statuses := map[int]int{first.status: 1}
	statuses[second.status]++
	if statuses[http.StatusOK] != 1 || statuses[http.StatusConflict] != 1 {
		t.Fatalf("restore/write statuses = %v, want one 200 and one 409", statuses)
	}

	row := requireBlockBinding(t, client, "platform", nil, "review")
	writeWon := (first.status == http.StatusOK && first.revision != "") ||
		(second.status == http.StatusOK && second.revision != "")
	if writeWon {
		want := first.revision
		if want == "" {
			want = second.revision
		}
		if row["revision"] != want {
			t.Fatalf("write won but stored revision = %v, want %q", row["revision"], want)
		}
		if skills := row["skills"].([]any); len(skills) != 1 || skills[0] != "team" {
			t.Fatalf("write won but skills = %v", skills)
		}
		return
	}
	// Restore won; the refreshed row must contain the default and custom content.
	if !skillsContain(row, "team-rules") || !skillsContain(row, "code-review") {
		t.Fatalf("restore won but skills = %v", row["skills"])
	}
}
