package store

// RepositorySkillAuthorization is an independently granted set of repository
// skills for one project, target branch, block and reviewed target commit. It
// never includes legacy block-binding strings. Revision changes on every write,
// including no-ops. Empty grants have no target SHA and no snapshot SHA. A
// non-empty grant persisted by migration 0021 has an empty SnapshotSHA256 and
// must not be used for execution; an administrator can still read and revoke it.
type RepositorySkillAuthorization struct {
	ProjectID      int64    `json:"project_id"`
	TargetBranch   string   `json:"target_branch"`
	TargetSHA      string   `json:"target_sha"`
	Block          string   `json:"block"`
	Skills         []string `json:"skills"`
	Revision       string   `json:"revision"`
	SnapshotSHA256 string   `json:"snapshot_sha256"`
}

// ValidRepositorySkillSnapshotSHA256 accepts only a lowercase hexadecimal
// SHA-256. The empty value is reserved for revoked grants and pre-0023 rows;
// callers must not treat it as an executable snapshot.
func ValidRepositorySkillSnapshotSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for i := 0; i < len(value); i++ {
		if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f')) {
			return false
		}
	}
	return true
}
