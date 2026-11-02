package repo

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"testing"
)

func expectedRepositorySkillsHash(t *testing.T, files map[string][]byte) string {
	t.Helper()
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool {
		return bytes.Compare([]byte(names[i]), []byte(names[j])) < 0
	})
	hash := sha256.New()
	var length [8]byte
	for _, name := range names {
		binary.BigEndian.PutUint64(length[:], uint64(len(name)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(name))
		binary.BigEndian.PutUint64(length[:], uint64(len(files[name])))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(files[name])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func TestRepositorySkillsSnapshotHashIsCanonical(t *testing.T) {
	files := map[string][]byte{
		"z/empty":          {},
		"a/binary":         {0x00, 0xff, 0x01, 0x00},
		"m/unicode-\u00e9": []byte("text"),
	}
	want := expectedRepositorySkillsHash(t, files)
	got, err := RepositorySkillsSnapshotSHA256(files)
	if err != nil {
		t.Fatal(err)
	}
	if got != want || len(got) != 64 || got != strings.ToLower(got) {
		t.Fatalf("hash = %q, want %q", got, want)
	}

	// Map insertion order is not a stable iteration order. Rebuilding the map in
	// reverse must still produce the canonical path-byte order.
	reversed := make(map[string][]byte, len(files))
	names := []string{"m/unicode-\u00e9", "a/binary", "z/empty"}
	for _, name := range names {
		reversed[name] = files[name]
	}
	if got, err := RepositorySkillsSnapshotSHA256(reversed); err != nil || got != want {
		t.Fatalf("reversed hash = %q, err=%v, want %q", got, err, want)
	}

	empty, err := RepositorySkillsSnapshotSHA256(nil)
	if err != nil || empty != expectedRepositorySkillsHash(t, nil) {
		t.Fatalf("empty hash = %q, err=%v", empty, err)
	}
}

func TestRepositorySkillsSnapshotHashChangesWithPathAndContent(t *testing.T) {
	base := map[string][]byte{"review/SKILL.md": []byte("same")}
	baseHash, err := RepositorySkillsSnapshotSHA256(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, files := range map[string]map[string][]byte{
		"rename":  {"review/OTHER.md": []byte("same")},
		"content": {"review/SKILL.md": []byte("changed")},
		"empty":   {"review/SKILL.md": []byte("same"), "review/empty": {}},
	} {
		got, err := RepositorySkillsSnapshotSHA256(files)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got == baseHash {
			t.Errorf("%s reused the approved snapshot hash", name)
		}
	}
}

func TestRepositorySkillsSnapshotHashRejectsUnsafePathsAndLimits(t *testing.T) {
	canary := []byte("CANARY_SNAPSHOT_CONTENT")
	for _, name := range []string{
		"", "/absolute", "../escape", "a//b", "a/./b", "a/../b",
		".git/config", "a/.GIT/config", "a\\b", "a\x00b", "a\rb", "a\nb",
		"a:b", string([]byte{0xff}),
	} {
		_, err := RepositorySkillsSnapshotSHA256(map[string][]byte{name: canary})
		if err == nil {
			t.Errorf("unsafe path %q accepted", name)
			continue
		}
		if strings.Contains(err.Error(), "CANARY") {
			t.Errorf("error leaked content for %q", name)
		}
	}
	if _, err := RepositorySkillsSnapshotSHA256(map[string][]byte{"large": make([]byte, maxRepositorySkillFile+1)}); err == nil {
		t.Error("oversized file accepted")
	}
	total := make(map[string][]byte, 65)
	for i := 0; i < 65; i++ {
		total[fmt.Sprintf("dir%02d/file", i)] = make([]byte, maxRepositorySkillFile)
	}
	if _, err := RepositorySkillsSnapshotSHA256(total); err == nil {
		t.Error("total size over the limit accepted")
	}
	entries := make(map[string][]byte, maxRepositorySkillEntries+1)
	for i := 0; i <= maxRepositorySkillEntries; i++ {
		entries[fmt.Sprintf("f%04d", i)] = nil
	}
	if _, err := RepositorySkillsSnapshotSHA256(entries); err == nil {
		t.Error("entry count over the limit accepted")
	}
}

func TestRepositorySkillsSnapshotHashDoesNotExposeContent(t *testing.T) {
	files := map[string][]byte{
		"review/SKILL.md": []byte("---\nname: review\n---\nCANARY_SECRET_BODY"),
		"review/blob.bin": {0x00, 0x01, 0xfe, 0xff},
	}
	got, err := RepositorySkillsSnapshotSHA256(files)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "CANARY") || strings.Contains(got, "SECRET") || len(got) != sha256.Size*2 {
		t.Fatalf("hash exposed content or was not a SHA-256: %q", got)
	}
}
