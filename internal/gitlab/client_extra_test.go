package gitlab

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func newExtraClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	ts := httptest.NewServer(handler)
	t.Cleanup(ts.Close)
	client, err := New(Config{BaseURL: ts.URL, RequestTimeout: 2 * time.Second, MaxConcurrency: 4})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func sha(char string) string {
	return strings.Repeat(char, 40)
}

func TestListMRDiffsPaginates(t *testing.T) {
	const token = "glpat-extra"
	var pages []string
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/42/merge_requests/7/diffs" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("PRIVATE-TOKEN"); got != token {
			t.Errorf("PRIVATE-TOKEN = %q", got)
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[{"old_path":"a.go","new_path":"a.go","diff":"@@ -1 +1 @@\n-old\n+new"}]`)
		case "2":
			fmt.Fprint(w, `[{"old_path":"b.go","new_path":"b.go","new_file":true,"diff":"@@ -0,0 +1,1 @@\n+x"}]`)
		default:
			t.Errorf("unexpected page %q", page)
		}
	})

	diffs, err := client.ListMRDiffs(context.Background(), token, 42, 7)
	if err != nil {
		t.Fatalf("ListMRDiffs: %v", err)
	}
	if len(diffs) != 2 || diffs[0].NewPath != "a.go" || !diffs[1].NewFile {
		t.Fatalf("diffs = %+v", diffs)
	}
	if fmt.Sprint(pages) != "[1 2]" {
		t.Fatalf("pages = %v", pages)
	}
}

func TestLatestMRVersionChoosesHighestID(t *testing.T) {
	var pages []string
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/42/merge_requests/7/versions" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		page := r.URL.Query().Get("page")
		pages = append(pages, page)
		w.Header().Set("Content-Type", "application/json")
		switch page {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprintf(w, `[{"id":2,"base_commit_sha":%q,"start_commit_sha":%q,"head_commit_sha":%q}]`,
				sha("a"), sha("b"), sha("c"))
		case "2":
			fmt.Fprintf(w, `[{"id":9,"base_commit_sha":%q,"start_commit_sha":%q,"head_commit_sha":%q}]`,
				sha("d"), sha("e"), sha("f"))
		default:
			t.Errorf("unexpected page %q", page)
		}
	})

	version, err := client.LatestMRVersion(context.Background(), "token", 42, 7)
	if err != nil {
		t.Fatalf("LatestMRVersion: %v", err)
	}
	if version.ID != 9 || version.HeadSHA != sha("f") || version.BaseSHA != sha("d") || version.StartSHA != sha("e") {
		t.Fatalf("version = %+v", version)
	}
	if fmt.Sprint(pages) != "[1 2]" {
		t.Fatalf("pages = %v", pages)
	}
}

func TestLatestMRVersionRejectsMalformedSHAs(t *testing.T) {
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"id":1,"base_commit_sha":"short","start_commit_sha":"","head_commit_sha":""}]`)
	})
	_, err := client.LatestMRVersion(context.Background(), "token", 42, 7)
	if err == nil || !strings.Contains(err.Error(), "get_mr_version") {
		t.Fatalf("error = %v", err)
	}
}

func TestGetRawFileEscapesPathAndValidatesInput(t *testing.T) {
	var gotPath, gotRef string
	var requests atomic.Int32
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		gotPath = r.URL.EscapedPath()
		gotRef = r.URL.Query().Get("ref")
		fmt.Fprint(w, "file body")
	})

	body, err := client.GetRawFile(context.Background(), "token", 42, "config/file.yml", "main")
	if err != nil {
		t.Fatalf("GetRawFile: %v", err)
	}
	if string(body) != "file body" {
		t.Fatalf("body = %q", body)
	}
	if gotPath != "/api/v4/projects/42/repository/files/config%2Ffile%2Eyml/raw" {
		t.Fatalf("escaped path = %q", gotPath)
	}
	if gotRef != "main" {
		t.Fatalf("ref = %q", gotRef)
	}

	before := requests.Load()
	for _, path := range []string{"", "/abs", "../x", "a/../b", "a//b", `a\b`, "a?b", "a\x01b"} {
		if _, err := client.GetRawFile(context.Background(), "token", 42, path, "main"); err == nil {
			t.Errorf("GetRawFile(%q) unexpectedly succeeded", path)
		}
	}
	for _, ref := range []string{"", " main", "-x", "a..b", "a b", "ma\x01in"} {
		if _, err := client.GetRawFile(context.Background(), "token", 42, "a.yml", ref); err == nil {
			t.Errorf("GetRawFile(ref %q) unexpectedly succeeded", ref)
		}
	}
	if requests.Load() != before {
		t.Fatalf("invalid input caused %d requests", requests.Load()-before)
	}
}

