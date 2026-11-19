package server

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func loginWithReturnTo(t *testing.T, env *authEnv, client *http.Client, returnTo string) *url.URL {
	t.Helper()
	path := "/auth/login"
	if returnTo != "" {
		path += "?return_to=" + url.QueryEscape(returnTo)
	}
	response := env.request(client, http.MethodGet, path, nil, nil)
	if response.status != http.StatusFound {
		t.Fatalf("login status = %d, body=%s", response.status, response.body)
	}
	parsed, err := url.Parse(response.header.Get("Location"))
	if err != nil {
		t.Fatalf("parse login Location: %v", err)
	}
	if parsed.Path != "/oauth/authorize" {
		t.Fatalf("login Location = %q", response.header.Get("Location"))
	}
	return parsed
}

func TestLoginReturnToRedirectsAfterCallback(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	authorize := loginWithReturnTo(t, env, client, "/reviews/wf-1?tab=findings")
	state := authorize.Query().Get("state")
	if state == "" {
		t.Fatal("login redirect has no state")
	}
	callback := env.callback(client, "code-admin", state)
	if callback.status != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
	}
	if location := callback.header.Get("Location"); location != "/reviews/wf-1?tab=findings" {
		t.Fatalf("callback Location = %q, want the original deep link", location)
	}
}

// TestLoginReturnToLengthBoundary pins the return_to limit and the state limit
// together. A 315-byte return_to encodes to a 512-byte state, and 316 bytes
// encode to 514: the two limits must not disagree, or a legitimate deep link
// is rejected on the first login and lost.
func TestLoginReturnToLengthBoundary(t *testing.T) {
	for _, size := range []int{315, 316, 512} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			returnTo := "/" + strings.Repeat("a", size-1)
			if len(returnTo) != size {
				t.Fatalf("return_to length = %d, want %d", len(returnTo), size)
			}
			env := newAuthEnv(t)
			client := env.client()
			authorize := loginWithReturnTo(t, env, client, returnTo)
			callback := env.callback(client, "code-admin", authorize.Query().Get("state"))
			if callback.status != http.StatusSeeOther {
				t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
			}
			if location := callback.header.Get("Location"); location != returnTo {
				t.Fatalf("callback Location length = %d, want the %d-byte deep link", len(location), size)
			}
		})
	}
}

// expandableReturnTo builds a deep link of exactly size bytes whose
// percent-encoded form is three times as long. A space and each byte of a
// Chinese character both become three percent-encoded bytes, which is the
// worst case for the signed state that carries the link. Spaces are kept
// between the leading slash and the Chinese characters so the trailing space
// trim of safeReturnTo does not shorten the link.
func expandableReturnTo(size int) string {
	chars := (size - 1) / 3
	spaces := size - 1 - chars*3
	return "/" + strings.Repeat(" ", spaces) + strings.Repeat("中", chars)
}

// TestLoginReturnToEncodedDeepLinkSurvivesCallback pins that a deep link the
// login accepts is not rejected by its own state limit after percent-encoding.
// The decoded link stays at or below maxReturnTo, but spaces and Chinese
// characters encode to three times their decoded length, so the state is much
// longer than the decoded deep link. Before the state limit covered the
// encoded form, the callback rejected the state this service had just issued
// and the first login failed with 400.
func TestLoginReturnToEncodedDeepLinkSurvivesCallback(t *testing.T) {
	for _, size := range []int{315, 316, 511, 512} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			returnTo := expandableReturnTo(size)
			if len(returnTo) != size {
				t.Fatalf("return_to length = %d, want %d", len(returnTo), size)
			}
			parsed, err := url.Parse(returnTo)
			if err != nil {
				t.Fatalf("parse return_to: %v", err)
			}
			want := parsed.RequestURI()
			if len(want) <= size {
				t.Fatalf("encoded return_to length = %d, want more than %d", len(want), size)
			}
			env := newAuthEnv(t)
			client := env.client()
			authorize := loginWithReturnTo(t, env, client, returnTo)
			state := authorize.Query().Get("state")
			if len(state) > maxState {
				t.Fatalf("state length = %d, want <= %d", len(state), maxState)
			}
			callback := env.callback(client, "code-admin", state)
			if callback.status != http.StatusSeeOther {
				t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
			}
			if location := callback.header.Get("Location"); location != want {
				t.Fatalf("callback Location = %q, want the percent-encoded deep link %q", location, want)
			}
		})
	}
}

