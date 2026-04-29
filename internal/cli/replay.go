package cli

import "github.com/spf13/cobra"

func newReplayCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "replay",
		Short: "Replay captured event traffic from a recorded log",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "replay", 7)
		},
	}
}
