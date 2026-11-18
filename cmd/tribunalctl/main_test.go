package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

type capturedRequest struct {
	method string
	path   string
	auth   string
	cookie string
	csrf   string
	body   map[string]any
}

func captureHandler(t *testing.T, status int, response string, captured *capturedRequest) http.HandlerFunc {
	t.Helper()
	return func(w http.ResponseWriter, r *http.Request) {
		captured.method = r.Method
		captured.path = r.URL.Path
		captured.auth = r.Header.Get("Authorization")
		captured.cookie = r.Header.Get("Cookie")
		captured.csrf = r.Header.Get("X-CSRF-Token")
		if r.Body != nil {
			data, _ := io.ReadAll(r.Body)
			if len(data) > 0 {
				if err := json.Unmarshal(data, &captured.body); err != nil {
					t.Errorf("request body is not JSON: %s", data)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}
}

func runCtl(t *testing.T, args []string, env map[string]string, client *http.Client) (string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	err := run(context.Background(), args, func(name string) string { return env[name] }, &stdout, &stderr, client)
	return stdout.String(), stderr.String(), err
}

func TestRegisterCommand(t *testing.T) {
	var captured capturedRequest
	ts := httptest.NewServer(captureHandler(t, http.StatusCreated,
		`{"project":{"id":42},"job_id":"job-1","webhook_secret":"one-time-secret"}`, &captured))
	defer ts.Close()

	stdout, _, err := runCtl(t, []string{
		"--url", ts.URL,
		"--api-token-env", "ADMIN_TOKEN",
		"projects", "register",
		"--project", "group/repo",
		"--token-env", "PROJECT_TOKEN",
		"--profile", "deep",
		"--auto-webhook",
		"--settings", `{"skip_draft":false}`,
	}, map[string]string{
		"ADMIN_TOKEN":   "admin-secret-value",
		"PROJECT_TOKEN": "glpat-project-token",
	}, ts.Client())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if captured.method != http.MethodPost || captured.path != "/api/v1/projects" {
		t.Fatalf("request = %s %s", captured.method, captured.path)
	}
	if captured.auth != "Bearer admin-secret-value" {
		t.Fatalf("auth = %q", captured.auth)
	}
	wantBody := map[string]any{
		"project":      "group/repo",
		"token":        "glpat-project-token",
		"profile":      "deep",
		"auto_webhook": true,
		"settings":     map[string]any{"skip_draft": false},
	}
	for key, value := range wantBody {
		if captured.body[key] == nil {
			t.Errorf("missing body field %q", key)
			continue
		}
		if key == "settings" {
			settings, _ := captured.body[key].(map[string]any)
			if settings["skip_draft"] != false {
				t.Errorf("settings = %v", captured.body[key])
			}
			continue
		}
		if key != "auto_webhook" && captured.body[key] != value {
			t.Errorf("body[%s] = %v, want %v", key, captured.body[key], value)
		}
	}
	if captured.body["auto_webhook"] != true || captured.body["token"] != "glpat-project-token" {
		t.Errorf("body = %v", captured.body)
	}
	if !strings.Contains(stdout, "one-time-secret") {
		t.Fatalf("stdout = %q", stdout)
	}
	for _, forbidden := range []string{"admin-secret-value", "glpat-project-token"} {
		if strings.Contains(stdout, forbidden) {
			t.Fatalf("stdout leaked %q", forbidden)
		}
	}
}

// Registration creates the webhook by default; --auto-webhook=false keeps the
// manual path and must be sent explicitly so the server cannot apply its
// omitted-field default.
func TestRegisterCommandAutoWebhookDefaultAndExplicitFalse(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "default on", want: true},
		{name: "explicit false", args: []string{"--auto-webhook=false"}, want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var captured capturedRequest
			ts := httptest.NewServer(captureHandler(t, http.StatusCreated,
				`{"project":{"id":42},"job_id":"job-1"}`, &captured))
			defer ts.Close()

			args := []string{
				"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
				"projects", "register", "--project", "group/repo", "--token-env", "PROJECT_TOKEN",
			}
			args = append(args, tt.args...)
			if _, _, err := runCtl(t, args, map[string]string{
				"ADMIN_TOKEN": "admin-secret-value", "PROJECT_TOKEN": "glpat-project-token",
			}, ts.Client()); err != nil {
				t.Fatalf("run: %v", err)
			}
			if got, ok := captured.body["auto_webhook"].(bool); !ok || got != tt.want {
				t.Fatalf("auto_webhook = %v, want %v (body %v)", captured.body["auto_webhook"], tt.want, captured.body)
			}
		})
	}
}

