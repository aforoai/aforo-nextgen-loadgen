package cli

import "github.com/spf13/cobra"

func newValidateCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate a scenario or config file without sending traffic",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "validate", 4)
		},
	}
}
