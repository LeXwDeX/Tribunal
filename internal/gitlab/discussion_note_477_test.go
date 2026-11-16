package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAddDiscussionNotePostsInsideThread checks that the reply is sent to the
// discussion endpoint (not the top-level notes endpoint), that the body is
// neutralized, and that the response note is mapped.
func TestAddDiscussionNotePostsInsideThread(t *testing.T) {
	const token = "glpat-thread"
	discussion := sha("d")
	var sentBody string
	var requests atomic.Int32
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost ||
			r.URL.Path != "/api/v4/projects/42/merge_requests/7/discussions/"+discussion+"/notes" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("PRIVATE-TOKEN"); got != token {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		sentBody = body.Body
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":31,"body":"已重审","author":{"id":9,"username":"tribunal-bot"},"resolvable":true}`)
	})

	note, err := client.AddDiscussionNote(context.Background(), token, 42, 7, discussion, "已重审\n/close")
	if err != nil {
		t.Fatalf("AddDiscussionNote: %v", err)
	}
	if note == nil || note.ID != 31 || note.Body != "已重审" || note.AuthorID != 9 ||
		note.AuthorUsername != "tribunal-bot" || !note.Resolvable || note.DiscussionID != discussion {
		t.Fatalf("note = %+v", note)
	}
	if sentBody != "已重审\n&#47;close" {
		t.Fatalf("sent body = %q, want the quick action neutralized", sentBody)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

// TestAddDiscussionNoteRejectsInvalidInput checks that invalid input is
// rejected before any request is sent.
func TestAddDiscussionNoteRejectsInvalidInput(t *testing.T) {
	var requests atomic.Int32
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	})
	ctx := context.Background()
	valid := sha("d")
	cases := []struct {
		name         string
		token        string
		projectID    int64
		mrIID        int64
		discussionID string
		body         string
	}{
		{"missing token", "", 42, 7, valid, "x"},
		{"project ID", "token", 0, 7, valid, "x"},
		{"merge request IID", "token", 42, -1, valid, "x"},
		{"empty discussion ID", "token", 42, 7, "", "x"},
		{"blank discussion ID", "token", 42, 7, " ", "x"},
		{"discussion ID with path", "token", 42, 7, "../notes/1", "x"},
		{"discussion ID with space", "token", 42, 7, "abc def", "x"},
		{"discussion ID with dot", "token", 42, 7, "d1.json", "x"},
		{"empty body", "token", 42, 7, valid, ""},
		{"blank body", "token", 42, 7, valid, " \n\t"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.AddDiscussionNote(ctx, tc.token, tc.projectID, tc.mrIID, tc.discussionID, tc.body); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid input sent %d requests", requests.Load())
	}
}

// TestAddDiscussionNoteMapsUpstreamStatus checks that upstream failures keep
// the existing error classification and do not leak the token or body.
func TestAddDiscussionNoteMapsUpstreamStatus(t *testing.T) {
	for _, status := range []int{
		http.StatusUnauthorized,
		http.StatusForbidden,
		http.StatusNotFound,
		http.StatusTooManyRequests,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "upstream-secret-body", status)
			})
			_, err := client.AddDiscussionNote(context.Background(), "glpat-secret", 42, 7, sha("d"), "x")
			if err == nil || StatusOf(err) != status {
				t.Fatalf("error = %v, status = %d, want %d", err, StatusOf(err), status)
			}
			if !strings.Contains(err.Error(), "add_discussion_note") {
				t.Fatalf("error = %v, want the add_discussion_note operation", err)
			}
			for _, forbidden := range []string{"upstream-secret-body", "glpat-secret"} {
				if strings.Contains(err.Error(), forbidden) {
					t.Fatalf("error %q leaked %q", err, forbidden)
				}
			}
		})
	}
}

