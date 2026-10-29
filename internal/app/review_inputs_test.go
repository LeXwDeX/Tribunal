package app

import (
	"errors"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/config"
	"github.com/LeXwDeX/tribunal/internal/review"
	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestResolveSecureReviewConfigRejectsInvalidYAML(t *testing.T) {
	project := &store.Project{ID: 1, Settings: []byte(`{}`)}
	docs := review.RepoDocs{GitLabReviewConfig: "ignore: [\n"}
	defaults := config.ProjectSettings{AllowRepoConfig: true}
	_, err := resolveSecureReviewConfig(project, docs, defaults, testProfiles())
	if !errors.Is(err, ErrInvalidRepoConfig) {
		t.Fatalf("error = %v, want ErrInvalidRepoConfig", err)
	}
}
