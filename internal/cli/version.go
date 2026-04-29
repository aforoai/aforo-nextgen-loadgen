package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/version"
)

// newVersionCommand prints semver, commit SHA, and build date. This subcommand
// is fully implemented in Session 1 — it is the only non-stub.
func newVersionCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the aforo-loadgen version, commit, and build date",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "aforo-loadgen %s (commit %s, built %s)\n",
				version.Version, version.Commit, version.BuildDate)
			return err
		},
	}
}
