package review

import (
	"encoding/json"
	"strings"
	"testing"
)

func provenanceDossier(t *testing.T) *Dossier {
	t.Helper()
	d := validDossier(t)
	d.IgnorePolicy = &IgnorePolicy{Version: IgnorePolicyVersion, Patterns: []string{}}
	return d
}

func TestWorkspaceProvenanceIndexSourceValidity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		source string
		valid  bool
	}{
		{"absent", "", true},
		{"workspace", IndexSourceWorkspace, true},
		{"run", IndexSourceRun, true},
		{"unknown", "cache", false},
		{"wrong case", "Run", false},
		{"padded", " run", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := provenanceDossier(t)
			d.WorkspaceProvenance.IndexSource = tc.source
			if got := d.WorkspaceProvenanceValid(); got != tc.valid {
				t.Fatalf("WorkspaceProvenanceValid = %v, want %v", got, tc.valid)
			}
			if got := d.MCPIndexEligible(); got != tc.valid {
				t.Fatalf("MCPIndexEligible = %v, want %v", got, tc.valid)
			}
		})
	}
}

// The source label never relaxes the evidence rule: the index must still be
// the one built for the MR's target tip.
func TestWorkspaceProvenanceIndexSourceKeepsIndexSHARule(t *testing.T) {
	for _, source := range []string{"", IndexSourceWorkspace, IndexSourceRun} {
		t.Run("source="+source, func(t *testing.T) {
			d := provenanceDossier(t)
			d.WorkspaceProvenance.IndexSource = source
			d.WorkspaceProvenance.IndexSHA = testBaseSHA
			if d.WorkspaceProvenanceValid() || d.MCPIndexEligible() {
				t.Fatal("an index of another commit was accepted")
			}

			d.WorkspaceProvenance.IndexSHA = ""
			if d.WorkspaceProvenanceValid() {
				t.Fatal("an eligible provenance without an index SHA was accepted")
			}

			d.WorkspaceProvenance.MCPEligible = false
			if !d.WorkspaceProvenanceValid() || d.MCPIndexEligible() {
				t.Fatalf("an ineligible provenance must stay valid without granting MCP: valid=%v eligible=%v",
					d.WorkspaceProvenanceValid(), d.MCPIndexEligible())
			}
		})
	}
}

func TestWorkspaceProvenanceIndexSourceSerialization(t *testing.T) {
	d := provenanceDossier(t)

	// A dossier collected before the field existed has no index_source key,
	// still decodes, and stays valid and byte-identical when written again.
	legacy, err := MarshalDossier(d)
	if err != nil {
		t.Fatalf("MarshalDossier: %v", err)
	}
	if strings.Contains(string(legacy), "index_source") {
		t.Fatalf("an unset index source was serialized: %s", legacy)
	}
	var decoded Dossier
	if err := json.Unmarshal(legacy, &decoded); err != nil {
		t.Fatalf("decode legacy dossier: %v", err)
	}
	if decoded.WorkspaceProvenance == nil || decoded.WorkspaceProvenance.IndexSource != "" || !decoded.WorkspaceProvenanceValid() || !decoded.MCPIndexEligible() {
		t.Fatalf("legacy dossier lost its provenance: %+v", decoded.WorkspaceProvenance)
	}
	again, err := MarshalDossier(&decoded)
	if err != nil || string(again) != string(legacy) {
		t.Fatalf("legacy dossier changed on re-encode: err=%v", err)
	}

	for _, source := range []string{IndexSourceWorkspace, IndexSourceRun} {
		d.WorkspaceProvenance.IndexSource = source
		raw, err := MarshalDossier(d)
		if err != nil {
			t.Fatalf("MarshalDossier(%s): %v", source, err)
		}
		if !strings.Contains(string(raw), `"index_source":"`+source+`"`) {
			t.Fatalf("index source %q missing from %s", source, raw)
		}
		var roundTrip Dossier
		if err := json.Unmarshal(raw, &roundTrip); err != nil {
			t.Fatalf("decode %s dossier: %v", source, err)
		}
		if roundTrip.WorkspaceProvenance.IndexSource != source || !roundTrip.MCPIndexEligible() {
			t.Fatalf("round trip lost source %q: %+v", source, roundTrip.WorkspaceProvenance)
		}
	}
}
