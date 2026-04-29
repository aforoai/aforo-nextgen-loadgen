package cli

import "github.com/spf13/cobra"

func newDoctorCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the local environment and target reachability",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "doctor", 11)
		},
	}
}
