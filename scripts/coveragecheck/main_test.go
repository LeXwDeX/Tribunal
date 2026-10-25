package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseProfileAggregatesStatements(t *testing.T) {
	profile := `mode: atomic
example.com/a/a.go:1.1,2.2 2 1
example.com/a/a.go:3.1,4.2 3 0
example.com/b/b.go:1.1,1.9 1 7
example.com/b/b.go:2.1,2.2 0 1
`
	cov, err := parseProfile(strings.NewReader(profile))
	if err != nil {
		t.Fatalf("parseProfile: %v", err)
	}
	if cov.Covered != 3 || cov.Total != 6 {
		t.Fatalf("coverage = %+v, want Covered=3 Total=6", cov)
	}
	if got, want := cov.Percent(), 50.0; got != want {
		t.Fatalf("Percent() = %v, want %v", got, want)
	}
}

func TestParseProfileRejectsInvalidProfiles(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		want    string
	}{
		{name: "empty", profile: "", want: "missing mode line"},
		{name: "missing mode", profile: "example.com/a/a.go:1.1,2.2 2 1\n", want: "before mode line"},
		{name: "duplicate mode", profile: "mode: set\nmode: set\n", want: "duplicate mode line"},
		{name: "invalid mode", profile: "mode: fast\n", want: "invalid coverage mode"},
		{name: "no statements", profile: "mode: set\nexample.com/a/a.go:1.1,2.2 0 1\n", want: "no statements"},
		{name: "malformed block", profile: "mode: set\nexample.com/a/a.go:1.1,2.2 2\n", want: "malformed coverage block"},
		{name: "negative statements", profile: "mode: set\nexample.com/a/a.go:1.1,2.2 -1 1\n", want: "invalid statement count"},
		{name: "negative count", profile: "mode: set\nexample.com/a/a.go:1.1,2.2 2 -1\n", want: "invalid execution count"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseProfile(strings.NewReader(tc.profile))
			if err == nil {
				t.Fatal("parseProfile succeeded, want error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestCheckThreshold(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	profile := "mode: set\nexample.com/a/a.go:1.1,2.2 1 1\nexample.com/a/a.go:3.1,4.2 1 0\n"
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := check(path, 50); err != nil {
		t.Fatalf("check at 50%%: %v", err)
	}
	if _, err := check(path, 50.01); err == nil || !strings.Contains(err.Error(), "below minimum") {
		t.Fatalf("check above coverage: err = %v, want below minimum error", err)
	}
	if _, err := check(filepath.Join(t.TempDir(), "missing.out"), 0); err == nil {
		t.Fatal("check of missing profile succeeded, want error")
	}
}

func TestRunExitCodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "coverage.out")
	profile := "mode: set\nexample.com/a/a.go:1.1,2.2 1 1\nexample.com/a/a.go:3.1,4.2 3 0\n"
	if err := os.WriteFile(path, []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	if code := run([]string{"-profile", path, "-min", "25"}, &stdout, &stderr); code != 0 {
		t.Fatalf("run = %d, stderr = %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "25.00% statement coverage (1/4)") {
		t.Fatalf("stdout = %q", stdout.String())
	}

	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-profile", path, "-min", "30"}, &stdout, &stderr); code != 1 {
		t.Fatalf("run below threshold = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "below minimum 30.00%") {
		t.Fatalf("stderr = %q", stderr.String())
	}

	if code := run([]string{"-profile", path, "-min", "101"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run with invalid minimum = %d, want 2", code)
	}
	if code := run([]string{"-profile", path, "-min", "NaN"}, &stdout, &stderr); code != 2 {
		t.Fatalf("run with NaN minimum = %d, want 2", code)
	}
}
