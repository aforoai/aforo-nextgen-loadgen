package cli

import "github.com/spf13/cobra"

func newE2ECommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "e2e",
		Short: "Run end-to-end smoke flows against a live target",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "e2e", 8)
		},
	}
}
