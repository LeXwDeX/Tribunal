package repo

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
)

// RepositorySkillsSnapshotSHA256 hashes the filtered repository Skill snapshot
// exactly as recorded in review provenance. Files are ordered by raw UTF-8 path
// bytes. Each file contributes its unsigned 64-bit big-endian path length, path,
// content length and content. The returned digest is lowercase hexadecimal and
// never includes file contents. The same path, size, count and total limits as
// materialization apply.
func RepositorySkillsSnapshotSHA256(files map[string][]byte) (string, error) {
	names := make([]string, 0, len(files))
	parents := make(map[string]bool)
	var total int64
	for name, data := range files {
		if !safeRepositorySkillPath(repositorySkillsRoot+"/"+name) || len(data) > maxRepositorySkillFile {
			return "", errRepositorySkills
		}
		total += int64(len(data))
		if total > maxRepositorySkillsBytes {
			return "", errRepositorySkills
		}
		for dir := parentRepositorySkillPath(name); dir != ""; dir = parentRepositorySkillPath(dir) {
			parents[dir] = true
		}
		names = append(names, name)
	}
	if len(names)+len(parents) > maxRepositorySkillEntries {
		return "", errRepositorySkills
	}
	sort.Slice(names, func(i, j int) bool {
		return bytes.Compare([]byte(names[i]), []byte(names[j])) < 0
	})
	hash := sha256.New()
	var length [8]byte
	for _, name := range names {
		data := files[name]
		binary.BigEndian.PutUint64(length[:], uint64(len(name)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write([]byte(name))
		binary.BigEndian.PutUint64(length[:], uint64(len(data)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(data)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func parentRepositorySkillPath(name string) string {
	index := bytes.LastIndexByte([]byte(name), '/')
	if index < 0 {
		return ""
	}
	return name[:index]
}
