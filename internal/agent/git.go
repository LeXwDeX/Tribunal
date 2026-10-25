package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/LeXwDeX/tribunal/internal/llm"
)

const (
	maxGitOutputBytes = 256 << 10
	gitTimeout        = 30 * time.Second
	gitWaitDelay      = 5 * time.Second
)

// inheritedGitEnv is the only environment inherited by git. Credentials,
// proxies, HOME and every GIT_* variable are dropped.
var inheritedGitEnv = map[string]bool{
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

var gitDangerousPathRE = regexp.MustCompile(`^[:\-]`)

type gitLogInput struct {
	Path string `json:"path,omitempty"`
	Max  int    `json:"max,omitempty"`
	Root string `json:"root,omitempty"`
}

type gitLogTool struct{ sb *Sandbox }

func (t gitLogTool) Def() llm.ToolDef {
	return def("git_log",
		"Read-only git log --oneline for the selected root.",
		`{"type":"object","properties":{
			"path":{"type":"string"},
			"max":{"type":"integer","minimum":1,"maximum":100},
			"root":{"type":"string"}},
			"additionalProperties":false}`)
}

func (t gitLogTool) Call(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in gitLogInput
	if err := decodeInput(raw, &in); err != nil {
		return Result{}, err
	}
	max := in.Max
	if max == 0 {
		max = 20
	}
	if max < 1 || max > 100 {
		return Result{}, errors.New("max must be between 1 and 100")
	}
	rel, err := t.sb.gitPath(in.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	out, truncated, err := t.sb.runGit(ctx, in.Root, "log", "log", "--oneline", "--no-decorate", "--no-show-signature",
		"-n", strconv.Itoa(max), "--", rel)
	if err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(out) == "" {
		return Result{Content: "(no commits)"}, nil
	}
	return gitResult(out, truncated)
}

type gitBlameInput struct {
	Path      string `json:"path"`
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
	Root      string `json:"root,omitempty"`
}

type gitBlameTool struct{ sb *Sandbox }

func (t gitBlameTool) Def() llm.ToolDef {
	return def("git_blame",
		"Read-only git blame for a line range of one file.",
		`{"type":"object","required":["path","start_line","end_line"],"properties":{
			"path":{"type":"string"},
			"start_line":{"type":"integer","minimum":1},
			"end_line":{"type":"integer","minimum":1},
			"root":{"type":"string"}},
			"additionalProperties":false}`)
}

func (t gitBlameTool) Call(ctx context.Context, raw json.RawMessage) (Result, error) {
	var in gitBlameInput
	if err := decodeInput(raw, &in); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Path) == "" {
		return Result{}, errors.New("path is required")
	}
	if in.StartLine < 1 || in.EndLine < in.StartLine {
		return Result{}, errors.New("line range must satisfy 1 <= start_line <= end_line")
	}
	if in.EndLine-in.StartLine+1 > maxGitBlameLines {
		return Result{}, fmt.Errorf("line range exceeds %d lines", maxGitBlameLines)
	}
	rel, err := t.sb.gitPath(in.Root, in.Path)
	if err != nil {
		return Result{}, err
	}
	if !t.sb.isRegularFile(in.Root, in.Path) {
		return Result{}, fmt.Errorf("%s is not a regular file", rel)
	}
	rangeArg := fmt.Sprintf("-L%d,%d", in.StartLine, in.EndLine)
	out, truncated, err := t.sb.runGit(ctx, in.Root, "blame", "blame", "--date=short", "--no-textconv", rangeArg, "--", rel)
	if err != nil {
		return Result{}, err
	}
	return gitResult(out, truncated)
}

func gitResult(out string, truncated bool) (Result, error) {
	content := strings.TrimRight(out, "\n")
	if truncated {
		content += "\n[truncated: git output limit reached]"
	}
	return Result{Content: content, Truncated: truncated}, nil
}

// gitPath validates a tool-supplied path and returns its slash form.
func (s *Sandbox) gitPath(rootName, rel string) (string, error) {
	root, _, clean, err := s.resolve(rootName, rel)
	if err != nil {
		return "", err
	}
	if clean == "." {
		return ".", nil
	}
	if gitDangerousPathRE.MatchString(clean) {
		return "", fmt.Errorf("%w: git paths must not start with '-' or ':'", ErrUnsafePath)
	}
	if _, err := root.Stat(filepath.FromSlash(clean)); err != nil {
		return "", fmt.Errorf("agent: stat %s: %w", clean, err)
	}
	return clean, nil
}

func (s *Sandbox) isRegularFile(rootName, rel string) bool {
	root, _, clean, err := s.resolve(rootName, rel)
	if err != nil {
		return false
	}
	info, err := root.Stat(filepath.FromSlash(clean))
	return err == nil && info.Mode().IsRegular()
}

func (s *Sandbox) runGit(ctx context.Context, rootName, op string, args ...string) (string, bool, error) {
	if s == nil {
		return "", false, errors.New("agent: sandbox is closed")
	}
	if rootName == "" {
		rootName = s.def
	}
	rootPath, ok := s.paths[rootName]
	if !ok || rootPath == "" {
		return "", false, fmt.Errorf("%w: %q", ErrUnknownRoot, rootName)
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, gitTimeout)
	defer cancel()

	cmdArgs := []string{
		"--no-pager",
		"-C", rootPath,
		"-c", "core.pager=cat",
		"-c", "pager.log=false",
		"-c", "pager.blame=false",
		"-c", "core.hooksPath=" + os.DevNull,
		"-c", "credential.helper=",
		"-c", "protocol.file.allow=never",
		"-c", "core.fsmonitor=false",
		"-c", "diff.external=",
		"-c", "filter.lfs.required=false",
	}
	cmdArgs = append(cmdArgs, args...)

	cmd := exec.CommandContext(timeoutCtx, "git", cmdArgs...)
	cmd.Env = gitEnv(rootPath)
	cmd.Stderr = io.Discard
	cmd.WaitDelay = gitWaitDelay
	buf := &boundedBuffer{limit: maxGitOutputBytes}
	cmd.Stdout = buf

	runErr := cmd.Run()
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	if timeoutCtx.Err() != nil {
		return "", false, fmt.Errorf("git %s timed out", op)
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			return "", false, fmt.Errorf("git %s failed (exit %d)", op, exitErr.ExitCode())
		}
		return "", false, fmt.Errorf("git %s failed: %w", op, runErr)
	}
	return buf.String(), buf.truncated, nil
}