func TestRegisterCommandSharedBotTokenOptional(t *testing.T) {
	newServer := func(t *testing.T, shared bool, captured *capturedRequest) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/me":
				capability := "false"
				if shared {
					capability = "true"
				}
				_, _ = io.WriteString(w, `{"id":1,"username":"admin","role":"admin","shared_bot_credential":`+capability+`}`)
			case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects":
				captured.method, captured.path = r.Method, r.URL.Path
				if r.Body != nil {
					data, _ := io.ReadAll(r.Body)
					if err := json.Unmarshal(data, &captured.body); err != nil {
						t.Errorf("request body is not JSON: %s", data)
					}
				}
				w.WriteHeader(http.StatusCreated)
				_, _ = io.WriteString(w, `{"project":{"id":42},"job_id":"job-1"}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}

	t.Run("omitted token uses the shared bot", func(t *testing.T) {
		var captured capturedRequest
		ts := newServer(t, true, &captured)
		defer ts.Close()
		if _, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "register", "--project", "group/repo",
		}, map[string]string{"ADMIN_TOKEN": "admin"}, ts.Client()); err != nil {
			t.Fatalf("run: %v", err)
		}
		if captured.path != "/api/v1/projects" || captured.body["token"] != "" {
			t.Fatalf("body = %v", captured.body)
		}
	})

	t.Run("explicit token still wins", func(t *testing.T) {
		var captured capturedRequest
		ts := newServer(t, true, &captured)
		defer ts.Close()
		if _, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "register", "--project", "group/repo", "--token-env", "PROJECT_TOKEN",
		}, map[string]string{"ADMIN_TOKEN": "admin", "PROJECT_TOKEN": "glpat-project-token"}, ts.Client()); err != nil {
			t.Fatalf("run: %v", err)
		}
		if captured.body["token"] != "glpat-project-token" {
			t.Fatalf("body = %v", captured.body)
		}
	})

	t.Run("omitted token without the capability is rejected", func(t *testing.T) {
		var captured capturedRequest
		ts := newServer(t, false, &captured)
		defer ts.Close()
		_, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "register", "--project", "group/repo",
		}, map[string]string{"ADMIN_TOKEN": "admin"}, ts.Client())
		if err == nil || err.Error() != "--token-env must be a valid environment variable name" {
			t.Fatalf("error = %v", err)
		}
		if captured.path != "" {
			t.Fatalf("registration was attempted: %s", captured.path)
		}
	})
}

func TestRotateTokenCommand(t *testing.T) {
	type rotateServer struct {
		shared   bool
		put      *capturedRequest
		meCalls  int
		putCalls int
	}
	newServer := func(t *testing.T, s *rotateServer) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/api/v1/me":
				s.meCalls++
				capability := "false"
				if s.shared {
					capability = "true"
				}
				_, _ = io.WriteString(w, `{"id":1,"username":"admin","role":"admin","shared_bot_credential":`+capability+`}`)
			case r.Method == http.MethodPut && r.URL.Path == "/api/v1/projects/42/token":
				s.putCalls++
				s.put.method, s.put.path = r.Method, r.URL.Path
				s.put.auth = r.Header.Get("Authorization")
				data, _ := io.ReadAll(r.Body)
				if err := json.Unmarshal(data, &s.put.body); err != nil {
					t.Errorf("request body is not JSON: %s", data)
				}
				_, _ = io.WriteString(w, `{"id":42,"token_scopes":[]}`)
			default:
				t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
			}
		}))
	}

	t.Run("shared sends an empty token", func(t *testing.T) {
		s := &rotateServer{shared: true, put: &capturedRequest{}}
		ts := newServer(t, s)
		defer ts.Close()
		stdout, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "rotate-token", "42", "--shared",
		}, map[string]string{"ADMIN_TOKEN": "admin"}, ts.Client())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if s.meCalls != 1 || s.putCalls != 1 {
			t.Fatalf("calls = me:%d put:%d", s.meCalls, s.putCalls)
		}
		if s.put.method != http.MethodPut || s.put.path != "/api/v1/projects/42/token" {
			t.Fatalf("request = %s %s", s.put.method, s.put.path)
		}
		if s.put.auth != "Bearer admin" {
			t.Fatalf("auth = %q", s.put.auth)
		}
		token, ok := s.put.body["token"].(string)
		if !ok || token != "" {
			t.Fatalf("token = %v, want an explicit empty string", s.put.body["token"])
		}
		if !strings.Contains(stdout, `"id": 42`) {
			t.Fatalf("stdout = %q", stdout)
		}
	})

	t.Run("token env rotates the project token", func(t *testing.T) {
		s := &rotateServer{put: &capturedRequest{}}
		ts := newServer(t, s)
		defer ts.Close()
		if _, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "rotate-token", "42", "--token-env", "PROJECT_TOKEN",
		}, map[string]string{"ADMIN_TOKEN": "admin", "PROJECT_TOKEN": "glpat-new"}, ts.Client()); err != nil {
			t.Fatalf("run: %v", err)
		}
		if s.meCalls != 0 || s.putCalls != 1 {
			t.Fatalf("calls = me:%d put:%d", s.meCalls, s.putCalls)
		}
		if s.put.body["token"] != "glpat-new" {
			t.Fatalf("body = %v", s.put.body)
		}
	})

	t.Run("shared without the capability is rejected before the request", func(t *testing.T) {
		s := &rotateServer{put: &capturedRequest{}}
		ts := newServer(t, s)
		defer ts.Close()
		_, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "rotate-token", "42", "--shared",
		}, map[string]string{"ADMIN_TOKEN": "admin"}, ts.Client())
		if err == nil || !strings.Contains(err.Error(), "shared bot credential") {
			t.Fatalf("error = %v", err)
		}
		if s.meCalls != 1 || s.putCalls != 0 {
			t.Fatalf("calls = me:%d put:%d", s.meCalls, s.putCalls)
		}
	})

	t.Run("shared and token env are mutually exclusive", func(t *testing.T) {
		s := &rotateServer{shared: true, put: &capturedRequest{}}
		ts := newServer(t, s)
		defer ts.Close()
		_, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "rotate-token", "42", "--shared", "--token-env", "PROJECT_TOKEN",
		}, map[string]string{"ADMIN_TOKEN": "admin", "PROJECT_TOKEN": "glpat-new"}, ts.Client())
		if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Fatalf("error = %v", err)
		}
		if s.meCalls != 0 || s.putCalls != 0 {
			t.Fatalf("calls = me:%d put:%d", s.meCalls, s.putCalls)
		}
	})

	t.Run("neither flag is rejected", func(t *testing.T) {
		s := &rotateServer{shared: true, put: &capturedRequest{}}
		ts := newServer(t, s)
		defer ts.Close()
		_, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "rotate-token", "42",
		}, map[string]string{"ADMIN_TOKEN": "admin"}, ts.Client())
		if err == nil || err.Error() != "one of --token-env or --shared is required" {
			t.Fatalf("error = %v", err)
		}
		if s.meCalls != 0 || s.putCalls != 0 {
			t.Fatalf("calls = me:%d put:%d", s.meCalls, s.putCalls)
		}
	})

	t.Run("a missing project token env fails before the request", func(t *testing.T) {
		s := &rotateServer{put: &capturedRequest{}}
		ts := newServer(t, s)
		defer ts.Close()
		_, _, err := runCtl(t, []string{
			"--url", ts.URL, "--api-token-env", "ADMIN_TOKEN",
			"projects", "rotate-token", "42", "--token-env", "PROJECT_TOKEN",
		}, map[string]string{"ADMIN_TOKEN": "admin"}, ts.Client())
		if err == nil || !strings.Contains(err.Error(), "PROJECT_TOKEN") {
			t.Fatalf("error = %v", err)
		}
		if s.putCalls != 0 {
			t.Fatalf("request was sent: %d", s.putCalls)
		}
	})

	t.Run("an invalid project id is rejected", func(t *testing.T) {
		_, _, err := runCtl(t, []string{
			"--url", "http://127.0.0.1:1", "--api-token-env", "ADMIN_TOKEN",
			"projects", "rotate-token", "abc", "--shared",
		}, map[string]string{"ADMIN_TOKEN": "admin"}, nil)
		if err == nil || !strings.Contains(err.Error(), "positive integer") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestListAndGetCommands(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		var captured capturedRequest
		ts := httptest.NewServer(captureHandler(t, http.StatusOK, `{"projects":[{"id":42}]}`, &captured))
		defer ts.Close()
		stdout, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
			"REVIEW_ADMIN_TOKEN": "admin",
		}, ts.Client())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if captured.method != http.MethodGet || captured.path != "/api/v1/projects" {
			t.Fatalf("request = %s %s", captured.method, captured.path)
		}
		if !strings.Contains(stdout, `"id": 42`) {
			t.Fatalf("stdout = %s", stdout)
		}
	})
	t.Run("get", func(t *testing.T) {
		var captured capturedRequest
		ts := httptest.NewServer(captureHandler(t, http.StatusOK, `{"id":42}`, &captured))
		defer ts.Close()
		_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "get", "42"}, map[string]string{
			"REVIEW_ADMIN_TOKEN": "admin",
		}, ts.Client())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if captured.path != "/api/v1/projects/42" {
			t.Fatalf("path = %q", captured.path)
		}
	})
}

func TestStatusErrorsAreMeaningful(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"not_found","message":"project not registered","request_id":"req-1"}}`)
	}))
	defer ts.Close()
	_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, ts.Client())
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"404", "not_found", "req-1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err, want)
		}
	}
}