func TestCreateDiscussionSendsPosition(t *testing.T) {
	var body struct {
		Body     string `json:"body"`
		Position struct {
			BaseSHA      string `json:"base_sha"`
			StartSHA     string `json:"start_sha"`
			HeadSHA      string `json:"head_sha"`
			OldPath      string `json:"old_path"`
			NewPath      string `json:"new_path"`
			OldLine      int64  `json:"old_line"`
			NewLine      int64  `json:"new_line"`
			PositionType string `json:"position_type"`
		} `json:"position"`
	}
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v4/projects/42/merge_requests/7/discussions" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"disc-1","notes":[{"id":5,"body":"hi","author":{"id":9,"username":"bot"},"position":{"base_sha":%q,"start_sha":%q,"head_sha":%q,"old_path":"a.go","new_path":"a.go","old_line":11,"new_line":11}}]}`,
			sha("a"), sha("b"), sha("c"))
	})

	discussion, err := client.CreateDiscussion(context.Background(), "token", 42, 7, NewDiscussion{
		Body: "found a bug",
		Position: &Position{
			BaseSHA:  sha("a"),
			StartSHA: sha("b"),
			HeadSHA:  sha("c"),
			OldPath:  "a.go",
			NewPath:  "a.go",
			OldLine:  11,
			NewLine:  11,
		},
	})
	if err != nil {
		t.Fatalf("CreateDiscussion: %v", err)
	}
	if discussion.ID != "disc-1" || len(discussion.Notes) != 1 || discussion.Notes[0].Position == nil ||
		discussion.Notes[0].Position.NewLine != 11 {
		t.Fatalf("discussion = %+v", discussion)
	}
	if body.Body != "found a bug" || body.Position.BaseSHA != sha("a") || body.Position.PositionType != "text" ||
		body.Position.OldLine != 11 || body.Position.NewLine != 11 || body.Position.OldPath != "a.go" {
		t.Fatalf("request body = %+v", body)
	}
}

func TestCreateDiscussionOmitsZeroLineFields(t *testing.T) {
	cases := []struct {
		name         string
		position     Position
		wantOld      bool
		wantNew      bool
		wantOldValue string
		wantNewValue string
	}{
		{
			name: "added line",
			position: Position{
				BaseSHA: sha("a"), StartSHA: sha("b"), HeadSHA: sha("c"),
				OldPath: "a.go", NewPath: "a.go", NewLine: 7,
			},
			wantNew:      true,
			wantNewValue: "7",
		},
		{
			name: "deleted line",
			position: Position{
				BaseSHA: sha("a"), StartSHA: sha("b"), HeadSHA: sha("c"),
				OldPath: "a.go", NewPath: "a.go", OldLine: 5,
			},
			wantOld:      true,
			wantOldValue: "5",
		},
		{
			name: "context line",
			position: Position{
				BaseSHA: sha("a"), StartSHA: sha("b"), HeadSHA: sha("c"),
				OldPath: "a.go", NewPath: "a.go", OldLine: 5, NewLine: 9,
			},
			wantOld:      true,
			wantNew:      true,
			wantOldValue: "5",
			wantNewValue: "9",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var position map[string]json.RawMessage
			client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					Position map[string]json.RawMessage `json:"position"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode body: %v", err)
				}
				position = body.Position
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{"id":"disc-1","notes":[]}`)
			})
			positionCopy := tc.position
			if _, err := client.CreateDiscussion(context.Background(), "token", 42, 7, NewDiscussion{
				Body: "found a bug", Position: &positionCopy,
			}); err != nil {
				t.Fatalf("CreateDiscussion: %v", err)
			}
			oldRaw, hasOld := position["old_line"]
			newRaw, hasNew := position["new_line"]
			if hasOld != tc.wantOld {
				t.Fatalf("old_line present = %v, want %v (position = %v)", hasOld, tc.wantOld, position)
			}
			if hasNew != tc.wantNew {
				t.Fatalf("new_line present = %v, want %v (position = %v)", hasNew, tc.wantNew, position)
			}
			if tc.wantOld && string(oldRaw) != tc.wantOldValue {
				t.Fatalf("old_line = %s, want %s", oldRaw, tc.wantOldValue)
			}
			if tc.wantNew && string(newRaw) != tc.wantNewValue {
				t.Fatalf("new_line = %s, want %s", newRaw, tc.wantNewValue)
			}
		})
	}
}