// TestLoginReturnToEncodedQuerySurvivesCallback covers a small deep link with
// a Chinese path and a query value containing spaces, so the encoded form is
// longer than the decoded one without reaching the boundary.
func TestLoginReturnToEncodedQuerySurvivesCallback(t *testing.T) {
	returnTo := "/reviews/我的 项目?tab=a b"
	parsed, err := url.Parse(returnTo)
	if err != nil {
		t.Fatalf("parse return_to: %v", err)
	}
	want := parsed.RequestURI()
	if len(want) <= len(returnTo) {
		t.Fatalf("encoded return_to length = %d, want more than %d", len(want), len(returnTo))
	}
	env := newAuthEnv(t)
	client := env.client()
	authorize := loginWithReturnTo(t, env, client, returnTo)
	callback := env.callback(client, "code-admin", authorize.Query().Get("state"))
	if callback.status != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
	}
	if location := callback.header.Get("Location"); location != want {
		t.Fatalf("callback Location = %q, want the percent-encoded deep link %q", location, want)
	}
}

// TestSafeStateReturnToOverEncodedLimitFallsBack pins the explicit fallback
// rule for the encoded deep link carried by a state: one byte over the encoded
// limit is dropped rather than redirected.
func TestSafeStateReturnToOverEncodedLimitFallsBack(t *testing.T) {
	for _, size := range []int{maxEncodedReturnTo, maxEncodedReturnTo + 1} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			value := "/" + strings.Repeat("a", size-1)
			got := safeStateReturnTo(value)
			if size <= maxEncodedReturnTo {
				if got != value {
					t.Fatalf("safeStateReturnTo length = %d, want the %d-byte deep link", len(got), size)
				}
				return
			}
			if got != "/" {
				t.Fatalf("safeStateReturnTo length = %d, want /", len(got))
			}
		})
	}
}

// TestIssuedStateAlwaysValid pins that a state carrying the longest deep link
// the login accepts still passes validState on the callback.
func TestIssuedStateAlwaysValid(t *testing.T) {
	auth, err := newAuthService(AuthConfig{SessionKey: []byte("0123456789abcdef0123456789abcdef")})
	if err != nil {
		t.Fatalf("newAuthService: %v", err)
	}
	returnTo := safeReturnTo(expandableReturnTo(512))
	if len(returnTo) <= maxReturnTo {
		t.Fatalf("encoded return_to length = %d, want more than the decoded %d-byte limit", len(returnTo), maxReturnTo)
	}
	state, err := auth.issueState(returnTo)
	if err != nil {
		t.Fatalf("issueState: %v", err)
	}
	if len(state) > maxState {
		t.Fatalf("state length = %d, want <= %d", len(state), maxState)
	}
	if !auth.validState(state) {
		t.Fatal("validState rejected state issued for an accepted deep link")
	}
	if got := auth.stateReturnTo(state); got != returnTo {
		t.Fatalf("stateReturnTo = %d bytes, want the %d-byte encoded deep link", len(got), len(returnTo))
	}
}

// TestLoginReturnToOverLimitFallsBack keeps the upper bound: one byte over the
// accepted length is still dropped rather than carried into the state.
func TestLoginReturnToOverLimitFallsBack(t *testing.T) {
	returnTo := "/" + strings.Repeat("a", 512)
	env := newAuthEnv(t)
	client := env.client()
	authorize := loginWithReturnTo(t, env, client, returnTo)
	callback := env.callback(client, "code-admin", authorize.Query().Get("state"))
	if callback.status != http.StatusSeeOther {
		t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
	}
	if location := callback.header.Get("Location"); location != "/" {
		t.Fatalf("callback Location length = %d, want /", len(location))
	}
}

func TestLoginRejectsUnsafeReturnTo(t *testing.T) {
	for _, returnTo := range []string{
		"https://evil.example/steal",
		"//evil.example/steal",
		"/\\evil.example",
		"javascript:alert(1)",
		"auth/callback",
		"/auth/logout",
		"",
	} {
		t.Run(returnTo, func(t *testing.T) {
			env := newAuthEnv(t)
			client := env.client()
			authorize := loginWithReturnTo(t, env, client, returnTo)
			callback := env.callback(client, "code-admin", authorize.Query().Get("state"))
			if callback.status != http.StatusSeeOther {
				t.Fatalf("callback status = %d, body=%s", callback.status, callback.body)
			}
			if location := callback.header.Get("Location"); location != "/" {
				t.Fatalf("return_to %q produced Location %q, want /", returnTo, location)
			}
		})
	}
}

func TestCSRFInvalidHasItsOwnErrorCode(t *testing.T) {
	env := newAuthEnv(t)
	client := env.client()
	env.loginAs(client, "code-admin")

	response := env.request(client, http.MethodPatch, "/api/v1/admin/users/1",
		[]byte(`{"role":"viewer","revision":1}`),
		map[string]string{"Content-Type": "application/json", csrfHeaderName: "stale-token"})
	if response.status != http.StatusForbidden {
		t.Fatalf("csrf status = %d, body=%s", response.status, response.body)
	}
	body := decodeMap(t, response.body)
	envelope := body["error"].(map[string]any)
	if envelope["code"] != "csrf_invalid" {
		t.Fatalf("csrf error code = %v, want csrf_invalid", envelope["code"])
	}
}
