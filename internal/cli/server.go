package cli

import "github.com/spf13/cobra"

func newServerCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Run the loadgen control-plane server (dashboard + coordinator)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "server", 12)
		},
	}
}
