package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/version"
)

// newVersionCommand prints semver, commit SHA, and build date. This subcommand
// is fully implemented in Session 1 — it is the only non-stub.
//
// Version-prefix convention (Session 10): the version string in code is the
// SemVer body without a leading "v" (e.g. "0.1.0", "0.0.0-dev"). The print
// format always prepends "v". This keeps the goreleaser-injected
// `{{.Version}}` (no v) and the Makefile-injected `git describe` (with v)
// rendering consistently — and matches both the Homebrew formula's test
// (`assert_match "aforo-loadgen v#{version}"`) and the documented
// `aforo-loadgen version` output shape.
func newVersionCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the aforo-loadgen version, commit, and build date",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "aforo-loadgen %s (commit %s, built %s)\n",
				formatVersion(version.Version), version.Commit, version.BuildDate)
			return err
		},
	}
}

// formatVersion returns a SemVer string with exactly one leading "v". Idempotent:
// already-prefixed strings pass through unchanged.
func formatVersion(v string) string {
	if strings.HasPrefix(v, "v") {
		return v
	}
	return "v" + v
}
