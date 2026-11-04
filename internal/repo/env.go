package repo

import (
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
)

// Env keys inherited from the process. Everything else, notably GIT_*,
// HOME, proxy variables and credential helpers, is dropped. Git repositories
// are treated as untrusted input, so their configuration must not reach the
// child process through the caller's environment.
var inheritedEnv = map[string]bool{
	"PATH":          true,
	"TMPDIR":        true,
	"LANG":          true,
	"LC_ALL":        true,
	"TZ":            true,
	"SSL_CERT_FILE": true,
	"SSL_CERT_DIR":  true,
	"SYSTEMROOT":    true,
	"WINDIR":        true,
	"COMSPEC":       true,
}

var envKeyRE = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateToken(token string) error {
	if len(token) > maxTokenLen {
		return errors.New("repo: token is too long")
	}
	if strings.ContainsAny(token, "\r\n\x00") {
		return errors.New("repo: token contains a control character")
	}
	return nil
}

// buildEnv returns a whitelisted environment for one git invocation. The
// token, when present, is injected as an http.extraHeader through Git's
// environment configuration. It never appears in argv, remotes or errors.
func (g *Git) buildEnv(a Auth) ([]string, error) {
	if a.Token != "" {
		if err := validateToken(a.Token); err != nil {
			return nil, err
		}
	}

	env := make(map[string]string, len(inheritedEnv)+len(g.BaseEnv)+12)
	for _, kv := range os.Environ() {
		key, value, ok := strings.Cut(kv, "=")
		if ok && inheritedEnv[key] {
			env[key] = value
		}
	}
	for _, kv := range g.BaseEnv {
		key, value, ok := strings.Cut(kv, "=")
		if !ok || !envKeyRE.MatchString(key) || unsafeEnvKey(key) {
			continue
		}
		env[key] = value
	}

	// Failure classification reads stable English messages. LC_ALL would
	// override LC_MESSAGES, so the message category is pinned explicitly.
	delete(env, "LC_ALL")
	env["LC_MESSAGES"] = "C"
	env["LANGUAGE"] = "C"
	env["GIT_CONFIG_NOSYSTEM"] = "1"
	env["GIT_CONFIG_GLOBAL"] = os.DevNull
	env["GIT_CONFIG_SYSTEM"] = os.DevNull
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_LFS_SKIP_SMUDGE"] = "1"
	env["GIT_ATTR_NOSYSTEM"] = "1"
	env["GIT_ALLOW_PROTOCOL"] = "http:https"
	env["GIT_PROTOCOL_FROM_USER"] = "0"
	env["GIT_ASKPASS"] = "/bin/false"
	env["SSH_ASKPASS"] = "/bin/false"
	env["GIT_SSH_COMMAND"] = "/bin/false"
	delete(env, "GIT_CONFIG_COUNT")

	if a.Token != "" {
		header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+a.Token))
		env["GIT_CONFIG_COUNT"] = "1"
		env["GIT_CONFIG_KEY_0"] = "http.extraHeader"
		env["GIT_CONFIG_VALUE_0"] = header
	}

	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	sort.Strings(out)
	return out, nil
}

func unsafeEnvKey(key string) bool {
	key = strings.ToUpper(key)
	return strings.HasPrefix(key, "GIT_") ||
		strings.HasPrefix(key, "GIT_CONFIG_") ||
		strings.HasPrefix(key, "SSH_") ||
		strings.HasPrefix(key, "LD_") ||
		strings.HasPrefix(key, "DYLD_") ||
		key == "HOME" ||
		key == "XDG_CONFIG_HOME"
}

// globalArgs disable hooks, filters, credential helpers, submodule and LFS
// work for every git command. The repository's own config is still read, so
// these command-line values take precedence over anything it defines.
func globalArgs() []string {
	return []string{
		"-c", "credential.helper=",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "protocol.file.allow=never",
		"-c", "submodule.recurse=false",
		"-c", "fetch.recurseSubmodules=false",
		"-c", "http.followRedirects=false",
		"-c", "filter.lfs.required=false",
		"-c", "advice.detachedHead=false",
	}
}

func wrapEnvError(err error) error {
	return fmt.Errorf("repo: environment: %w", err)
}
