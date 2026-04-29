// Package scenarios bundles the built-in scenario catalog as an embed.FS so
// the CLI can list, show, and validate them without depending on the
// caller's working directory.
//
// The set is deliberately small (six scenarios) and covers the Crawl /
// Walk / Run methodology plus the matrix-billing exhaustive case and a
// lifecycle-stress profile. Add new scenarios by dropping a .yaml file
// here; the //go:embed pattern picks it up automatically.
//
// Files in this package are NOT a Go scenario API — that lives in
// internal/scenario. This package only owns the embedding.
package scenarios

import (
	"embed"
	"io/fs"
	"sort"
	"strings"
)

// FS is the read-only filesystem of bundled scenario YAML files.
//
//go:embed *.yaml
var FS embed.FS

// Names returns the basenames (without .yaml extension) of every embedded
// scenario, sorted alphabetically. Returns an empty slice — never nil — if
// no scenarios are bundled.
func Names() []string {
	entries, err := fs.ReadDir(FS, ".")
	if err != nil {
		return []string{}
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".yaml") {
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".yaml"))
	}
	sort.Strings(names)
	return names
}

// Read returns the YAML bytes for the scenario whose basename (without
// extension) matches name. Returns fs.ErrNotExist if no such scenario.
func Read(name string) ([]byte, error) {
	return FS.ReadFile(name + ".yaml")
}
