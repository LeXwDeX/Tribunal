package sqlite

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func BenchmarkProjectListGlobalRead(b *testing.B) {
	ctx := context.Background()
	s, err := Open(ctx, filepath.Join(b.TempDir(), "bench.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	if _, err := s.db.ExecContext(ctx, `INSERT INTO users (id, username, name, role, global_read) VALUES
		(1, 'global', 'Global', 'viewer', 1), (2, 'granted', 'Granted', 'viewer', 0)`); err != nil {
		b.Fatal(err)
	}
	for id := 1; id <= 10; id++ {
		if _, err := s.db.ExecContext(ctx, `INSERT INTO projects (id, path_with_namespace, web_url, default_branch, token_enc, webhook_secret_enc, created_at, updated_at)
			VALUES (?, 'group/project', 'https://example.test', 'main', x'00', x'00', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`, id); err != nil {
			b.Fatal(err)
		}
		if err := s.SetProjectGrant(ctx, int64(id), 2, store.ProjectAccessRead); err != nil {
			b.Fatal(err)
		}
	}
	for _, tc := range []struct {
		name string
		user int64
	}{{"global", 1}, {"granted", 2}} {
		b.Run(tc.name, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				projects, _, err := s.ListProjectsFilteredPage(ctx, store.ProjectFilter{AccessUserID: &tc.user}, "", 20)
				if err != nil || len(projects) != 10 {
					b.Fatalf("projects = %d, %v", len(projects), err)
				}
			}
		})
	}
}
