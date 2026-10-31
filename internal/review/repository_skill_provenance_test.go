package review

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRepositorySkillProvenanceSerialization(t *testing.T) {
	base, err := MarshalDossier(validDossier(t))
	if err != nil {
		t.Fatal(err)
	}
	withMarker := func(marker string) []byte {
		return []byte(strings.TrimSuffix(string(base), "}") + `,"repository_skill_provenance":` + marker + `}`)
	}
	revision := strings.Repeat("a", 64)
	digest := strings.Repeat("b", 64)
	repository := `{"version":1,"mode":"repository","target_branch":"main","target_sha":"` + testLastSHA + `","grant_revisions":{"code-review":"` + revision + `"},"snapshot_sha256":"` + digest + `"}`
	for _, tc := range []struct {
		name, marker string
		valid        bool
	}{
		{"none", `{"version":1,"mode":"none"}`, true},
		{"repository-independent-target", repository, true},
		{"null", `null`, false},
		{"empty", `{}`, false},
		{"version", `{"version":2,"mode":"none"}`, false},
		{"null-version", `{"version":null,"mode":"none"}`, false},
		{"unknown-mode", `{"version":1,"mode":"used"}`, false},
		{"null-mode", `{"version":1,"mode":null}`, false},
		{"unknown-field", `{"version":1,"mode":"none","future":true}`, false},
		{"none-extra", `{"version":1,"mode":"none","target_sha":""}`, false},
		{"duplicate-mode", `{"version":1,"mode":"none","mode":"repository"}`, false},
		{"missing-target", strings.Replace(repository, `"target_sha":"`+testLastSHA+`",`, "", 1), false},
		{"wrong-branch", strings.Replace(repository, `"target_branch":"main"`, `"target_branch":"other"`, 1), false},
		{"bad-branch", strings.Replace(repository, `"target_branch":"main"`, `"target_branch":"../main"`, 1), false},
		{"null-grants", strings.Replace(repository, `{"code-review":"`+revision+`"}`, "null", 1), false},
		{"null-revision", strings.Replace(repository, `"`+revision+`"`, "null", 1), false},
		{"empty-grants", strings.Replace(repository, `{"code-review":"`+revision+`"}`, "{}", 1), false},
		{"invalid-grant", strings.Replace(repository, revision, "bad", 1), false},
		{"duplicate-grant", strings.Replace(repository, `{"code-review":"`+revision+`"}`, `{"code-review":"`+revision+`","code-review":"`+revision+`"}`, 1), false},
		{"bad-snapshot", strings.Replace(repository, digest, "bad", 1), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var d Dossier
			err := json.Unmarshal(withMarker(tc.marker), &d)
			if err == nil {
				err = ValidateDossier(&d)
			}
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if tc.valid {
				raw, err := MarshalDossier(&d)
				if err != nil || !strings.Contains(string(raw), `"repository_skill_provenance"`) {
					t.Fatalf("provenance lost in round trip: %s, %v", raw, err)
				}
			}
		})
	}
	var d Dossier
	if err := json.Unmarshal(withMarker(`{"version":1,"mode":"none"},"repository_skill_provenance":{"version":1,"mode":"none"}`), &d); err == nil {
		t.Fatal("duplicate provenance marker accepted")
	}
	if err := json.Unmarshal(withMarker(`{"version":1,"mode":"none"},"REPOSITORY_SKILL_PROVENANCE":null`), &d); err == nil {
		t.Fatal("case-variant provenance marker accepted")
	}
}

func TestRepositorySkillProvenanceSnapshotIsCopiedAndHashed(t *testing.T) {
	d := validDossier(t)
	revisions := map[string]string{"code-review": strings.Repeat("a", 64)}
	provenance := &RepositorySkillProvenance{Version: RepositorySkillProvenanceVersion, Mode: "repository", TargetBranch: d.MR.TargetBranch, TargetSHA: testLastSHA, GrantRevisions: revisions, SnapshotSHA256: strings.Repeat("b", 64)}
	copy, err := BuildDossier(DossierInput{
		Project: d.Project, MR: d.MR, DiffVersion: d.DiffVersion, DiffSummary: d.DiffSummary,
		Files: d.Files, GeneratedAt: d.GeneratedAt, RepositorySkillProvenance: provenance,
	})
	if err != nil {
		t.Fatal(err)
	}
	first, err := DossierHash(copy)
	if err != nil {
		t.Fatal(err)
	}
	revisions["code-review"] = strings.Repeat("c", 64)
	if copy.RepositorySkillProvenance.GrantRevisions["code-review"] != strings.Repeat("a", 64) {
		t.Fatal("grant revisions share the input map")
	}
	copy.RepositorySkillProvenance.SnapshotSHA256 = strings.Repeat("d", 64)
	second, err := DossierHash(copy)
	if err != nil || first == second {
		t.Fatalf("snapshot digest is not bound to dossier hash: %s, %s, %v", first, second, err)
	}
}
