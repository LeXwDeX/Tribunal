package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
)

// TestOpenWarnsOnPreexistingForeignKeyViolations covers #541: when a database
// already records every migration but contains orphan rows, startup must run
// the check, log a structured warning with the table and count, and continue
// instead of skipping the check or refusing to start.
func TestOpenWarnsOnPreexistingForeignKeyViolations(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "review.db")

	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	// Foreign keys are off on this plain connection, so the orphan is accepted.
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	if _, err := raw.Exec(`INSERT INTO context_docs
		(id, project_id, base_sha, kind, source, content, created_at)
		VALUES ('orphan', 999, 'sha', 'agents', 'generated', 'body', '2026-11-18T10:00:00.000000000Z')`); err != nil {
		raw.Close()
		t.Fatalf("insert orphan context doc: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw db: %v", err)
	}

	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	s, err := Open(ctx, path, WithLogger(logger))
	if err != nil {
		t.Fatalf("Open with pre-existing orphan: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close warned store: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"WARN", "foreign key", "table=context_docs", "violations=1"} {
		if !strings.Contains(out, want) {
			t.Errorf("warning log missing %q: %s", want, out)
		}
	}

	// The same database still starts without a logger: a violation is reported,
	// not fatal, when this startup applied no migration.
	plain, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open without logger: %v", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatalf("close plain store: %v", err)
	}
}