// TestAddDiscussionNoteEmptyResponseFails checks the defensive branch for a
// successful response that carries no usable note.
func TestAddDiscussionNoteEmptyResponseFails(t *testing.T) {
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{}`)
	})
	note, err := client.AddDiscussionNote(context.Background(), "token", 42, 7, sha("d"), "x")
	var opErr *Error
	if note != nil || !errors.As(err, &opErr) || opErr.Op != "add_discussion_note" || opErr.Status != 0 {
		t.Fatalf("note = %+v, err = %v", note, err)
	}
}

// TestAddDiscussionNoteDiscussionIDLengthBoundary checks that the maximum
// accepted discussion ID length is 255 and that one more byte is rejected
// before any request.
func TestAddDiscussionNoteDiscussionIDLengthBoundary(t *testing.T) {
	longest := strings.Repeat("a", 255)
	tooLong := strings.Repeat("a", 256)
	var requests atomic.Int32
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path != "/api/v4/projects/42/merge_requests/7/discussions/"+longest+"/notes" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":32,"body":"ok","author":{"id":9,"username":"bot"}}`)
	})

	note, err := client.AddDiscussionNote(context.Background(), "token", 42, 7, longest, "x")
	if err != nil || note == nil || note.ID != 32 || note.DiscussionID != longest {
		t.Fatalf("255-char discussion ID: note = %+v, err = %v", note, err)
	}
	if _, err := client.AddDiscussionNote(context.Background(), "token", 42, 7, tooLong, "x"); err == nil {
		t.Fatal("256-char discussion ID accepted")
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d, want 1", requests.Load())
	}
}

// TestListDiscussionsTagsNotesWithDiscussionID checks that notes converted from
// a discussion carry the parent discussion ID, and that the notes endpoint
// (which cannot report it) leaves the field empty.
func TestListDiscussionsTagsNotesWithDiscussionID(t *testing.T) {
	first, second := sha("a"), sha("b")
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/projects/42/merge_requests/7/discussions":
			fmt.Fprintf(w, `[{"id":%q,"notes":[{"id":21,"body":"one","author":{"id":9,"username":"bot"}},{"id":22,"body":"two","author":{"id":9,"username":"bot"}}]},{"id":%q,"individual_note":true,"notes":[{"id":23,"body":"three","author":{"id":9,"username":"bot"}}]}]`,
				first, second)
		case "/api/v4/projects/42/merge_requests/7/notes":
			fmt.Fprint(w, `[{"id":11,"body":"summary","author":{"id":9,"username":"bot"}}]`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	discussions, err := client.ListDiscussions(context.Background(), "token", 42, 7)
	if err != nil {
		t.Fatalf("ListDiscussions: %v", err)
	}
	if len(discussions) != 2 || len(discussions[0].Notes) != 2 || len(discussions[1].Notes) != 1 {
		t.Fatalf("discussions = %+v", discussions)
	}
	for i, discussion := range discussions {
		for j, note := range discussion.Notes {
			if note.DiscussionID != discussion.ID {
				t.Fatalf("discussion %d note %d DiscussionID = %q, want %q", i, j, note.DiscussionID, discussion.ID)
			}
		}
	}

	notes, err := client.ListMRNotes(context.Background(), "token", 42, 7)
	if err != nil {
		t.Fatalf("ListMRNotes: %v", err)
	}
	if len(notes) != 1 || notes[0].DiscussionID != "" {
		t.Fatalf("notes = %+v, want an empty DiscussionID from the notes endpoint", notes)
	}
}

// TestParseWebhookCarriesDiscussionID checks the note hook field used to reply
// inside the originating thread, including backward compatibility.
func TestParseWebhookCarriesDiscussionID(t *testing.T) {
	discussion := sha("c")
	payload := fmt.Sprintf(`{"object_kind":"note","object_attributes":{"id":5,"note":"@tribunal 请重审","noteable_type":"MergeRequest","noteable_iid":7,"discussion_id":%q}}`, discussion)
	hook, err := ParseWebhook("Note Hook", "uuid-1", "token", []byte(payload))
	if err != nil {
		t.Fatalf("ParseWebhook: %v", err)
	}
	if hook.ObjectAttributes.DiscussionID != discussion {
		t.Fatalf("DiscussionID = %q, want %q", hook.ObjectAttributes.DiscussionID, discussion)
	}

	old, err := ParseWebhook("Note Hook", "uuid-2", "token", []byte(`{"object_kind":"note","object_attributes":{"id":6,"note":"no thread"}}`))
	if err != nil {
		t.Fatalf("ParseWebhook without discussion_id: %v", err)
	}
	if old.ObjectAttributes.DiscussionID != "" {
		t.Fatalf("DiscussionID = %q, want the zero value", old.ObjectAttributes.DiscussionID)
	}
}