func TestStatusErrorsDoNotEchoArbitraryBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, `<html>upstream-secret-detail</html>`)
	}))
	defer ts.Close()
	_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, ts.Client())
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), "upstream-secret-detail") {
		t.Fatalf("error leaked body: %v", err)
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	var targetHits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		targetHits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusFound)
	}))
	defer redirect.Close()

	_, _, err := runCtl(t, []string{"--url", redirect.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, redirect.Client())
	if err == nil {
		t.Fatal("expected redirect to be rejected")
	}
	if targetHits.Load() != 0 {
		t.Fatal("redirect target was called")
	}
}

func TestMissingEnvironment(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer ts.Close()
	_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{}, ts.Client())
	if err == nil || !strings.Contains(err.Error(), "REVIEW_ADMIN_TOKEN") {
		t.Fatalf("error = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("request was sent without a token")
	}

	_, _, err = runCtl(t, []string{
		"--url", ts.URL, "projects", "register", "--project", "42", "--token-env", "PROJECT_TOKEN",
	}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
	if err == nil || !strings.Contains(err.Error(), "PROJECT_TOKEN") {
		t.Fatalf("error = %v", err)
	}
	if hits.Load() != 0 {
		t.Fatal("request was sent without a project token")
	}
}

func TestInvalidSettingsFailsBeforeRequest(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	defer ts.Close()
	for _, settings := range []string{`{`, `[]`} {
		_, _, err := runCtl(t, []string{
			"--url", ts.URL, "projects", "register", "--project", "42", "--token-env", "PROJECT_TOKEN", "--settings", settings,
		}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin", "PROJECT_TOKEN": "token"}, ts.Client())
		if err == nil {
			t.Fatalf("settings %q accepted", settings)
		}
	}
	if hits.Load() != 0 {
		t.Fatal("invalid settings reached the server")
	}
}

func TestInvalidServiceURLs(t *testing.T) {
	for _, value := range []string{
		"ftp://example.com", "https://user:pass@example.com", "https://example.com?x=1",
		"https://example.com/#fragment", "not-a-url", " https://example.com",
	} {
		_, _, err := runCtl(t, []string{"--url", value, "projects", "list"}, map[string]string{
			"REVIEW_ADMIN_TOKEN": "admin",
		}, nil)
		if err == nil {
			t.Errorf("--url %q accepted", value)
		}
	}
}

func TestHelpAndUsage(t *testing.T) {
	_, stderr, err := runCtl(t, nil, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil)
	if err == nil {
		t.Fatal("missing projects subcommand accepted")
	}
	if !strings.Contains(stderr, "Usage: tribunalctl") || strings.Contains(stderr, "Usage: reviewctl") {
		t.Fatalf("usage does not use tribunalctl: %q", stderr)
	}
	_, helpStderr, err := runCtl(t, []string{"--help"}, nil, nil)
	if !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help error = %v", err)
	}
	if !strings.Contains(helpStderr, "Usage: tribunalctl") {
		t.Fatalf("help usage does not use tribunalctl: %q", helpStderr)
	}
	if _, _, err := runCtl(t, []string{"projects", "bogus"}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil); err == nil {
		t.Fatal("unknown action accepted")
	}
	if _, _, err := runCtl(t, []string{"projects", "get", "abc"}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil); err == nil {
		t.Fatal("invalid id accepted")
	}
}

func TestSubcommandHelp(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want []string
	}{
		{"projects long", []string{"projects", "--help"}, []string{"Usage: tribunalctl", "register", "webhook"}},
		{"projects short", []string{"projects", "-h"}, []string{"Usage: tribunalctl", "register", "webhook"}},
		{"projects word", []string{"projects", "help"}, []string{"Usage: tribunalctl", "register", "webhook"}},
		{"project get", []string{"projects", "get", "--help"}, []string{"Usage: tribunalctl projects get", "<project ID>"}},
		{"project get after id", []string{"projects", "get", "42", "--help"}, []string{"Usage: tribunalctl projects get", "<project ID>"}},
		{"retry onboarding", []string{"projects", "retry-onboarding", "--help"}, []string{"Usage: tribunalctl projects retry-onboarding", "<project ID>"}},
		{"rotate token", []string{"projects", "rotate-token", "--help"}, []string{"Usage: tribunalctl projects rotate-token", "--shared"}},
		{"users long", []string{"users", "--help"}, []string{"Usage: tribunalctl", "disable", "revoke"}},
		{"user disable", []string{"users", "disable", "--help"}, []string{"Usage: tribunalctl users disable", "<user ID>"}},
		{"grants", []string{"projects", "grants", "--help"}, []string{"Usage: tribunalctl projects grants", "list", "set", "delete"}},
		{"webhook enable", []string{"projects", "webhook", "enable", "--help"}, []string{"Usage: tribunalctl projects webhook enable", "<project ID>"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// No token is configured: help must not require one.
			_, stderr, err := runCtl(t, tc.args, nil, nil)
			if !errors.Is(err, flag.ErrHelp) {
				t.Fatalf("error = %v, want flag.ErrHelp", err)
			}
			for _, want := range tc.want {
				if !strings.Contains(stderr, want) {
					t.Errorf("stderr %q missing %q", stderr, want)
				}
			}
		})
	}
}

