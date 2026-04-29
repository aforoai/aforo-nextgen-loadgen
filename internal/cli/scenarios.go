package cli

import "github.com/spf13/cobra"

func newScenariosCommand(_ *GlobalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "scenarios",
		Short: "List, describe, and inspect built-in load-test scenarios",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return notImplemented(cmd.OutOrStdout(), "scenarios", 2)
		},
	}
}
