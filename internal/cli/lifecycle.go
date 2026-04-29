package cli

import "github.com/spf13/cobra"

func newLifecycleCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "lifecycle",
		Short: "Drive subscription lifecycle transitions (upgrade, downgrade, pause, cancel)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "lifecycle", 5)
		},
	}
}
