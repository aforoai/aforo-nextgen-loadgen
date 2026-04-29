package cli

import "github.com/spf13/cobra"

func newPaymentsCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "payments",
		Short: "Drive payment, tax, and ERP integration flows",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "payments", 6)
		},
	}
}
