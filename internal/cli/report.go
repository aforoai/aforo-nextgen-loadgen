package cli

import "github.com/spf13/cobra"

func newReportCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "report",
		Short: "Render a results report from a completed run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "report", 10)
		},
	}
}
