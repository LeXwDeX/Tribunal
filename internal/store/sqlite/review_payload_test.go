package sqlite

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/LeXwDeX/tribunal/internal/store"
)

func TestReviewJobPayloadKeepsExactNoteID(t *testing.T) {
	head := reviewHead("a")
	for _, tt := range []struct {
		name string
		note int64
	}{
		{"below float limit", 1<<53 - 1},
		{"float limit", 1 << 53},
		{"above float limit", 1<<53 + 1},
		{"max int64", 1<<63 - 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			command := store.ReviewCommand{
				ProjectID: 42,
				MRIID:     7,
				HeadSHA:   head,
				Payload:   json.RawMessage(`{"trigger":"command","note_id":` + strconv.FormatInt(tt.note, 10) + `}`),
			}
			raw, err := reviewJobPayload(command, head, 3)
			if err != nil {
				t.Fatalf("reviewJobPayload: %v", err)
			}
			var payload struct {
				ProjectID  int64  `json:"project_id"`
				MRIID      int64  `json:"mr_iid"`
				HeadSHA    string `json:"head_sha"`
				Generation int64  `json:"generation"`
				Trigger    string `json:"trigger"`
				NoteID     int64  `json:"note_id"`
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			if payload.NoteID != tt.note {
				t.Errorf("note_id = %d, want %d (payload %s)", payload.NoteID, tt.note, raw)
			}
			if payload.ProjectID != 42 || payload.MRIID != 7 || payload.HeadSHA != head ||
				payload.Generation != 3 || payload.Trigger != "command" {
				t.Errorf("store fields changed: %+v", payload)
			}
			if !strings.Contains(string(raw), `"note_id":`+strconv.FormatInt(tt.note, 10)) {
				t.Errorf("payload did not keep the exact note_id text: %s", raw)
			}
		})
	}
}

func TestReviewJobPayloadPreservesPersistedPayload(t *testing.T) {
	head := reviewHead("a")
	const legacy = `{"trigger":"command","note_id":9007199254740992,"author_id":9223372036854775807,` +
		`"profile_explicit":true,"full":true,"profile":"deep","weight":1.0}`
	for _, cancel := range []bool{false, true} {
		command := store.ReviewCommand{
			ProjectID: 42, MRIID: 7, HeadSHA: head, Cancel: cancel, Payload: json.RawMessage(legacy),
		}
		raw, err := reviewJobPayload(command, head, 2)
		if err != nil {
			t.Fatalf("reviewJobPayload cancel=%v: %v", cancel, err)
		}
		var payload struct {
			HeadSHA  string  `json:"head_sha"`
			NoteID   int64   `json:"note_id"`
			AuthorID int64   `json:"author_id"`
			Profile  string  `json:"profile"`
			Full     bool    `json:"full"`
			Weight   float64 `json:"weight"`
		}
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatalf("decode payload: %v", err)
		}
		if payload.NoteID != 9007199254740992 || payload.AuthorID != 9223372036854775807 ||
			payload.Profile != "deep" || !payload.Full || payload.Weight != 1.0 {
			t.Errorf("cancel=%v changed persisted fields: %+v (payload %s)", cancel, payload, raw)
		}
		if cancel && payload.HeadSHA != "" {
			t.Errorf("cancel kept head_sha: %s", raw)
		}
		if !cancel && payload.HeadSHA != head {
			t.Errorf("run head_sha = %q, want %q", payload.HeadSHA, head)
		}
	}
}

func TestReviewJobPayloadValidation(t *testing.T) {
	head := reviewHead("a")
	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{"generation is store owned", `{"generation":2}`, "must not contain generation"},
		{"array", `[]`, "must be a JSON object"},
		{"string", `"payload"`, "must be a JSON object"},
		{"invalid", `{"note_id":`, "must be a JSON object"},
		{"trailing value", `{}{}`, "single JSON object"},
		{"trailing garbage", `{} {}`, "single JSON object"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := reviewJobPayload(store.ReviewCommand{
				ProjectID: 42, MRIID: 7, HeadSHA: head, Payload: json.RawMessage(tt.raw),
			}, head, 1)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}

	for _, raw := range []string{"", "   ", "null"} {
		payload, err := reviewJobPayload(store.ReviewCommand{
			ProjectID: 42, MRIID: 7, HeadSHA: head, Payload: json.RawMessage(raw),
		}, head, 4)
		if err != nil {
			t.Fatalf("payload %q: %v", raw, err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(payload, &fields); err != nil {
			t.Fatal(err)
		}
		if string(fields["project_id"]) != "42" || string(fields["mr_iid"]) != "7" ||
			string(fields["generation"]) != "4" || string(fields["head_sha"]) != `"`+head+`"` {
			t.Errorf("payload %q = %s", raw, payload)
		}
	}
}

func TestReviewJobPayloadDuplicateKeyLastWins(t *testing.T) {
	head := reviewHead("a")
	raw, err := reviewJobPayload(store.ReviewCommand{
		ProjectID: 42, MRIID: 7, HeadSHA: head,
		Payload: json.RawMessage(`{"trigger":"mr_update","trigger":"command","note_id":9007199254740993}`),
	}, head, 1)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Trigger string `json:"trigger"`
		NoteID  int64  `json:"note_id"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Trigger != "command" || payload.NoteID != 9007199254740993 {
		t.Errorf("payload = %+v (%s), want last duplicate and exact note_id", payload, raw)
	}
}
