// Package cli wires the cobra command tree for aforo-loadgen.
package cli

import (
	"github.com/spf13/cobra"
)

// GlobalFlags holds flag values shared across every subcommand. The pointer
// is bound to PersistentFlags on the root command.
type GlobalFlags struct {
	Target   string
	Config   string
	LogLevel string
	JSONLogs bool
}

// NewRootCommand builds a fresh root command tree. Each invocation returns
// an isolated tree so tests can run in parallel without flag-state leaking.
func NewRootCommand() *cobra.Command {
	var flags GlobalFlags

	root := &cobra.Command{
		Use:   "aforo-loadgen",
		Short: "Load-test the Aforo NextGen ingestion pipeline",
		Long: `aforo-loadgen drives realistic event traffic through the Aforo NextGen
platform — covering all 4 product types, 9 gateway adapters, 6 pricing models,
3 billing modes, and the full subscription/payment/ERP lifecycle.

Target: 15K TPS sustained, 500 tenants, Crawl-Walk-Run methodology.

The headline workflow is "aforo-loadgen e2e" (Session 7) — chains doctor
→ seed → run + lifecycle → validate → report → clean against a live
target in one command. See "aforo-loadgen e2e --help" or the README for
the full session roadmap.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.PersistentFlags().StringVar(&flags.Target, "target", "",
		"base URL of the Aforo platform under test (e.g. https://usage-ingestor.aforo.space)")
	root.PersistentFlags().StringVar(&flags.Config, "config", "",
		"path to a loadgen YAML config file")
	root.PersistentFlags().StringVar(&flags.LogLevel, "log-level", "info",
		"log level: debug, info, warn, error")
	root.PersistentFlags().BoolVar(&flags.JSONLogs, "json-logs", false,
		"emit logs as newline-delimited JSON instead of human-readable text")

	root.AddCommand(
		newRunCommand(&flags),
		newReplayCommand(&flags),
		newSeedCommand(&flags),
		newValidateCommand(&flags),
		newLifecycleCommand(&flags),
		newPaymentsCommand(&flags),
		newReportCommand(&flags),
		newScenariosCommand(&flags),
		newDoctorCommand(&flags),
		newServerCommand(&flags),
		newE2ECommand(&flags),
		newCoordinatorCommand(&flags),
		newWorkerCommand(&flags),
		newVersionCommand(&flags),
	)

	return root
}
