package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// RunIndex runs each configured x-review.index_command once for baseDir. It is
// an administrator action, never a model tool. Commands run without a shell,
// with the same restricted environment as stdio servers, and their output is
// discarded. The first failure stops the sequence and returns a message that
// does not include arguments or command output. The count is the number of
// commands that completed successfully; a server without an index command
// contributes nothing.
func (m *Manager) RunIndex(ctx context.Context, baseDir string) (int, error) {
	if m == nil {
		return 0, ErrClosed
	}
	m.mu.Lock()
	closed := m.closed
	m.mu.Unlock()
	if closed {
		return 0, ErrClosed
	}
	base, err := existingDir(baseDir, "base directory")
	if err != nil {
		return 0, err
	}
	names := make([]string, 0)
	for name, cfg := range m.servers {
		if len(cfg.XReview.IndexCommand) > 0 {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	count := 0
	for _, name := range names {
		m.mu.Lock()
		closed := m.closed
		m.mu.Unlock()
		if closed {
			return count, ErrClosed
		}
		cfg := m.servers[name]
		if err := m.runIndexOne(ctx, name, cfg, base); err != nil {
			return count, err
		}
		count++
	}
	return count, nil
}

func (m *Manager) runIndexOne(ctx context.Context, server string, cfg ServerConfig, baseDir string) error {
	argv := make([]string, len(cfg.XReview.IndexCommand))
	for i, arg := range cfg.XReview.IndexCommand {
		argv[i] = strings.ReplaceAll(arg, "{base_dir}", baseDir)
		argv[i] = strings.ReplaceAll(argv[i], "{workspace}", m.workspace)
	}
	env, _, err := m.env.build(cfg, m.workspace, server)
	if err != nil {
		return fmt.Errorf("mcp: index command for %q: environment rejected", server)
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = baseDir
	cmd.Env = env
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("mcp: index command for %q: %w", server, ctx.Err())
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("mcp: index command for %q failed (exit %d)", server, exitErr.ExitCode())
		}
		return fmt.Errorf("mcp: index command for %q failed", server)
	}
	return nil
}

func existingDir(path, what string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("mcp: %s is required", what)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("mcp: %s is invalid", what)
	}
	absolute = filepath.Clean(absolute)
	if filepath.Dir(absolute) == absolute {
		return "", fmt.Errorf("mcp: %s must not be a filesystem root", what)
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("mcp: inspect %s: %w", what, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return "", fmt.Errorf("mcp: %s must be a real directory", what)
	}
	return absolute, nil
}