func TestNoteDiscussionAndAwardRequests(t *testing.T) {
	var createdNoteBody, updatedNoteBody, addedAwardName string
	var deletedAward bool
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/42/merge_requests/7/notes":
			fmt.Fprint(w, `[{"id":11,"body":"summary","author":{"id":9,"username":"bot"},"system":false,"resolvable":false}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/projects/42/merge_requests/7/notes":
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			createdNoteBody = body.Body
			fmt.Fprint(w, `{"id":12,"body":"created","author":{"id":9,"username":"bot"}}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/v4/projects/42/merge_requests/7/notes/11":
			var body struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			updatedNoteBody = body.Body
			fmt.Fprint(w, `{"id":11,"body":"updated","author":{"id":9,"username":"bot"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/42/merge_requests/7/discussions":
			fmt.Fprint(w, `[{"id":"d1","individual_note":false,"notes":[{"id":21,"body":"inline","author":{"id":9,"username":"bot"},"position":{"base_sha":"`+sha("a")+`","head_sha":"`+sha("c")+`","old_path":"a.go","new_path":"a.go","old_line":3,"new_line":3}}]}]`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v4/projects/42/merge_requests/7/notes/11/award_emoji":
			fmt.Fprint(w, `[{"id":1,"name":"thumbsup","user":{"id":3,"username":"dev"}},{"id":2,"name":"thumbsdown","user":{"id":4,"username":"other"}}]`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/v4/projects/42/merge_requests/7/notes/11/award_emoji":
			var body struct {
				Name string `json:"name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			addedAwardName = body.Name
			fmt.Fprint(w, `{"id":3,"name":"eyes","user":{"id":9,"username":"bot"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v4/projects/42/merge_requests/7/notes/11/award_emoji/3":
			deletedAward = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	})

	notes, err := client.ListMRNotes(context.Background(), "token", 42, 7)
	if err != nil || len(notes) != 1 || notes[0].ID != 11 || notes[0].Body != "summary" || notes[0].AuthorUsername != "bot" {
		t.Fatalf("notes = %+v, %v", notes, err)
	}
	created, err := client.CreateMRNote(context.Background(), "token", 42, 7, "created")
	if err != nil || created.ID != 12 || createdNoteBody != "created" {
		t.Fatalf("created = %+v, body = %q, err = %v", created, createdNoteBody, err)
	}
	if err := client.UpdateMRNote(context.Background(), "token", 42, 7, 11, "updated"); err != nil || updatedNoteBody != "updated" {
		t.Fatalf("UpdateMRNote err = %v, body = %q", err, updatedNoteBody)
	}
	discussions, err := client.ListDiscussions(context.Background(), "token", 42, 7)
	if err != nil || len(discussions) != 1 || discussions[0].ID != "d1" || len(discussions[0].Notes) != 1 ||
		discussions[0].Notes[0].Position == nil || discussions[0].Notes[0].Position.OldLine != 3 {
		t.Fatalf("discussions = %+v, %v", discussions, err)
	}
	awards, err := client.ListNoteAwards(context.Background(), "token", 42, 7, 11)
	if err != nil || len(awards) != 2 || awards[0].Name != "thumbsup" || awards[0].Username != "dev" {
		t.Fatalf("awards = %+v, %v", awards, err)
	}
	added, err := client.AddNoteAward(context.Background(), "token", 42, 7, 11, "eyes")
	if err != nil || added == nil || added.ID != 3 || added.Name != "eyes" || addedAwardName != "eyes" {
		t.Fatalf("AddNoteAward = %+v, name = %q, err = %v", added, addedAwardName, err)
	}
	if err := client.DeleteNoteAward(context.Background(), "token", 42, 7, 11, 3); err != nil || !deletedAward {
		t.Fatalf("DeleteNoteAward err = %v, deleted = %v", err, deletedAward)
	}
}

func TestListPipelinesJobsAndClosingIssues(t *testing.T) {
	var pipelinePages []string
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v4/projects/42/merge_requests/7/pipelines":
			page := r.URL.Query().Get("page")
			pipelinePages = append(pipelinePages, page)
			if page == "1" {
				w.Header().Set("X-Next-Page", "2")
				fmt.Fprint(w, `[{"id":10,"iid":3,"status":"failed","ref":"feat","sha":"`+sha("a")+`","web_url":"https://gitlab.example/p/10"}]`)
				return
			}
			fmt.Fprint(w, `[]`)
		case "/api/v4/projects/42/pipelines/10/jobs":
			fmt.Fprint(w, `[{"id":1,"name":"test","stage":"test","status":"failed","allow_failure":false,"failure_reason":"script_failure","web_url":"https://gitlab.example/j/1"},null]`)
		case "/api/v4/projects/42/merge_requests/7/closes_issues":
			fmt.Fprint(w, `[{"id":500,"iid":5,"title":"Bug","state":"opened","description":"desc","labels":["bug"],"web_url":"https://gitlab.example/i/5"}]`)
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			http.NotFound(w, r)
		}
	})

	pipelines, err := client.ListMRPipelines(context.Background(), "token", 42, 7)
	if err != nil || len(pipelines) != 1 || pipelines[0].ID != 10 || pipelines[0].Status != "failed" {
		t.Fatalf("pipelines = %+v, %v", pipelines, err)
	}
	if fmt.Sprint(pipelinePages) != "[1 2]" {
		t.Fatalf("pipeline pages = %v", pipelinePages)
	}
	jobs, err := client.ListPipelineJobs(context.Background(), "token", 42, 10)
	if err != nil || len(jobs) != 1 || jobs[0].Name != "test" || jobs[0].FailureReason != "script_failure" {
		t.Fatalf("jobs = %+v, %v", jobs, err)
	}
	issues, err := client.ListClosingIssues(context.Background(), "token", 42, 7)
	if err != nil || len(issues) != 1 || issues[0].IID != 5 || issues[0].Labels[0] != "bug" {
		t.Fatalf("issues = %+v, %v", issues, err)
	}
}

func TestSetCommitStatusRequestAndValidation(t *testing.T) {
	var requests atomic.Int32
	var body struct {
		State       string `json:"state"`
		Name        string `json:"name"`
		TargetURL   string `json:"target_url"`
		Description string `json:"description"`
	}
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/v4/projects/42/statuses/"+sha("a") {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":1,"status":"failed"}`)
	})

	err := client.SetCommitStatus(context.Background(), "token", 42, sha("a"), CommitStatus{
		State:       "failed",
		TargetURL:   "https://review.example/runs/1",
		Description: "blocking findings",
	})
	if err != nil {
		t.Fatalf("SetCommitStatus: %v", err)
	}
	if body.State != "failed" || body.Name != "ai-review" || body.TargetURL != "https://review.example/runs/1" ||
		body.Description != "blocking findings" {
		t.Fatalf("status body = %+v", body)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests = %d", requests.Load())
	}

	invalid := []CommitStatus{
		{State: "success", Description: strings.Repeat("x", 141)},
		{State: "success", TargetURL: "ftp://example.com/x"},
		{State: "success", TargetURL: "https://user:pass@example.com/x"},
		{State: "pending"},
	}
	for _, status := range invalid {
		if err := client.SetCommitStatus(context.Background(), "token", 42, sha("a"), status); err == nil {
			t.Errorf("SetCommitStatus(%+v) unexpectedly succeeded", status)
		}
	}
	if requests.Load() != 1 {
		t.Fatalf("invalid statuses caused requests: %d", requests.Load())
	}
}