func TestHelpDoesNotMaskErrors(t *testing.T) {
	for _, args := range [][]string{
		{"--url", "http://example.com", "projects", "bogus", "--help"},
		{"--url", "http://example.com", "projects", "get", "help"},
	} {
		_, _, err := runCtl(t, args, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil)
		if err == nil || errors.Is(err, flag.ErrHelp) {
			t.Fatalf("%v: error = %v, want a non-help error", args, err)
		}
	}
}

func TestInvalidJSONResponse(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "not json")
	}))
	defer ts.Close()
	_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, ts.Client())
	if err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestAPIErrorEnvelope(t *testing.T) {
	var envelope errorEnvelope
	if err := json.Unmarshal([]byte(`{"error":{"code":"conflict","message":"already registered","request_id":"r"}}`), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Error.Code != "conflict" {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestListFollowsNextCursor(t *testing.T) {
	var requests atomic.Int32
	var secondQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{"projects":[{"id":42}],"next_cursor":"page-two"}`)
		case "page-two":
			secondQuery = r.URL.RawQuery
			_, _ = io.WriteString(w, `{"projects":[{"id":43}],"next_cursor":""}`)
		default:
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer ts.Close()

	stdout, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "list"}, map[string]string{
		"REVIEW_ADMIN_TOKEN": "admin",
	}, ts.Client())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests = %d", requests.Load())
	}
	if secondQuery != "cursor=page-two" {
		t.Fatalf("second query = %q", secondQuery)
	}
	for _, want := range []string{`"id": 42`, `"id": 43`} {
		if !strings.Contains(stdout, want) {
			t.Fatalf("stdout missing %s: %s", want, stdout)
		}
	}
	if strings.Contains(stdout, "next_cursor") {
		t.Fatalf("stdout contains pagination metadata: %s", stdout)
	}
}

func TestAdminUserAndGrantCommands(t *testing.T) {
	cases := []struct {
		name       string
		args       []string
		method     string
		path       string
		status     int
		response   string
		wantBody   map[string]any
		wantOutput bool
	}{
		{
			name: "users list", args: []string{"users", "list"},
			method: http.MethodGet, path: "/api/v1/admin/users", status: http.StatusOK,
			response:   `{"users":[{"id":7,"username":"alice","disabled":false}],"next_cursor":""}`,
			wantOutput: true,
		},
		{
			name: "revoke credentials", args: []string{"users", "revoke", "7"},
			method: http.MethodDelete, path: "/api/v1/admin/users/7/credentials", status: http.StatusNoContent,
		},
		{
			name: "all projects", args: []string{"projects", "all"},
			method: http.MethodGet, path: "/api/v1/admin/projects", status: http.StatusOK,
			response:   `{"projects":[{"id":42,"path_with_namespace":"group/repo"}],"next_cursor":""}`,
			wantOutput: true,
		},
		{
			name: "list grants", args: []string{"projects", "grants", "list", "42"},
			method: http.MethodGet, path: "/api/v1/admin/projects/42/grants", status: http.StatusOK,
			response:   `{"grants":[{"project_id":42,"user_id":7,"role":"manage"}],"next_cursor":""}`,
			wantOutput: true,
		},
		{
			name: "set grant", args: []string{"projects", "grants", "set", "42", "7", "read"},
			method: http.MethodPut, path: "/api/v1/admin/projects/42/grants/7", status: http.StatusOK,
			response:   `{"project_id":42,"user_id":7,"role":"read"}`,
			wantBody:   map[string]any{"role": "read"},
			wantOutput: true,
		},
		{
			name: "delete grant", args: []string{"projects", "grants", "delete", "42", "7"},
			method: http.MethodDelete, path: "/api/v1/admin/projects/42/grants/7", status: http.StatusNoContent,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured capturedRequest
			ts := httptest.NewServer(captureHandler(t, tc.status, tc.response, &captured))
			defer ts.Close()
			stdout, _, err := runCtl(t, append([]string{"--url", ts.URL}, tc.args...),
				map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if captured.method != tc.method || captured.path != tc.path {
				t.Fatalf("request = %s %s, want %s %s", captured.method, captured.path, tc.method, tc.path)
			}
			for key, value := range tc.wantBody {
				if captured.body[key] != value {
					t.Fatalf("body[%s] = %v, want %v", key, captured.body[key], value)
				}
			}
			if tc.wantOutput && strings.TrimSpace(stdout) == "" {
				t.Fatal("stdout is empty")
			}
			if !tc.wantOutput && strings.TrimSpace(stdout) != "" {
				t.Fatalf("stdout = %q, want empty", stdout)
			}
		})
	}
}

type userAdminCall struct {
	method string
	path   string
	query  string
	body   map[string]any
}

// userAdminServer emulates the admin user endpoints with the revision contract
// of the real server: every read returns the next configured revision, and a
// PATCH without a positive revision is rejected with 400. patch decides the
// answer for a well-formed PATCH.
func userAdminServer(t *testing.T, userID int64, revisions []int64, patch func(revision int64, body map[string]any) (int, string)) (*httptest.Server, func() []userAdminCall) {
	t.Helper()
	var mu sync.Mutex
	var calls []userAdminCall
	read := 0
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if r.Body != nil {
			data, _ := io.ReadAll(r.Body)
			if len(data) > 0 {
				if err := json.Unmarshal(data, &body); err != nil {
					t.Errorf("request body is not JSON: %s", data)
				}
			}
		}
		mu.Lock()
		calls = append(calls, userAdminCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, body: body})
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/admin/users":
			mu.Lock()
			revision := int64(0)
			if read < len(revisions) {
				revision = revisions[read]
			}
			read++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprintf(w, `{"users":[{"id":%d,"revision":%d}],"next_cursor":""}`, userID, revision)
		case r.Method == http.MethodPatch && r.URL.Path == fmt.Sprintf("/api/v1/admin/users/%d", userID):
			revision, _ := body["revision"].(float64)
			if revision <= 0 {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"validation_failed","message":"revision is required"}}`)
				return
			}
			status, response := patch(int64(revision), body)
			w.WriteHeader(status)
			_, _ = io.WriteString(w, response)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"code":"bad_request"}}`)
		}
	}))
	return ts, func() []userAdminCall {
		mu.Lock()
		defer mu.Unlock()
		return append([]userAdminCall(nil), calls...)
	}
}

func TestUsersStateChangeSendsCurrentRevision(t *testing.T) {
	for _, tc := range []struct {
		action   string
		disabled bool
	}{
		{"disable", true},
		{"enable", false},
	} {
		t.Run(tc.action, func(t *testing.T) {
			ts, calls := userAdminServer(t, 7, []int64{3}, func(revision int64, body map[string]any) (int, string) {
				if revision != 3 {
					t.Errorf("revision = %d, want 3", revision)
				}
				if body["disabled"] != tc.disabled {
					t.Errorf("disabled = %v, want %v", body["disabled"], tc.disabled)
				}
				return http.StatusOK, fmt.Sprintf(`{"id":7,"disabled":%t}`, tc.disabled)
			})
			defer ts.Close()
			stdout, _, err := runCtl(t, []string{"--url", ts.URL, "users", tc.action, "7"},
				map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			got := calls()
			if len(got) != 2 || got[0].method != http.MethodGet || got[1].method != http.MethodPatch {
				t.Fatalf("requests = %+v, want GET then PATCH", got)
			}
			if strings.TrimSpace(stdout) == "" {
				t.Fatal("stdout is empty")
			}
		})
	}
}

func TestUsersStateChangeRetriesStaleRevision(t *testing.T) {
	ts, calls := userAdminServer(t, 7, []int64{3, 4}, func(revision int64, body map[string]any) (int, string) {
		if revision == 3 {
			return http.StatusConflict, `{"error":{"code":"stale_revision","message":"user revision is stale"}}`
		}
		if revision != 4 {
			t.Errorf("retry revision = %d, want 4", revision)
		}
		if body["disabled"] != true {
			t.Errorf("disabled = %v, want true", body["disabled"])
		}
		return http.StatusOK, `{"id":7,"disabled":true}`
	})
	defer ts.Close()

	stdout, _, err := runCtl(t, []string{"--url", ts.URL, "users", "disable", "7"},
		map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := calls()
	want := []string{http.MethodGet, http.MethodPatch, http.MethodGet, http.MethodPatch}
	if len(got) != len(want) {
		t.Fatalf("request count = %d (%+v), want %d", len(got), got, len(want))
	}
	for i := range want {
		if got[i].method != want[i] {
			t.Fatalf("request %d = %s, want %s", i, got[i].method, want[i])
		}
	}
	if got[1].body["revision"] != float64(3) || got[3].body["revision"] != float64(4) {
		t.Fatalf("revisions sent = %v, %v; want 3, 4", got[1].body["revision"], got[3].body["revision"])
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatal("stdout is empty")
	}
}

func TestUsersStateChangeReportsUnresolvedStaleRevision(t *testing.T) {
	ts, calls := userAdminServer(t, 7, []int64{3, 3}, func(revision int64, body map[string]any) (int, string) {
		return http.StatusConflict, `{"error":{"code":"stale_revision","message":"user revision is stale"}}`
	})
	defer ts.Close()

	_, _, err := runCtl(t, []string{"--url", ts.URL, "users", "disable", "7"},
		map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
	if err == nil || !strings.Contains(err.Error(), "stale_revision") {
		t.Fatalf("err = %v, want stale_revision failure", err)
	}
	got := calls()
	patches := 0
	for _, call := range got {
		if call.method == http.MethodPatch {
			patches++
		}
	}
	if patches != 2 {
		t.Fatalf("PATCH count = %d (%+v), want 2 (one retry)", patches, got)
	}
}

func TestUsersRevisionLookupRejectsUnknownUser(t *testing.T) {
	var mu sync.Mutex
	var methods []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		methods = append(methods, r.Method)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("cursor") {
		case "":
			_, _ = io.WriteString(w, `{"users":[{"id":8,"revision":1}],"next_cursor":"page-two"}`)
		case "page-two":
			_, _ = io.WriteString(w, `{"users":[],"next_cursor":""}`)
		default:
			http.Error(w, "bad cursor", http.StatusBadRequest)
		}
	}))
	defer ts.Close()

	_, _, err := runCtl(t, []string{"--url", ts.URL, "users", "disable", "7"},
		map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
	if err == nil || !strings.Contains(err.Error(), "user 7 not found") {
		t.Fatalf("err = %v, want missing user failure", err)
	}
	mu.Lock()
	defer mu.Unlock()
	for _, method := range methods {
		if method != http.MethodGet {
			t.Fatalf("methods = %v, want only GET before the user is found", methods)
		}
	}
	if len(methods) != 2 {
		t.Fatalf("requests = %v, want two paginated reads", methods)
	}
}

func TestProjectsWebhookLifecycle(t *testing.T) {
	cases := []struct {
		action string
		method string
		path   string
	}{
		{"enable", http.MethodPost, "/api/v1/projects/42/webhook"},
		{"rebuild", http.MethodPost, "/api/v1/projects/42/webhook/rebuild"},
		{"disable", http.MethodDelete, "/api/v1/projects/42/webhook"},
	}
	for _, tc := range cases {
		t.Run(tc.action, func(t *testing.T) {
			var captured capturedRequest
			ts := httptest.NewServer(captureHandler(t, http.StatusOK,
				`{"state":"enabled","managed_by":"crc"}`, &captured))
			defer ts.Close()
			stdout, _, err := runCtl(t, []string{
				"--url", ts.URL, "projects", "webhook", tc.action, "42",
			}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if captured.method != tc.method || captured.path != tc.path {
				t.Fatalf("request = %s %s", captured.method, captured.path)
			}
			if captured.auth != "Bearer admin" {
				t.Fatalf("auth = %q", captured.auth)
			}
			if !strings.Contains(stdout, `"managed_by": "crc"`) {
				t.Fatalf("stdout = %s", stdout)
			}
		})
	}
}

func TestAdminCommandValidation(t *testing.T) {
	for _, args := range [][]string{
		{"users", "bogus"},
		{"users", "disable"},
		{"users", "disable", "abc"},
		{"users", "revoke", "0"},
		{"projects", "grants"},
		{"projects", "grants", "set", "42", "7", "owner"},
		{"projects", "grants", "delete", "42"},
		{"projects", "webhook"},
		{"projects", "webhook", "enable"},
		{"projects", "webhook", "enable", "0"},
		{"projects", "webhook", "bogus", "42"},
	} {
		if _, _, err := runCtl(t, append([]string{"--url", "http://example.com"}, args...),
			map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil); err == nil {
			t.Fatalf("%v accepted", args)
		}
	}
}

func TestProjectsResiduesCommands(t *testing.T) {
	t.Run("list", func(t *testing.T) {
		var captured capturedRequest
		ts := httptest.NewServer(captureHandler(t, http.StatusOK,
			`{"residues":[{"project_id":42,"path":"/data/workspaces/42","kind":"workspace","removable":true}]}`, &captured))
		defer ts.Close()
		stdout, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "residues"},
			map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if captured.method != http.MethodGet || captured.path != "/api/v1/admin/projects/residues" {
			t.Fatalf("request = %s %s", captured.method, captured.path)
		}
		if !strings.Contains(stdout, `"project_id": 42`) {
			t.Fatalf("stdout = %s", stdout)
		}
	})

	t.Run("clean requires yes", func(t *testing.T) {
		var hits atomic.Int32
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
		}))
		defer ts.Close()
		_, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "residues", "clean"},
			map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
		if err == nil || !strings.Contains(err.Error(), "--yes") {
			t.Fatalf("error = %v", err)
		}
		if hits.Load() != 0 {
			t.Fatal("cleanup request was sent without --yes")
		}
	})

	t.Run("clean", func(t *testing.T) {
		var captured capturedRequest
		ts := httptest.NewServer(captureHandler(t, http.StatusOK,
			`{"removed":[{"project_id":42,"path":"/data/workspaces/42","kind":"workspace","removable":true}],"kept":[],"failed":[]}`, &captured))
		defer ts.Close()
		stdout, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "residues", "clean", "--yes"},
			map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if captured.method != http.MethodPost || captured.path != "/api/v1/admin/projects/residues/cleanup" {
			t.Fatalf("request = %s %s", captured.method, captured.path)
		}
		if captured.body["confirm"] != true {
			t.Fatalf("body = %v", captured.body)
		}
		if !strings.Contains(stdout, `"removed"`) {
			t.Fatalf("stdout = %s", stdout)
		}
	})

	t.Run("unknown action", func(t *testing.T) {
		if _, _, err := runCtl(t, []string{"--url", "http://example.com", "projects", "residues", "bogus"},
			map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, nil); err == nil {
			t.Fatal("unknown action accepted")
		}
	})
}

func TestRetryOnboardingCommand(t *testing.T) {
	var captured capturedRequest
	ts := httptest.NewServer(captureHandler(t, http.StatusAccepted,
		`{"job_id":"job-retry"}`, &captured))
	defer ts.Close()

	stdout, _, err := runCtl(t, []string{
		"--url", ts.URL, "projects", "retry-onboarding", "42",
	}, map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client())
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if captured.method != http.MethodPost || captured.path != "/api/v1/admin/projects/42/onboarding/retry" {
		t.Fatalf("request = %s %s", captured.method, captured.path)
	}
	if captured.auth != "Bearer admin" {
		t.Fatalf("auth = %q", captured.auth)
	}
	if captured.cookie != "" || captured.csrf != "" {
		t.Fatalf("project credentials sent: cookie=%q csrf=%q", captured.cookie, captured.csrf)
	}
	if !strings.Contains(stdout, "job-retry") {
		t.Fatalf("stdout = %q", stdout)
	}

	if _, _, err := runCtl(t, []string{"--url", ts.URL, "projects", "retry-onboarding", "abc"},
		map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, ts.Client()); err == nil {
		t.Fatal("invalid project ID accepted")
	}

	var notFound capturedRequest
	missing := httptest.NewServer(captureHandler(t, http.StatusNotFound,
		`{"error":{"code":"not_found","message":"project not found"}}`, &notFound))
	defer missing.Close()
	_, _, err = runCtl(t, []string{"--url", missing.URL, "projects", "retry-onboarding", "99"},
		map[string]string{"REVIEW_ADMIN_TOKEN": "admin"}, missing.Client())
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing project error = %v", err)
	}
}
