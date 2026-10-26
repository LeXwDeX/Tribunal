// Package builtin embeds the instruction skills Tribunal ships with. Each
// top-level directory is one Claude-compatible skill; the platform copies it
// into the managed skills root on first start (see app.ApplyExtensionDefaults).
//
// Every bundled skill keeps its upstream LICENSE and a NOTICE.md with the
// source and commit it was adapted from; scripts/licensecheck and
// scripts/licensebundle read them from this directory.
package builtin

import (
	"embed"
	"fmt"
	"io/fs"
	"sort"
)

//go:embed code-review
var files embed.FS

// Names returns the bundled skill names in stable order.
func Names() []string {
	entries, err := fs.ReadDir(files, ".")
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names
}

// Has reports whether name is a bundled skill.
func Has(name string) bool {
	for _, candidate := range Names() {
		if candidate == name {
			return true
		}
	}
	return false
}

// Sub returns the read-only files of one bundled skill, rooted at its
// directory.
func Sub(name string) (fs.FS, error) {
	if !Has(name) {
		return nil, fmt.Errorf("builtin: unknown skill %q", name)
	}
	return fs.Sub(files, name)
}
