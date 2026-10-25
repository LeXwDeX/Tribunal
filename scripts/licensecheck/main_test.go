package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const (
	mitText       = "MIT License\n\nCopyright (c) 2026\n\nPermission is hereby granted, free of charge, to any person obtaining a copy\n"
	iscText       = "ISC License\n\nPermission to use, copy, modify, and distribute this software for any purpose\nwith or without fee is hereby granted.\n"
	apacheText    = "Apache License\nVersion 2.0, January 2004\n"
	mplText       = "Mozilla Public License Version 2.0\n\nIf You choose to distribute this software under the terms of this License, You may also offer it under the terms of the GNU General Public License, Version 2.0, the GNU Lesser General Public License, Version 2.1, or the GNU Affero General Public License, Version 3.0.\n"
	gplText       = "GNU GENERAL PUBLIC LICENSE\nVersion 3, 29 June 2007\n"
	agplText      = "GNU AFFERO GENERAL PUBLIC LICENSE\nVersion 3, 19 November 2007\n"
	lgplText      = "GNU LESSER GENERAL PUBLIC LICENSE\nVersion 2.1, February 1999\n"
	unrelatedText = "All rights reserved. This file contains no license text.\n"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		text string
		want licenseClass
	}{
		{name: "mit", text: mitText, want: allowedLicense},
		{name: "isc", text: iscText, want: allowedLicense},
		{name: "apache", text: apacheText, want: allowedLicense},
		{name: "mpl", text: mplText, want: mplLicense},
		{name: "gpl", text: gplText, want: forbiddenLicense},
		{name: "agpl", text: agplText, want: forbiddenLicense},
		{name: "lgpl", text: lgplText, want: forbiddenLicense},
		{name: "spdx lgpl", text: "SPDX-License-Identifier: LGPL-2.1-only\n", want: forbiddenLicense},
		{name: "gpl version", text: "Licensed under GPLv3\n", want: forbiddenLicense},
		{name: "no word boundary", text: "This is SHLGPLX software\n", want: unknownLicense},
		{name: "unrelated", text: unrelatedText, want: unknownLicense},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify(tc.text); got != tc.want {
				t.Fatalf("classify = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRunAllowsPermissiveLicenses(t *testing.T) {
	mitDir := t.TempDir()
	writeFile(t, mitDir, "LICENSE", mitText)
	apacheDir := t.TempDir()
	writeFile(t, apacheDir, "LICENSE", apacheText)
	writeFile(t, apacheDir, "NOTICE", "Copyright 2026 Example\n")
	iscDir := t.TempDir()
	writeFile(t, iscDir, "LICENSE.txt", iscText)

	code, stdout, stderr := runModules(t, []string{"-min", "3"}, mitDir, apacheDir, iscDir)
	if code != 0 {
		t.Fatalf("run = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "checked 3 modules") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestRunRejectsForbiddenLicenses(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{name: "gpl", text: gplText},
		{name: "agpl", text: agplText},
		{name: "lgpl", text: lgplText},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			writeFile(t, dir, "LICENSE", tc.text)

			code, _, stderr := runModules(t, nil, dir)
			if code != 1 {
				t.Fatalf("run = %d, want 1", code)
			}
			if !strings.Contains(stderr, "forbidden license") {
				t.Fatalf("stderr = %q", stderr)
			}
		})
	}
}

func TestRunAllowsMPLAndLogs(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "LICENSE", mplText)

	code, stdout, stderr := runModules(t, nil, dir)
	if code != 0 {
		t.Fatalf("run = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "MPL in LICENSE (allowed)") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestRunFailsMissingLicenseFile(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "README.md", mitText)

	code, _, stderr := runModules(t, nil, dir)
	if code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if !strings.Contains(stderr, "no LICENSE, COPYING, or NOTICE file") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunFailsUnknownLicense(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, dir, "LICENSE", unrelatedText)

	code, _, stderr := runModules(t, nil, dir)
	if code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if !strings.Contains(stderr, "unrecognized license") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestLicenseFilesRecognizesNames(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"LICENSE", "COPYING", "NOTICE", "license.md", "license.go", "license_templates.go", "README.md"} {
		writeFile(t, dir, name, mitText)
	}

	got, err := licenseFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"COPYING", "LICENSE", "NOTICE", "license.md"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("licenseFiles = %v, want %v", got, want)
	}
}

func TestRunFailsWhenNothingChecked(t *testing.T) {
	// The main module has a license, but it must not count toward -min.
	dir := t.TempDir()
	writeFile(t, dir, "LICENSE", mitText)
	data, err := encodeModules(module{Path: "example.com/main", Dir: dir, Main: true})
	if err != nil {
		t.Fatal(err)
	}
	list := func() ([]byte, error) { return data, nil }

	code, stdout, stderr := runWithList([]string{"-min", "1"}, list)
	if code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if !strings.Contains(stderr, "checked 0 modules, minimum is 1") {
		t.Fatalf("stderr = %q, stdout = %q", stderr, stdout)
	}

	code, _, stderr = runWithList(nil, list)
	if code != 0 {
		t.Fatalf("run without -min = %d, stderr = %s", code, stderr)
	}
}

func TestRunFailsOnEmptyModuleList(t *testing.T) {
	list := func() ([]byte, error) { return nil, nil }

	code, _, stderr := runWithList([]string{"-min", "1"}, list)
	if code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if !strings.Contains(stderr, "checked 0 modules, minimum is 1") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunSkipsModulesWithoutDirectory(t *testing.T) {
	data, err := encodeModules(module{Path: "example.com/not-downloaded"})
	if err != nil {
		t.Fatal(err)
	}
	list := func() ([]byte, error) { return data, nil }

	code, stdout, stderr := runWithList(nil, list)
	if code != 0 {
		t.Fatalf("run = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "checked 0 modules") {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestRunFailsWhenListingModulesFails(t *testing.T) {
	list := func() ([]byte, error) { return nil, os.ErrPermission }

	code, _, stderr := runWithList(nil, list)
	if code != 1 {
		t.Fatalf("run = %d, want 1", code)
	}
	if !strings.Contains(stderr, "list modules") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestRunRejectsNegativeMinimum(t *testing.T) {
	code, _, stderr := runWithList([]string{"-min", "-1"}, func() ([]byte, error) { return nil, nil })
	if code != 2 {
		t.Fatalf("run = %d, want 2", code)
	}
	if !strings.Contains(stderr, "-min must not be negative") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func runModules(t *testing.T, args []string, dirs ...string) (int, string, string) {
	t.Helper()
	mods := make([]module, 0, len(dirs))
	for i, dir := range dirs {
		mods = append(mods, module{Path: "example.com/module" + string(rune('a'+i)), Dir: dir})
	}
	data, err := encodeModules(mods...)
	if err != nil {
		t.Fatal(err)
	}
	return runWithList(args, func() ([]byte, error) { return data, nil })
}

func runWithList(args []string, list func() ([]byte, error)) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr, list)
	return code, stdout.String(), stderr.String()
}

func encodeModules(mods ...module) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, m := range mods {
		if err := enc.Encode(m); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}