func gitEnv(rootPath string) []string {
	env := make(map[string]string, len(inheritedGitEnv)+16)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok && inheritedGitEnv[key] {
			env[key] = value
		}
	}
	env["GIT_CONFIG_NOSYSTEM"] = "1"
	env["GIT_CONFIG_GLOBAL"] = os.DevNull
	env["GIT_CONFIG_SYSTEM"] = os.DevNull
	env["GIT_TERMINAL_PROMPT"] = "0"
	env["GIT_PAGER"] = "cat"
	env["PAGER"] = "cat"
	env["GIT_OPTIONAL_LOCKS"] = "0"
	env["GIT_LFS_SKIP_SMUDGE"] = "1"
	env["GIT_ATTR_NOSYSTEM"] = "1"
	env["GIT_LITERAL_PATHSPECS"] = "1"
	env["GIT_ASKPASS"] = "/bin/false"
	env["SSH_ASKPASS"] = "/bin/false"
	if rootPath != "" {
		// Do not discover a repository above the sandbox root.
		env["GIT_CEILING_DIRECTORIES"] = filepath.Dir(rootPath)
	}

	out := make([]string, 0, len(env))
	for key, value := range env {
		out = append(out, key+"="+value)
	}
	return out
}

type boundedBuffer struct {
	limit     int
	buf       bytes.Buffer
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	b.buf.Write(p)
	return len(p), nil
}

func (b *boundedBuffer) String() string { return b.buf.String() }
