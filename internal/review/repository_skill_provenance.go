package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"

	"github.com/LeXwDeX/tribunal/internal/repo"
	"github.com/LeXwDeX/tribunal/internal/store"
)

const RepositorySkillProvenanceVersion = 1

// RepositorySkillProvenance records the managed repository Skill capability,
// not the origin of independently configured platform or project Skills.
// A repository capability is not executable until its snapshot and grants are
// revalidated by the runtime and publisher.
// SnapshotSHA256 is the SHA-256 of all filtered files, ordered by raw UTF-8
// path bytes relative to .claude/skills. For each file, hash its path length,
// path, content length, and content; lengths are unsigned 64-bit big-endian.
type RepositorySkillProvenance struct {
	Version        int               `json:"version"`
	Mode           string            `json:"mode"`
	TargetBranch   string            `json:"target_branch,omitempty"`
	TargetSHA      string            `json:"target_sha,omitempty"`
	GrantRevisions map[string]string `json:"grant_revisions,omitempty"`
	SnapshotSHA256 string            `json:"snapshot_sha256,omitempty"`
}

var repositorySkillBlockRE = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (p *RepositorySkillProvenance) validate() error {
	if p == nil || p.Version != RepositorySkillProvenanceVersion {
		return dossierErr("repository_skill_provenance", "unsupported version")
	}
	switch p.Mode {
	case "none":
		if p.TargetBranch != "" || p.TargetSHA != "" || p.GrantRevisions != nil || p.SnapshotSHA256 != "" {
			return dossierErr("repository_skill_provenance", "none must not include a source or grants")
		}
	case "repository":
		if p.TargetBranch == "" || strings.TrimSpace(p.TargetBranch) != p.TargetBranch {
			return dossierErr("repository_skill_provenance", "invalid target branch")
		}
		if _, err := repo.NormalizeRef("refs/heads/" + p.TargetBranch); err != nil || !validObjectID(p.TargetSHA) || !dossierSHA256RE.MatchString(p.SnapshotSHA256) || p.SnapshotSHA256 != strings.ToLower(p.SnapshotSHA256) || len(p.GrantRevisions) == 0 {
			return dossierErr("repository_skill_provenance", "invalid source or grants")
		}
		granted := false
		for block, revision := range p.GrantRevisions {
			if !repositorySkillBlockRE.MatchString(block) || !store.ValidBlockBindingRevision(revision) {
				return dossierErr("repository_skill_provenance", "invalid grant revision")
			}
			granted = granted || revision != store.BlockBindingAbsentRevision
		}
		if !granted {
			return dossierErr("repository_skill_provenance", "repository capability has no grant")
		}
	default:
		return dossierErr("repository_skill_provenance", "unknown mode")
	}
	return nil
}

// NoRepositorySkillCapability reports provenance that never used a repository
// Skill. A repository capability is replayable only after the runtime
// revalidates its grants and snapshot.
func (d *Dossier) NoRepositorySkillCapability() bool {
	return d != nil && d.RepositorySkillProvenance != nil && d.RepositorySkillProvenance.Mode == "none" && d.RepositorySkillProvenance.validate() == nil
}

// RepositorySkillCapabilitySupported reports a well-formed marker that may be
// replayed after runtime validation. It does not prove the grants or snapshot.
func (d *Dossier) RepositorySkillCapabilitySupported() bool {
	if d == nil || d.RepositorySkillProvenance == nil || d.RepositorySkillProvenance.validate() != nil {
		return false
	}
	return d.RepositorySkillProvenance.Mode == "none" || d.RepositorySkillProvenance.Mode == "repository"
}

// DossierHasNoRepositorySkillCapability checks the marker even when unrelated
// dossier fields are malformed and must be handled by their own recovery path.
func DossierHasNoRepositorySkillCapability(raw []byte) bool {
	marker, err := dossierRepositorySkillMarker(raw)
	if err != nil || len(marker) == 0 {
		return false
	}
	var p RepositorySkillProvenance
	return json.Unmarshal(marker, &p) == nil && p.Mode == "none"
}

// DossierHasSupportedRepositorySkillCapability checks the marker alone. The
// caller still revalidates repository grants and snapshot before execution or
// publication.
func DossierHasSupportedRepositorySkillCapability(raw []byte) bool {
	marker, err := dossierRepositorySkillMarker(raw)
	if err != nil || len(marker) == 0 {
		return false
	}
	var p RepositorySkillProvenance
	return json.Unmarshal(marker, &p) == nil && (p.Mode == "none" || p.Mode == "repository")
}

// DossierUsesRepositorySkills reports whether the stored dossier claims a
// repository capability, even when its grants can no longer be revalidated.
func DossierUsesRepositorySkills(raw []byte) bool {
	marker, err := dossierRepositorySkillMarker(raw)
	if err != nil || len(marker) == 0 {
		return false
	}
	var p RepositorySkillProvenance
	return json.Unmarshal(marker, &p) == nil && p.Mode == "repository"
}

// jsonObject rejects duplicate keys, including in grant revisions.
func jsonObject(raw []byte) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected an object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("invalid object key")
		}
		if _, exists := fields[key]; exists {
			return nil, errors.New("duplicate object key")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON")
	}
	return fields, nil
}

func dossierRepositorySkillMarker(raw []byte) (json.RawMessage, error) {
	fields, err := jsonObject(raw)
	if err != nil {
		return nil, err
	}
	for key := range fields {
		if key != "repository_skill_provenance" && strings.EqualFold(key, "repository_skill_provenance") {
			return nil, dossierErr("repository_skill_provenance", "invalid field name")
		}
	}
	marker := fields["repository_skill_provenance"]
	if bytes.Equal(bytes.TrimSpace(marker), []byte("null")) {
		return nil, dossierErr("repository_skill_provenance", "must not be null")
	}
	return marker, nil
}

func (p *RepositorySkillProvenance) UnmarshalJSON(raw []byte) error {
	fields, err := jsonObject(raw)
	if err != nil {
		return dossierErr("repository_skill_provenance", "invalid object")
	}
	for key, value := range fields {
		switch key {
		case "version", "mode", "target_branch", "target_sha", "grant_revisions", "snapshot_sha256":
		default:
			return dossierErr("repository_skill_provenance", "unknown field")
		}
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return dossierErr("repository_skill_provenance", "null field")
		}
	}
	if grants, ok := fields["grant_revisions"]; ok {
		if _, err := jsonObject(grants); err != nil {
			return dossierErr("repository_skill_provenance", "invalid grant revisions")
		}
	}
	type plain RepositorySkillProvenance
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return dossierErr("repository_skill_provenance", "invalid fields")
	}
	if value.Mode == "none" && len(fields) != 2 {
		return dossierErr("repository_skill_provenance", "none must not include a source or grants")
	}
	if err := (*RepositorySkillProvenance)(&value).validate(); err != nil {
		return err
	}
	*p = RepositorySkillProvenance(value)
	return nil
}

// UnmarshalJSON distinguishes an absent legacy marker from an explicit null.
func (d *Dossier) UnmarshalJSON(raw []byte) error {
	if _, err := dossierRepositorySkillMarker(raw); err != nil {
		return dossierErr("dossier", "invalid object or repository skill marker")
	}
	type plain Dossier
	var value plain
	if err := json.Unmarshal(raw, &value); err != nil {
		return err
	}
	*d = Dossier(value)
	return nil
}
