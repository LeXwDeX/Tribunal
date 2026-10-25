package app

import (
	"context"

	"github.com/LeXwDeX/Code-Review-For-Gitlab-CE/internal/repo"
)

// FileDiff exposes the zero-context diff capability F8 needs without widening
// the shared GitOps interface.
func (r repoGit) FileDiff(ctx context.Context, dir, from, to, path string) (*repo.FileDiff, error) {
	return r.git.FileDiff(ctx, dir, from, to, path)
}

var _ feedbackDiff = repoGit{}
