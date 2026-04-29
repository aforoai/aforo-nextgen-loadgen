package cli

import "github.com/spf13/cobra"

func newRunCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Drive a load-test scenario against the platform",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "run", 3)
		},
	}
}
