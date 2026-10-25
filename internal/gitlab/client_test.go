package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseProjectRef(t *testing.T) {
	valid := map[string]ProjectRef{
		"42":            {ID: 42},
		"group/repo":    {Path: "group/repo"},
		"group/sub/r":   {Path: "group/sub/r"},
		"a_b/c.d-e":     {Path: "a_b/c.d-e"},
		"group/repo_1":  {Path: "group/repo_1"},
		"namespace/ABC": {Path: "namespace/ABC"},
	}
	for input, want := range valid {
		got, err := ParseProjectRef(input)
		if err != nil {
			t.Errorf("ParseProjectRef(%q) error: %v", input, err)
			continue
		}
		if got != want {
			t.Errorf("ParseProjectRef(%q) = %+v, want %+v", input, got, want)
		}
	}
	invalid := []string{
		"", " 42", "42 ", "0", "-1", "+1", "1.5",
		"https://gitlab.com/group/repo",
		"/group/repo", "group/repo/", "group//repo", "single",
		"group/../repo", "group/./repo", "group/%2e%2e/repo",
		"group/repo?x=1", "group/repo#x", "group/repo\\x",
		"group/re po", "group/repo\n", "é/repo", "group/-repo",
	}
	for _, input := range invalid {
		if _, err := ParseProjectRef(input); err == nil {
			t.Errorf("ParseProjectRef(%q) unexpectedly succeeded", input)
		}
	}
}

func TestNewRejectsUnsafeBaseURLs(t *testing.T) {
	cases := []string{
		"", "gitlab.example.com", "ftp://gitlab.example.com",
		"https://user:pass@gitlab.example.com", "https://gitlab.example.com?x=1",
		"https://gitlab.example.com/#fragment", " https://gitlab.example.com",
	}
	for _, base := range cases {
		if _, err := New(Config{BaseURL: base}); err == nil {
			t.Errorf("New(%q) unexpectedly succeeded", base)
		}
	}
	if _, err := New(Config{BaseURL: "https://gitlab.example.com"}); err != nil {
		t.Fatalf("New(valid) error: %v", err)
	}
}

func TestWebhookLabelsAcceptStringsAndObjects(t *testing.T) {
	body := []byte(`{
		"object_kind":"merge_request",
		"project":{"id":42},
		"object_attributes":{
			"iid":7,
			"target_project_id":42,
			"last_commit":{"id":"` + strings.Repeat("a", 40) + `"},
			"labels":["plain",{"title":"object-title"},{"name":"object-name"}]
		}
	}`)
	payload, err := ParseWebhook("Merge Request Hook", "u", "t", body)
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	want := []string{"plain", "object-title", "object-name"}
	if fmt.Sprint(payload.ObjectAttributes.Labels) != fmt.Sprint(want) {
		t.Errorf("labels = %v, want %v", payload.ObjectAttributes.Labels, want)
	}
	if payload.ObjectAttributes.IID != 7 || payload.ObjectAttributes.TargetProjectID != 42 {
		t.Errorf("attributes = %+v", payload.ObjectAttributes)
	}
	if payload.Event != "Merge Request Hook" || payload.UUID != "u" || payload.Token != "t" {
		t.Errorf("request metadata = %+v", payload)
	}
}

func TestParseWebhookRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseWebhook("Merge Request Hook", "", "", []byte(`{`)); err == nil {
		t.Fatal("malformed JSON unexpectedly accepted")
	}
}

