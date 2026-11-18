package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
)

// TestApplyMigrationsRejectsInvalidNames covers #542: a SQL file that does not
// match NNNN_name.sql must fail startup with the offending filename before any
// SQL runs. Names the old version parser accepted, such as a one-digit version,
// are rejected too, and AppleDouble files from macOS archives are named.
func TestApplyMigrationsRejectsInvalidNames(t *testing.T) {
	valid := &fstest.MapFile{Data: []byte(`CREATE TABLE applied (id INTEGER)`)}

	cases := []struct {
		name  string
		files fstest.MapFS
		want  string
	}{
		{
			name:  "short version prefix",
			files: fstest.MapFS{"1_init.sql": valid},
			want:  "1_init.sql",
		},
		{
			name: "apple double",
			files: fstest.MapFS{
				"0001_init.sql":   valid,
				"._0001_init.sql": {Data: []byte("apple double")},
			},
			want: "._0001_init.sql",
		},
		{
			name:  "missing underscore",
			files: fstest.MapFS{"0001-init.sql": valid},
			want:  "0001-init.sql",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "review.db"))
			if err != nil {
				t.Fatalf("open raw db: %v", err)
			}
			defer db.Close()

			err = applyMigrations(context.Background(), db, newFakeClock().Now, tc.files)
			if err == nil {
				t.Fatalf("applyMigrations accepted invalid migration %q", tc.want)
			}
			var openErr *OpenError
			if !errors.As(err, &openErr) {
				t.Fatalf("error is not an *OpenError: %v", err)
			}
			if openErr.Stage != StageMigrate {
				t.Fatalf("stage = %q, want %q", openErr.Stage, StageMigrate)
			}
			if openErr.Migration != tc.want {
				t.Fatalf("migration = %q, want %q", openErr.Migration, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error does not name the file: %v", err)
			}
			if !strings.Contains(err.Error(), "NNNN_name.sql") {
				t.Fatalf("error does not state the required pattern: %v", err)
			}
		})
	}
}

// TestApplyMigrationsAcceptsStrictValidName checks that the strict pattern
// still loads normal migrations and ignores non-SQL files.
func TestApplyMigrationsAcceptsStrictValidName(t *testing.T) {
	files := fstest.MapFS{
		"0001_init.sql": &fstest.MapFile{Data: []byte(`CREATE TABLE applied (id INTEGER)`)},
		"README.md":     &fstest.MapFile{Data: []byte("not a migration")},
	}
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatalf("open raw db: %v", err)
	}
	defer db.Close()

	if err := applyMigrations(context.Background(), db, newFakeClock().Now, files); err != nil {
		t.Fatalf("applyMigrations rejected a valid migration: %v", err)
	}
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&count); err != nil {
		t.Fatalf("count schema_migrations: %v", err)
	}
	if count != 1 {
		t.Fatalf("schema_migrations = %d, want 1", count)
	}
}
