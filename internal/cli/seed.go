package cli

import "github.com/spf13/cobra"

func newSeedCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "seed",
		Short: "Seed tenants, products, rate plans, and subscriptions for a run",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "seed", 2)
		},
	}
}
