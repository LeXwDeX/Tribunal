package gitlab

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func TestLatestMRVersionKeepsCollectionState(t *testing.T) {
	client := newExtraClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/42/merge_requests/7/versions" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `[{"id":3,"base_commit_sha":%q,"start_commit_sha":%q,"head_commit_sha":%q,"state":"overflow","real_size":"1000+"}]`,
			sha("a"), sha("b"), sha("c"))
	})
	version, err := client.LatestMRVersion(context.Background(), "token", 42, 7)
	if err != nil {
		t.Fatalf("LatestMRVersion: %v", err)
	}
	if version.State != "overflow" || version.RealSize != "1000+" {
		t.Fatalf("version = %+v, want the diff collection state preserved", version)
	}
}

func TestParseChangedFileCount(t *testing.T) {
	cases := []struct {
		value     string
		want      int
		truncated bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"12", 12, false},
		{"1000+", 1000, true},
		{" 7 ", 7, false},
		{"abc", 0, false},
		{"-3", 0, false},
	}
	for _, tc := range cases {
		count, truncated := ParseChangedFileCount(tc.value)
		if count != tc.want || truncated != tc.truncated {
			t.Errorf("ParseChangedFileCount(%q) = %d/%v, want %d/%v", tc.value, count, truncated, tc.want, tc.truncated)
		}
	}
}
