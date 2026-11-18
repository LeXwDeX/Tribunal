package repo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFileDiffOversizedHunksReportIncomparable covers the failure scenario of
// #546: a published finding points at a file that the author rewrites as one
// huge blob of text between two heads. The zero-context diff is larger than
// the per-file bound, so it must be reported as incomparable instead of being
// buffered without a limit or retried as an error on every feedback round.
func TestFileDiffOversizedHunksReportIncomparable(t *testing.T) {
	dir := initRepo(t)
	oldBody, newBody := oversizedRewrite(30000)
	writeDiffFile(t, dir, "big.txt", oldBody)
	gitCmd(t, dir, "add", "big.txt")
	gitCmd(t, dir, "commit", "-q", "-m", "base")
	from := trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))
	writeDiffFile(t, dir, "big.txt", newBody)
	gitCmd(t, dir, "add", "big.txt")
	gitCmd(t, dir, "commit", "-q", "-m", "rewrite")
	to := trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))

	change, err := (&Git{}).FileDiff(context.Background(), dir, from, to, "big.txt")
	if err != nil {
		t.Fatalf("FileDiff() error = %v, want an incomparable result", err)
	}
	if change == nil {
		t.Fatal("FileDiff() = nil, want an incomparable result")
	}
	if !change.Incomparable {
		t.Fatalf("FileDiff().Incomparable = false, want true for a diff above %d bytes", maxFileDiffOutput)
	}
	if change.Binary {
		t.Fatal("FileDiff().Binary = true, want a text diff above the bound")
	}
	if len(change.Hunks) != 0 {
		t.Fatalf("FileDiff().Hunks = %d entries, want none when the output is discarded", len(change.Hunks))
	}
}

// oversizedRewrite returns two bodies whose zero-context diff exceeds the
// per-file bound. Every line differs, so the diff carries both sides in full.
func oversizedRewrite(lines int) (string, string) {
	oldBody := &strings.Builder{}
	newBody := &strings.Builder{}
	oldBody.Grow(lines * 40)
	newBody.Grow(lines * 40)
	for i := 0; i < lines; i++ {
		oldBody.WriteString("old line with a long payload number ")
		oldBody.WriteString(paddedNumber(i))
		oldBody.WriteString("\n")
		newBody.WriteString("new line with a long payload number ")
		newBody.WriteString(paddedNumber(i))
		newBody.WriteString("\n")
	}
	return oldBody.String(), newBody.String()
}

func paddedNumber(value int) string {
	raw := []byte("0000000000")
	for i := len(raw) - 1; i >= 0 && value > 0; i-- {
		raw[i] = byte('0' + value%10)
		value /= 10
	}
	return string(raw)
}

func TestRunBoundedOutputLimit(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-git.sh")
	body := `#!/bin/sh
n=${FAKE_BYTES:-0}
if [ "$n" -gt 0 ]; then
  head -c "$n" /dev/zero | tr '\0' 'x'
fi
exit "${FAKE_EXIT:-0}"
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		env        []string
		wantLimit  bool
		wantOutput int
	}{
		{name: "under the limit", env: []string{"FAKE_BYTES=64"}, wantOutput: 64},
		{name: "above the limit with success exit", env: []string{"FAKE_BYTES=4096"}, wantLimit: true},
		{name: "above the limit with failing exit", env: []string{"FAKE_BYTES=4096", "FAKE_EXIT=3"}, wantLimit: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := &Git{Bin: script, BaseEnv: tc.env}
			out, err := g.runBounded(context.Background(), "fake", "", Auth{}, time.Second, 1024, "status")
			if tc.wantLimit {
				if !errors.Is(err, ErrOutputLimit) {
					t.Fatalf("runBounded() error = %v, want ErrOutputLimit", err)
				}
				if out != nil {
					t.Fatalf("runBounded() output = %d bytes, want none when the bound is hit", len(out))
				}
				return
			}
			if err != nil {
				t.Fatalf("runBounded() error = %v", err)
			}
			if len(out) != tc.wantOutput {
				t.Fatalf("runBounded() output = %d bytes, want %d", len(out), tc.wantOutput)
			}
		})
	}
}

func TestRunBoundedOutputLimitGenuineFailure(t *testing.T) {
	script := filepath.Join(t.TempDir(), "fake-git.sh")
	body := `#!/bin/sh
head -c 64 /dev/zero | tr '\0' 'x'
exit 3
`
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	g := &Git{Bin: script}
	_, err := g.runBounded(context.Background(), "fake", "", Auth{}, time.Second, 1024, "status")
	var gitErr *Error
	if !errors.As(err, &gitErr) || gitErr.ExitCode != 3 {
		t.Fatalf("runBounded() error = %v, want *Error with exit 3", err)
	}
	if errors.Is(err, ErrOutputLimit) {
		t.Fatal("runBounded() error = ErrOutputLimit, want a genuine command failure")
	}
}
