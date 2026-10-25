package repo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFileDiffLineChanges(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		mutate func(t *testing.T, dir string)
		want   []LineChange
	}{
		{
			name: "replace",
			path: "main.go",
			mutate: func(t *testing.T, dir string) {
				writeDiffFile(t, dir, "main.go", "one\ntwo\nthree changed\nfour\nfive\nsix\n")
			},
			want: []LineChange{{OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1}},
		},
		{
			name: "insert before finding line",
			path: "main.go",
			mutate: func(t *testing.T, dir string) {
				writeDiffFile(t, dir, "main.go", "one\ntwo\ninserted\nthree\nfour\nfive\nsix\n")
			},
			want: []LineChange{{OldStart: 2, OldLines: 0, NewStart: 3, NewLines: 1}},
		},
		{
			name: "delete",
			path: "main.go",
			mutate: func(t *testing.T, dir string) {
				writeDiffFile(t, dir, "main.go", "one\ntwo\nfour\nfive\nsix\n")
			},
			want: []LineChange{{OldStart: 3, OldLines: 1, NewStart: 2, NewLines: 0}},
		},
		{
			name: "pure rename",
			path: "main.go",
			mutate: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, "main.go"), filepath.Join(dir, "renamed.go")); err != nil {
					t.Fatal(err)
				}
			},
			want: nil,
		},
		{
			name: "rename and replace",
			path: "main.go",
			mutate: func(t *testing.T, dir string) {
				if err := os.Rename(filepath.Join(dir, "main.go"), filepath.Join(dir, "renamed.go")); err != nil {
					t.Fatal(err)
				}
				writeDiffFile(t, dir, "renamed.go", "one\ntwo\nthree changed\nfour\nfive\nsix\n")
			},
			want: []LineChange{{OldStart: 3, OldLines: 1, NewStart: 3, NewLines: 1}},
		},
		{
			name: "same file other line",
			path: "main.go",
			mutate: func(t *testing.T, dir string) {
				writeDiffFile(t, dir, "main.go", "one changed\ntwo\nthree\nfour\nfive\nsix\n")
			},
			want: []LineChange{{OldStart: 1, OldLines: 1, NewStart: 1, NewLines: 1}},
		},
		{
			name: "unrelated file",
			path: "main.go",
			mutate: func(t *testing.T, dir string) {
				writeDiffFile(t, dir, "other.go", "other\n")
			},
			want: nil,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir, from, to := diffRepo(t, tc.mutate)
			change, err := (&Git{}).FileDiff(context.Background(), dir, from, to, tc.path)
			if err != nil {
				t.Fatalf("FileDiff: %v", err)
			}
			if tc.want == nil {
				if change != nil {
					if len(change.Hunks) != 0 {
						t.Fatalf("FileDiff hunks = %+v, want none", change.Hunks)
					}
					if tc.name == "unrelated file" {
						t.Fatalf("FileDiff = %+v, want nil for an unchanged path", change)
					}
				}
				return
			}
			if change == nil {
				t.Fatalf("FileDiff = nil, want hunks %+v", tc.want)
			}
			if !reflect.DeepEqual(change.Hunks, tc.want) {
				t.Fatalf("FileDiff hunks = %+v, want %+v", change.Hunks, tc.want)
			}
		})
	}
}

func TestFileDiffPureRenameKeepsBothPaths(t *testing.T) {
	dir, from, to := diffRepo(t, func(t *testing.T, dir string) {
		if err := os.Rename(filepath.Join(dir, "main.go"), filepath.Join(dir, "renamed.go")); err != nil {
			t.Fatal(err)
		}
	})
	change, err := (&Git{}).FileDiff(context.Background(), dir, from, to, "main.go")
	if err != nil || change == nil {
		t.Fatalf("FileDiff = %+v, %v", change, err)
	}
	if change.OldPath != "main.go" || change.NewPath != "renamed.go" || change.Deleted || change.Binary || change.Incomparable {
		t.Fatalf("pure rename change = %+v", change)
	}
	if len(change.Hunks) != 0 {
		t.Fatalf("pure rename hunks = %+v", change.Hunks)
	}
}