func TestExtraMethodsHonorCancellationWithoutLeakingTokens(t *testing.T) {
	const token = "glpat-secret-extra"
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Query().Get("page") == "1" {
			w.Header().Set("X-Next-Page", "2")
			fmt.Fprint(w, `[]`)
			return
		}
		time.Sleep(200 * time.Millisecond)
	}))
	defer ts.Close()
	client, err := New(Config{BaseURL: ts.URL, RequestTimeout: 50 * time.Millisecond, MaxConcurrency: 1})
	if err != nil {
		t.Fatal(err)
	}

	_, err = client.ListMRDiffs(context.Background(), token, 42, 7)
	if err == nil {
		t.Fatal("expected the paginated request to time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context timeout", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), ts.URL) {
		t.Fatalf("error leaked details: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.ListMRNotes(ctx, token, 42, 7)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled error = %v", err)
	}
	if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), ts.URL) {
		t.Fatalf("cancelled error leaked details: %v", err)
	}
}

func TestExtraMethodsRejectMissingTokenAndInvalidIDs(t *testing.T) {
	var requests atomic.Int32
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
	})
	ctx := context.Background()
	cases := []func() error{
		func() error { _, err := client.ListMRDiffs(ctx, "", 42, 7); return err },
		func() error { _, err := client.ListMRDiffs(ctx, "token", 0, 7); return err },
		func() error { _, err := client.LatestMRVersion(ctx, "token", 42, 0); return err },
		func() error { _, err := client.ListMRPipelines(ctx, "token", 42, -1); return err },
		func() error { _, err := client.ListPipelineJobs(ctx, "token", 42, 0); return err },
		func() error { _, err := client.ListClosingIssues(ctx, "token", 0, 7); return err },
		func() error { _, err := client.ListMRNotes(ctx, "token", 42, 0); return err },
		func() error { _, err := client.CreateMRNote(ctx, "token", 42, 7, " "); return err },
		func() error { return client.UpdateMRNote(ctx, "token", 42, 7, 0, "x") },
		func() error { _, err := client.ListDiscussions(ctx, "token", 42, 0); return err },
		func() error {
			_, err := client.CreateDiscussion(ctx, "token", 42, 7, NewDiscussion{Body: "x", Position: &Position{}})
			return err
		},
		func() error { return client.SetCommitStatus(ctx, "token", 42, "short", CommitStatus{State: "success"}) },
		func() error { _, err := client.ListNoteAwards(ctx, "token", 42, 7, 0); return err },
		func() error { _, err := client.AddNoteAward(ctx, "token", 42, 7, 0, "eyes"); return err },
		func() error { _, err := client.AddNoteAward(ctx, "token", 42, 7, 11, " "); return err },
		func() error { return client.DeleteNoteAward(ctx, "token", 42, 7, 11, 0) },
	}
	for i, fn := range cases {
		if err := fn(); err == nil {
			t.Errorf("case %d unexpectedly succeeded", i)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid input sent %d requests", requests.Load())
	}
}

func TestHasSummaryMarker(t *testing.T) {
	if !HasSummaryMarker("<!-- gitlab-ce-review:summary v1 fp=x -->\nbody") {
		t.Fatal("summary marker was not detected")
	}
	if HasSummaryMarker("body\n<!-- gitlab-ce-review:summary v1 fp=x -->") {
		t.Fatal("marker on a later line was accepted")
	}
	if HasSummaryMarker("<!-- gitlab-ce-review:summar") {
		t.Fatal("truncated marker was accepted")
	}
	if HasSummaryMarker("") {
		t.Fatal("empty body was accepted")
	}
}