func newSDKTestServer(t *testing.T, handler http.HandlerFunc) (*httptest.Server, API) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client, err := New(Config{
		BaseURL:        ts.URL,
		RequestTimeout: 2 * time.Second,
		MaxConcurrency: 4,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ts, client
}

func TestClientProjectsAndTokenInfo(t *testing.T) {
	const token = "glpat-test-token"
	var paths []string
	ts, client := newSDKTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("PRIVATE-TOKEN"); got != token {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v4/projects/group%2Frepo" || r.URL.EscapedPath() == "/api/v4/projects/group%2Frepo":
			fmt.Fprint(w, `{"id":42,"path_with_namespace":"group/repo","web_url":"https://gitlab.example.com/group/repo","http_url_to_repo":"https://gitlab.example.com/group/repo.git","default_branch":"main"}`)
		case r.URL.Path == "/api/v4/personal_access_tokens/self":
			fmt.Fprint(w, `{"scopes":["api","read_repository"],"active":true,"revoked":false,"expires_at":"2026-12-31"}`)
		case r.URL.Path == "/api/v4/user":
			fmt.Fprint(w, `{"id":9,"username":"review-bot","name":"Review Bot","bot":true}`)
		case r.URL.Path == "/api/v4/projects/42/members/all/7":
			fmt.Fprint(w, `{"id":7,"access_level":30}`)
		case r.URL.Path == "/api/v4/projects/42/merge_requests/8":
			fmt.Fprint(w, `{"iid":8,"state":"opened","target_project_id":42,"sha":"`+strings.Repeat("b", 40)+`","merged_at":"2026-10-20T10:00:00Z","title":"Fix bug","description":"body","source_branch":"feat","target_branch":"main","labels":["bug"],"draft":false,"web_url":"https://gitlab.example.com/group/repo/-/merge_requests/8","author":{"id":9,"username":"review-bot","name":"Review Bot"}}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	})
	defer ts.Close()

	ctx := context.Background()
	project, err := client.GetProject(ctx, token, "group/repo")
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if project.ID != 42 || project.PathWithNamespace != "group/repo" || project.DefaultBranch != "main" {
		t.Errorf("project = %+v", project)
	}
	if project.HTTPURLToRepo != "https://gitlab.example.com/group/repo.git" {
		t.Errorf("project clone URL = %q", project.HTTPURLToRepo)
	}
	info, err := client.GetTokenInfo(ctx, token)
	if err != nil {
		t.Fatalf("GetTokenInfo: %v", err)
	}
	if !info.Active || info.Revoked || len(info.Scopes) != 2 || info.ExpiresAt == nil {
		t.Errorf("token info = %+v", info)
	}
	user, err := client.CurrentUser(ctx, token)
	if err != nil {
		t.Fatalf("CurrentUser: %v", err)
	}
	if user.ID != 9 || user.Username != "review-bot" || user.Name != "Review Bot" || !user.Bot {
		t.Errorf("user = %+v", user)
	}
	access, err := client.GetMemberAccess(ctx, token, 42, 7)
	if err != nil || access != 30 {
		t.Fatalf("GetMemberAccess = %d, %v", access, err)
	}
	mr, err := client.GetMergeRequest(ctx, token, 42, 8)
	if err != nil || mr.State != "opened" || mr.TargetProjectID != 42 {
		t.Fatalf("GetMergeRequest = %+v, %v", mr, err)
	}
	if mr.MergedAt == nil || !mr.MergedAt.Equal(time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)) {
		t.Errorf("merge request merged_at = %v", mr.MergedAt)
	}
	if mr.Title != "Fix bug" || mr.Description != "body" || mr.SourceBranch != "feat" ||
		mr.TargetBranch != "main" || mr.Draft || len(mr.Labels) != 1 || mr.Labels[0] != "bug" ||
		mr.WebURL == "" || mr.Author == nil || mr.Author.Name != "Review Bot" || mr.Author.Username != "review-bot" {
		t.Errorf("merge request details = %+v", mr)
	}
	if len(paths) != 5 {
		t.Errorf("paths = %v", paths)
	}
}

func TestClientDoesNotFollowRedirects(t *testing.T) {
	var followed atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		followed.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()

	ts, client := newSDKTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	})
	defer ts.Close()

	_, err := client.GetProject(context.Background(), "token", "42")
	if err == nil {
		t.Fatal("expected redirect to fail")
	}
	if followed.Load() != 0 {
		t.Fatal("redirect target was called")
	}
	if StatusOf(err) != http.StatusFound {
		t.Errorf("status = %d, want 302", StatusOf(err))
	}
}

func TestCreateHookDoesNotRetryAndDoesNotLeakUpstreamBody(t *testing.T) {
	var count atomic.Int32
	_, client := newSDKTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		http.Error(w, "upstream-secret-body", http.StatusInternalServerError)
	})
	_, err := client.CreateProjectHook(context.Background(), "glpat-secret", 42, HookSpec{
		URL:    "https://review.example.com/webhooks/gitlab",
		Secret: "whsec",
	})
	if err == nil {
		t.Fatal("expected hook creation to fail")
	}
	if count.Load() != 1 {
		t.Fatalf("hook create called %d times, want 1", count.Load())
	}
	text := err.Error()
	for _, forbidden := range []string{"upstream-secret-body", "glpat-secret", "https://review.example.com"} {
		if strings.Contains(text, forbidden) {
			t.Errorf("error %q leaked %q", text, forbidden)
		}
	}
	if StatusOf(err) != http.StatusInternalServerError {
		t.Errorf("status = %d", StatusOf(err))
	}
}

func TestCreateAndDeleteHookPayload(t *testing.T) {
	var created, deleted bool
	_, client := newSDKTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/projects/42/hooks":
			created = true
			var body struct {
				URL                   string `json:"url"`
				Token                 string `json:"token"`
				MergeRequestsEvents   bool   `json:"merge_requests_events"`
				NoteEvents            bool   `json:"note_events"`
				PushEvents            bool   `json:"push_events"`
				EnableSSLVerification bool   `json:"enable_ssl_verification"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode hook body: %v", err)
			}
			checks := map[string]string{
				"url":                     body.URL,
				"token":                   body.Token,
				"merge_requests_events":   fmt.Sprint(body.MergeRequestsEvents),
				"note_events":             fmt.Sprint(body.NoteEvents),
				"push_events":             fmt.Sprint(body.PushEvents),
				"enable_ssl_verification": fmt.Sprint(body.EnableSSLVerification),
			}
			for key, want := range checks {
				if key == "url" && want != "https://review.example.com/webhooks/gitlab" {
					t.Errorf("hook url = %q", want)
				}
				if key == "token" && want != "whsec" {
					t.Errorf("hook token = %q", want)
				}
				if key != "url" && key != "token" && want != "true" {
					t.Errorf("hook field %s = %q, want true", key, want)
				}
			}
			fmt.Fprint(w, `{"id":99}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v4/projects/42/hooks/99":
			deleted = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})
	id, err := client.CreateProjectHook(context.Background(), "token", 42, HookSpec{
		URL:                   "https://review.example.com/webhooks/gitlab",
		Secret:                "whsec",
		PushEvents:            true,
		MergeRequestsEvents:   true,
		NoteEvents:            true,
		EnableSSLVerification: true,
	})
	if err != nil || id != 99 {
		t.Fatalf("CreateProjectHook = %d, %v", id, err)
	}
	if err := client.DeleteProjectHook(context.Background(), "token", 42, 99); err != nil {
		t.Fatalf("DeleteProjectHook: %v", err)
	}
	if !created || !deleted {
		t.Fatalf("created=%v deleted=%v", created, deleted)
	}
}

func TestListAndUpdateHookPayload(t *testing.T) {
	var listed bool
	_, client := newSDKTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/42/hooks":
			listed = true
			if got := r.URL.Query().Get("page"); got != "1" {
				t.Errorf("page = %q", got)
			}
			fmt.Fprint(w, `[{"id":11,"url":"https://review.example.com/webhooks/gitlab",
				"push_events":true,"merge_requests_events":true,"note_events":false,
				"enable_ssl_verification":true,"alert_status":"executable"}]`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v4/projects/42/hooks/11":
			var body struct {
				URL                   string `json:"url"`
				Token                 string `json:"token"`
				MergeRequestsEvents   bool   `json:"merge_requests_events"`
				NoteEvents            bool   `json:"note_events"`
				PushEvents            bool   `json:"push_events"`
				EnableSSLVerification bool   `json:"enable_ssl_verification"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode hook body: %v", err)
			}
			if body.URL != "https://review.example.com/webhooks/gitlab" || body.Token != "whsec" ||
				!body.PushEvents || !body.MergeRequestsEvents || !body.NoteEvents || !body.EnableSSLVerification {
				t.Errorf("update body = %+v", body)
			}
			fmt.Fprint(w, `{"id":11,"url":"https://review.example.com/webhooks/gitlab"}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	hooks, err := client.ListProjectHooks(context.Background(), "token", 42)
	if err != nil || !listed || len(hooks) != 1 {
		t.Fatalf("ListProjectHooks = %+v, %v (listed=%v)", hooks, err, listed)
	}
	hook := hooks[0]
	if hook.ID != 11 || hook.URL != "https://review.example.com/webhooks/gitlab" ||
		!hook.PushEvents || !hook.MergeRequestsEvents || hook.NoteEvents ||
		!hook.EnableSSLVerification || hook.AlertStatus != "executable" {
		t.Fatalf("hook = %+v", hook)
	}
	if err := client.UpdateProjectHook(context.Background(), "token", 42, 11, HookSpec{
		URL:                   "https://review.example.com/webhooks/gitlab",
		Secret:                "whsec",
		PushEvents:            true,
		MergeRequestsEvents:   true,
		NoteEvents:            true,
		EnableSSLVerification: true,
	}); err != nil {
		t.Fatalf("UpdateProjectHook: %v", err)
	}
}

func TestUpdateHookFailureIsSanitized(t *testing.T) {
	_, client := newSDKTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "upstream-secret-body", http.StatusForbidden)
	})
	err := client.UpdateProjectHook(context.Background(), "glpat-secret", 42, 11, HookSpec{
		URL:    "https://review.example.com/webhooks/gitlab",
		Secret: "whsec",
	})
	if err == nil || StatusOf(err) != http.StatusForbidden {
		t.Fatalf("error = %v", err)
	}
	for _, forbidden := range []string{"upstream-secret-body", "glpat-secret", "https://review.example.com"} {
		if strings.Contains(err.Error(), forbidden) {
			t.Errorf("error %q leaked %q", err, forbidden)
		}
	}
}

func TestClientConcurrencyLimit(t *testing.T) {
	var current, maximum atomic.Int32
	start := make(chan struct{})
	client, err := New(Config{
		BaseURL:        "https://gitlab.invalid",
		RequestTimeout: 3 * time.Second,
		MaxConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := current.Add(1)
		for {
			old := maximum.Load()
			if n <= old || maximum.CompareAndSwap(old, n) {
				break
			}
		}
		<-start
		time.Sleep(20 * time.Millisecond)
		current.Add(-1)
		fmt.Fprint(w, `{"id":42,"path_with_namespace":"g/r","web_url":"https://example.com","default_branch":"main"}`)
	}))
	defer ts.Close()
	// Rebuild the client with the real test server but the same concurrency.
	client, err = New(Config{BaseURL: ts.URL, RequestTimeout: 3 * time.Second, MaxConcurrency: 2})
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 6)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := client.GetProject(context.Background(), "token", "42")
			errs <- err
		}()
	}
	time.Sleep(30 * time.Millisecond)
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("GetProject: %v", err)
		}
	}
	if got := maximum.Load(); got > 2 {
		t.Fatalf("maximum concurrency = %d, want <= 2", got)
	}
}

func TestClientTimeoutIsSanitized(t *testing.T) {
	ts, client := newSDKTestServerWithTimeout(t, 40*time.Millisecond, func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
	})
	_, err := client.GetProject(context.Background(), "glpat-secret", "42")
	if err == nil {
		t.Fatal("expected timeout")
	}
	if StatusOf(err) != 0 {
		t.Errorf("status = %d, want 0", StatusOf(err))
	}
	text := err.Error()
	for _, forbidden := range []string{"glpat-secret", ts.URL} {
		if strings.Contains(text, forbidden) {
			t.Errorf("error %q leaked %q", text, forbidden)
		}
	}
}

func newSDKTestServerWithTimeout(t *testing.T, timeout time.Duration, handler http.HandlerFunc) (*httptest.Server, API) {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client, err := New(Config{BaseURL: ts.URL, RequestTimeout: timeout, MaxConcurrency: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return ts, client
}

func TestClientRejectsEmptyToken(t *testing.T) {
	_, client := newSDKTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Error("server must not be called without a token")
	})
	if _, err := client.GetProject(context.Background(), "", "42"); err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestClientSemaphoreWaitHonorsRequestTimeout(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"id":42,"path_with_namespace":"g/r","web_url":"https://example.com","default_branch":"main"}`)
	}))
	defer ts.Close()
	client, err := New(Config{BaseURL: ts.URL, RequestTimeout: 100 * time.Millisecond, MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	client.sem <- struct{}{}

	start := time.Now()
	_, err = client.GetProject(context.Background(), "glpat-secret", "42")
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want DeadlineExceeded", err)
	}
	if elapsed > time.Second {
		t.Fatalf("semaphore wait was not bounded: %v", elapsed)
	}
	if requests.Load() != 0 {
		t.Fatal("request was sent while the semaphore was occupied")
	}
	if strings.Contains(err.Error(), "glpat-secret") || strings.Contains(err.Error(), ts.URL) {
		t.Fatalf("error leaked details: %v", err)
	}

	<-client.sem
	if _, err := client.GetProject(context.Background(), "token", "42"); err != nil {
		t.Fatalf("semaphore was not released after timeout: %v", err)
	}
}

func TestClientSemaphoreWaitHonorsCancellation(t *testing.T) {
	var requests atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		fmt.Fprint(w, `{"id":42,"path_with_namespace":"g/r","web_url":"https://example.com","default_branch":"main"}`)
	}))
	defer ts.Close()
	client, err := New(Config{BaseURL: ts.URL, RequestTimeout: 2 * time.Second, MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}
	client.sem <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	_, err = client.GetProject(ctx, "token", "42")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want Canceled", err)
	}
	if requests.Load() != 0 {
		t.Fatal("cancelled request was sent")
	}
	<-client.sem
	if _, err := client.GetProject(context.Background(), "token", "42"); err != nil {
		t.Fatalf("semaphore was not released after cancellation: %v", err)
	}
}