func TestFileDiffDeletedBinaryAndIncomparable(t *testing.T) {
	dir, from, to := diffRepo(t, func(t *testing.T, dir string) {
		if err := os.Remove(filepath.Join(dir, "main.go")); err != nil {
			t.Fatal(err)
		}
	})
	change, err := (&Git{}).FileDiff(context.Background(), dir, from, to, "main.go")
	if err != nil || change == nil || !change.Deleted || change.NewPath != "" {
		t.Fatalf("deleted FileDiff = %+v, %v", change, err)
	}

	dir, from, to = diffRepo(t, func(t *testing.T, dir string) {
		if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte{0, 1, 2, 0}, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	change, err = (&Git{}).FileDiff(context.Background(), dir, from, to, "main.go")
	if err != nil || change == nil || !change.Binary {
		t.Fatalf("binary FileDiff = %+v, %v", change, err)
	}

	dir, from, to = diffRepo(t, func(t *testing.T, dir string) {
		writeDiffFile(t, dir, "added.go", "added\n")
	})
	change, err = (&Git{}).FileDiff(context.Background(), dir, from, to, "added.go")
	if err != nil || change == nil || !change.Incomparable {
		t.Fatalf("added FileDiff = %+v, %v", change, err)
	}
}

func TestFileDiffRenameWithSpaces(t *testing.T) {
	dir := initRepo(t)
	writeDiffFile(t, dir, "a b.txt", "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\ntwelve\n")
	gitCmd(t, dir, "add", ".")
	gitCmd(t, dir, "commit", "-q", "-m", "base")
	from := trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))
	if err := os.Rename(filepath.Join(dir, "a b.txt"), filepath.Join(dir, "c d.txt")); err != nil {
		t.Fatal(err)
	}
	writeDiffFile(t, dir, "c d.txt", "one\ntwo changed\nthree\nfour\nfive\nsix\nseven\neight\nnine\nten\neleven\ntwelve\n")
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "-m", "rename")
	to := trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))

	change, err := (&Git{}).FileDiff(context.Background(), dir, from, to, "a b.txt")
	if err != nil || change == nil {
		t.Fatalf("FileDiff = %+v, %v", change, err)
	}
	if change.OldPath != "a b.txt" || change.NewPath != "c d.txt" {
		t.Fatalf("rename paths = %q -> %q", change.OldPath, change.NewPath)
	}
	if len(change.Hunks) != 1 || change.Hunks[0].OldStart != 2 {
		t.Fatalf("rename hunks = %+v", change.Hunks)
	}
}

func TestFileDiffRejectsUnsafePath(t *testing.T) {
	dir, from, to := diffRepo(t, func(t *testing.T, dir string) {
		writeDiffFile(t, dir, "main.go", "one changed\ntwo\nthree\nfour\nfive\nsix\n")
	})
	for _, path := range []string{"", "../main.go", "/main.go", "a//b", "./main.go", "a\\b"} {
		if _, err := (&Git{}).FileDiff(context.Background(), dir, from, to, path); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("FileDiff(%q) = %v, want ErrInvalidPath", path, err)
		}
	}
}

func diffRepo(t *testing.T, mutate func(t *testing.T, dir string)) (string, string, string) {
	t.Helper()
	dir := initRepo(t)
	writeDiffFile(t, dir, "main.go", "one\ntwo\nthree\nfour\nfive\nsix\n")
	gitCmd(t, dir, "add", "main.go")
	gitCmd(t, dir, "commit", "-q", "-m", "base")
	from := trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))
	mutate(t, dir)
	gitCmd(t, dir, "add", "-A")
	gitCmd(t, dir, "commit", "-q", "-m", "change")
	to := trimSHA(gitCmd(t, dir, "rev-parse", "HEAD"))
	return dir, from, to
}

func writeDiffFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func trimSHA(value string) string {
	for len(value) > 0 && (value[len(value)-1] == '\n' || value[len(value)-1] == '\r') {
		value = value[:len(value)-1]
	}
	return value
}
