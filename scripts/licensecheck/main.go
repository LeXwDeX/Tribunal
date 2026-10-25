// Command licensecheck rejects modules with forbidden or unrecognized licenses.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type module struct {
	Path string
	Dir  string
	Main bool
}

type licenseClass int

const (
	unknownLicense licenseClass = iota
	allowedLicense
	mplLicense
	forbiddenLicense
)

// MPL is checked first: the MPL 2.0 text references GPL, LGPL, and AGPL as
// secondary licenses, which must not classify the MPL itself as forbidden.
var (
	mplRE       = regexp.MustCompile(`(?i)\bMozilla Public License\b`)
	forbiddenRE = regexp.MustCompile(
		`(?i)(?:\b(?:A?GPL|LGPL)(?:[- ]?v?\d+(?:\.\d+)*)?\b|\bGNU (?:Affero |Lesser |Library )?General Public License\b)`)
	allowedRE = regexp.MustCompile(
		`(?i)(?:\bApache License\b|\bISC License\b|Permission is hereby granted, free of charge|Redistribution and use in source and binary forms|Permission to use, copy, modify, and(?:/or)? distribute this software)`)
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr, nil))
}

func run(args []string, stdout, stderr io.Writer, list func() ([]byte, error)) int {
	fs := flag.NewFlagSet("licensecheck", flag.ContinueOnError)
	fs.SetOutput(stderr)
	min := fs.Int("min", 0, "minimum number of non-main modules to check")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *min < 0 {
		fmt.Fprintln(stderr, "licensecheck: -min must not be negative")
		return 2
	}
	if list == nil {
		list = goListModules
	}

	data, err := list()
	if err != nil {
		fmt.Fprintf(stderr, "licensecheck: list modules: %v\n", err)
		return 1
	}
	mods, err := parseModules(data)
	if err != nil {
		fmt.Fprintf(stderr, "licensecheck: list modules: %v\n", err)
		return 1
	}

	checked, err := checkModules(mods, stdout)
	if err != nil {
		fmt.Fprintf(stderr, "licensecheck: %v\n", err)
		return 1
	}
	if checked < *min {
		fmt.Fprintf(stderr, "licensecheck: checked %d modules, minimum is %d\n", checked, *min)
		return 1
	}
	fmt.Fprintf(stdout, "licensecheck: checked %d modules, minimum %d\n", checked, *min)
	return 0
}

func goListModules() ([]byte, error) {
	cmd := exec.Command("go", "list", "-m", "-json", "all")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list -m -json all: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func parseModules(data []byte) ([]module, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	var mods []module
	for {
		var m module
		if err := dec.Decode(&m); errors.Is(err, io.EOF) {
			return mods, nil
		} else if err != nil {
			return nil, err
		}
		mods = append(mods, m)
	}
}

func checkModules(mods []module, stdout io.Writer) (int, error) {
	checked := 0
	for _, m := range mods {
		if m.Main || m.Dir == "" {
			continue
		}
		files, err := licenseFiles(m.Dir)
		if err != nil {
			return checked, fmt.Errorf("module %s: %w", m.Path, err)
		}
		if len(files) == 0 {
			return checked, fmt.Errorf("module %s: no LICENSE, COPYING, or NOTICE file in %s", m.Path, m.Dir)
		}
		recognized := false
		for _, name := range files {
			path := filepath.Join(m.Dir, name)
			text, err := os.ReadFile(path)
			if err != nil {
				return checked, fmt.Errorf("module %s: %w", m.Path, err)
			}
			switch classify(string(text)) {
			case forbiddenLicense:
				return checked, fmt.Errorf("module %s: forbidden license in %s", m.Path, name)
			case mplLicense:
				fmt.Fprintf(stdout, "licensecheck: %s: MPL in %s (allowed)\n", m.Path, name)
				recognized = true
			case allowedLicense:
				recognized = true
			}
		}
		if !recognized {
			return checked, fmt.Errorf("module %s: unrecognized license in %s", m.Path, m.Dir)
		}
		checked++
	}
	return checked, nil
}

func licenseFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if isLicenseFile(entry.Name()) {
			files = append(files, entry.Name())
		}
	}
	sort.Strings(files)
	return files, nil
}

// isLicenseFile accepts license and notice text files, including bundled
// component notices such as LICENSE-3RD-PARTY.md and LICENSE-SQLITE. It
// rejects source files whose names merely start with "license", for example
// license.go.
func isLicenseFile(name string) bool {
	lower := strings.ToLower(name)
	switch lower {
	case "license", "copying", "notice", "copying.lesser":
		return true
	}
	for _, prefix := range []string{"license-", "copying-", "notice-"} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	for _, prefix := range []string{"license.", "copying.", "notice."} {
		if !strings.HasPrefix(lower, prefix) {
			continue
		}
		switch filepath.Ext(lower) {
		case ".md", ".txt", ".rst", ".html":
			return true
		}
	}
	return false
}

func classify(text string) licenseClass {
	switch {
	case mplRE.MatchString(text):
		return mplLicense
	case forbiddenRE.MatchString(text):
		return forbiddenLicense
	case allowedRE.MatchString(text):
		return allowedLicense
	default:
		return unknownLicense
	}
}
