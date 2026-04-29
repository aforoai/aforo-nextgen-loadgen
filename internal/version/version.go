// Package version exposes build-time version metadata for the CLI.
//
// The three vars are set via -ldflags at build time. When building from a
// development checkout (e.g. `go run`), the defaults below are used.
package version

var (
	// Version is the semver release tag, e.g. "v0.1.0".
	Version = "0.0.0-dev"
	// Commit is the short git SHA the binary was built from.
	Commit = "unknown"
	// BuildDate is an ISO-8601 timestamp of the build.
	BuildDate = "unknown"
)
