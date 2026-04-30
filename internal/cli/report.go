// Session 5: report subcommand. Renders a self-contained HTML report from
// a run output dir's run.json (+ optional validation.json). Intentionally
// offline: no CDN, no fonts.googleapis, system fonts only — operators
// forward these to slack channels and PR comments and they MUST render
// anywhere.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate/report"
)

type reportFlags struct {
	runOutput string
}

func newReportCommand(_ *GlobalFlags) *cobra.Command {
	var f reportFlags
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Render a self-contained HTML report from a completed run + validation pass",
		Long: `Reads <run-output>/run.json and (if present) <run-output>/validation.json
and emits <run-output>/report.html.

The HTML is fully self-contained — no Google Fonts, no CDN, no relative
images. System fonts only. Operators forward the file to Slack channels
or attach it to PR comments; it must render identically everywhere,
including offline.

Examples:
  aforo-loadgen report --run-output runs/ci-smoke-1714438800
  aforo-loadgen report --run-output runs/matrix-billing-1714438800

Exits 0 on success, non-zero on a missing or malformed run.json.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runReport(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.runOutput, "run-output", "", "directory containing run.json from `aforo-loadgen run`")
	return cmd
}

func runReport(_ context.Context, out, errOut io.Writer, f *reportFlags) error {
	if f.runOutput == "" {
		return errors.New("--run-output is required")
	}

	runPath := filepath.Join(f.runOutput, "run.json")
	runData, err := os.ReadFile(runPath) // #nosec G304 — caller-controlled
	if err != nil {
		return fmt.Errorf("read %s: %w", runPath, err)
	}
	var run runner.RunResult
	if err := json.Unmarshal(runData, &run); err != nil {
		return fmt.Errorf("parse %s: %w", runPath, err)
	}

	var valReport *validate.ValidationReport
	valPath := filepath.Join(f.runOutput, "validation.json")
	if _, statErr := os.Stat(valPath); statErr == nil {
		r, err := validate.LoadValidationReport(valPath)
		if err != nil {
			fmt.Fprintf(errOut, "warning: %s present but unreadable: %v\n", valPath, err)
		} else {
			valReport = r
		}
	}

	htmlPath, err := report.Render(f.runOutput, &run, valReport)
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(htmlPath)
	fmt.Fprintf(out, "report rendered: %s\n", abs)
	return nil
}
