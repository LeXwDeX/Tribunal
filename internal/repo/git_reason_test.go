package repo

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestFetchFailureReasonFromRealGit(t *testing.T) {
	ctx := context.Background()
	source := initRepo(t)
	commitFile(t, source, "a.txt", "one\n", "first")
	serverRoot := t.TempDir()
	bare := filepath.Join(serverRoot, "repo.git")
	gitCmd(t, "", "clone", "--bare", source, bare)
	gitCmd(t, bare, "update-server-info")
	files := http.FileServer(http.Dir(serverRoot))

	cases := []struct {
		name    string
		status  int
		refspec string
		want    string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized, refspec: "+refs/heads/main:refs/workspace/base", want: ReasonAuth},
		{name: "forbidden", status: http.StatusForbidden, refspec: "+refs/heads/main:refs/workspace/base", want: ReasonForbidden},
		{name: "repository missing", status: http.StatusNotFound, refspec: "+refs/heads/main:refs/workspace/base", want: ReasonRepositoryNotFound},
		{name: "branch missing", refspec: "+refs/heads/master:refs/workspace/base", want: ReasonRefNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.status != 0 {
					if tc.status == http.StatusUnauthorized {
						w.Header().Set("WWW-Authenticate", `Basic realm="GitLab"`)
					}
					http.Error(w, "secret-body-marker", tc.status)
					return
				}
				files.ServeHTTP(w, r)
			}))
			defer server.Close()
			mirror := filepath.Join(t.TempDir(), "mirror.git")
			g := &Git{}
			if err := g.EnsureMirror(ctx, mirror, server.URL+"/repo.git", Auth{Token: "token-123"}); err != nil {
				t.Fatalf("EnsureMirror() error = %v", err)
			}
			err := g.Fetch(ctx, mirror, Auth{Token: "token-123"}, tc.refspec)
			if err == nil {
				t.Fatal("Fetch() = nil, want error")
			}
			if got := ReasonOf(err); got != tc.want {
				t.Fatalf("ReasonOf(%v) = %q, want %q", err, got, tc.want)
			}
			if strings.Contains(err.Error(), "secret-body-marker") || strings.Contains(err.Error(), "token-123") || strings.Contains(err.Error(), server.URL) {
				t.Fatalf("error leaked upstream detail: %v", err)
			}
		})
	}
}

func TestFetchFailureReasonNetwork(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := listener.Addr().String()
	listener.Close()
	ctx := context.Background()
	mirror := filepath.Join(t.TempDir(), "mirror.git")
	g := &Git{}
	if err := g.EnsureMirror(ctx, mirror, "http://"+addr+"/repo.git", Auth{}); err != nil {
		t.Fatalf("EnsureMirror() error = %v", err)
	}
	err = g.Fetch(ctx, mirror, Auth{}, "+refs/heads/main:refs/workspace/base")
	if got := ReasonOf(err); got != ReasonNetwork {
		t.Fatalf("ReasonOf(%v) = %q, want %q", err, got, ReasonNetwork)
	}
}

func TestFetchFailureReasonFromStderrStaysSanitized(t *testing.T) {
	dir := t.TempDir()
	secret := "sekret-token-value"
	g := &Git{
		Bin: fakeGit(t),
		BaseEnv: []string{
			"FAKE_ARGS=" + filepath.Join(dir, "args"),
			"FAKE_ENV=" + filepath.Join(dir, "env"),
			"FAKE_STDERR=fatal: couldn't find remote ref refs/heads/" + secret,
			"FAKE_EXIT=128",
		},
	}
	err := g.Fetch(context.Background(), t.TempDir(), Auth{Token: secret}, "+refs/heads/main:refs/workspace/base")
	var ge *Error
	if !errors.As(err, &ge) || ge.Reason != ReasonRefNotFound || ge.ExitCode != 128 {
		t.Fatalf("error = %#v, want ref_not_found exit 128", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked stderr: %v", err)
	}
	if got := err.Error(); got != "repo: fetch failed (exit 128, ref_not_found)" {
		t.Fatalf("Error() = %q", got)
	}
}

func TestClassifyGitStderr(t *testing.T) {
	cases := map[string]string{
		"fatal: Authentication failed for 'https://host/x.git/'":                                                                                                  ReasonAuth,
		"remote: HTTP Basic: Access denied\nfatal: Authentication failed":                                                                                         ReasonAuth,
		"fatal: could not read Username for 'https://host': terminal prompts disabled":                                                                            ReasonAuth,
		"fatal: unable to access 'https://host/x.git/': The requested URL returned error: 403":                                                                    ReasonForbidden,
		"remote: You are not allowed to download code from this project.\nfatal: unable to access":                                                                ReasonForbidden,
		"remote: The project you were looking for could not be found or you don't have permission to view it.\nfatal: repository 'https://host/x.git/' not found": ReasonRepositoryNotFound,
		"fatal: couldn't find remote ref refs/heads/master":                                                                                                       ReasonRefNotFound,
		"fatal: unable to access 'https://host/': Could not resolve host: host":                                                                                   ReasonNetwork,
		"fatal: unable to access 'https://host/': Failed to connect to host port 443":                                                                             ReasonNetwork,
		"error: RPC failed; curl 56 Recv failure: Connection reset by peer":                                                                                       ReasonNetwork,
		"fatal: unable to access 'https://host/': SSL certificate problem: unable to get local issuer":                                                            ReasonNetwork,
		"fatal: something else entirely":                                                                                                                          "",
		"":                                                                                                                                                        "",
	}
	for stderr, want := range cases {
		if got := classifyGitStderr([]byte(stderr)); got != want {
			t.Errorf("classifyGitStderr(%q) = %q, want %q", stderr, got, want)
		}
	}
}
