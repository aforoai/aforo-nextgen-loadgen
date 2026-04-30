// Session 5: validate subcommand. Reads a completed run's output dir
// (run.json, scenario.yaml) plus a seed manifest, runs the eight checks,
// and writes <out>/validation.json + a human-readable summary to stdout.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate"
)

type validateFlags struct {
	runOutput      string
	target         string
	manifest       string
	includeBilling bool
	tolerancePct   float64
	checks         []string
	archetypes     []string
}

func newValidateCommand(_ *GlobalFlags) *cobra.Command {
	var f validateFlags
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a completed run against the platform's expected behavior",
		Long: `Validate runs the Session-5 oracle against a completed run's output:

  * Check 1 — events sent ≈ events stored per tenant
  * Check 2 — cross-tenant leakage (10 IDOR probes)
  * Check 3 — every event resolved a customer (no NULL customer_id)
  * Check 4 — Redis cache hit ratio above threshold
  * Check 5 — per-archetype invoice math matches expected (--include-billing)
  * Check 6 — every negative-path category was rejected (incl. stale_keys)
  * Check 7 — property-based billing invariants hold over the seed
  * Check 8 — bill run concurrency: 1 of 2 simultaneous runs returns 409

Without infrastructure access, checks 1, 6, 7 still run from run.json alone
— ci-smoke validate exits 0 in pure-CI mode. Other checks SKIP with a
clear reason.

Examples:
  aforo-loadgen validate --run-output runs/ci-smoke-1714438800 --manifest manifest.json --target local
  aforo-loadgen validate --run-output runs/matrix-billing-1714438800 \
       --manifest manifest.json --target staging --include-billing
  aforo-loadgen validate --run-output runs/foo --manifest m.json --target local \
       --checks event_count_per_tenant,negative_path_correctness

Exits 0 on PASS (or no FAILs), 1 on any FAIL.
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runValidate(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.runOutput, "run-output", "", "directory containing run.json + scenario.yaml from `aforo-loadgen run`")
	cmd.Flags().StringVar(&f.target, "target", "local", "target environment (local, staging, prod) or full URL")
	cmd.Flags().StringVar(&f.manifest, "manifest", "", "path to seed manifest")
	cmd.Flags().BoolVar(&f.includeBilling, "include-billing", false, "run Check 5 (billing match) + Check 8 (bill run concurrency); requires backend")
	cmd.Flags().Float64Var(&f.tolerancePct, "tolerance-pct", 0.001, "billing-amount drift tolerance (0.001 = 0.1%)")
	cmd.Flags().StringSliceVar(&f.checks, "checks", nil, "subset of checks to run (default: all). e.g. --checks event_count_per_tenant,negative_path_correctness")
	cmd.Flags().StringSliceVar(&f.archetypes, "archetypes-only", nil, "limit Check 5 (billing match) to these archetypes")
	return cmd
}

func runValidate(ctx context.Context, out, errOut io.Writer, f *validateFlags) error {
	if f.runOutput == "" {
		return errors.New("--run-output is required")
	}
	if f.manifest == "" {
		return errors.New("--manifest is required")
	}

	run, _, doc, err := validate.LoadFromRunOutput(f.runOutput)
	if err != nil {
		return err
	}

	manifest, err := seed.LoadManifest(f.manifest)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}

	in := &validate.Inputs{
		RunOutputDir:   f.runOutput,
		Manifest:       manifest,
		Run:            run,
		Scenario:       doc.Scenario,
		Backend:        validate.NewOfflineBackend(run),
		IncludeBilling: f.includeBilling,
		TolerancePct:   f.tolerancePct,
		OnlyChecks:     f.checks,
		OnlyArchetypes: f.archetypes,
	}

	v, err := validate.New(in)
	if err != nil {
		return err
	}

	report, err := v.Run(ctx)
	if err != nil {
		return err
	}

	path, err := report.Save(f.runOutput)
	if err != nil {
		return err
	}

	printValidationSummary(out, report, path)

	if report.Summary.Failed > 0 {
		fmt.Fprintln(errOut, "validate: one or more checks FAILED")
		os.Exit(1) // CI gates on this
	}
	return nil
}

// printValidationSummary writes a tab-aligned summary suitable for human
// review. The full detail lives in validation.json; stdout is the
// "headline" view.
func printValidationSummary(out io.Writer, r *validate.ValidationReport, path string) {
	fmt.Fprintf(out, "scenario: %s\n", r.Scenario)
	fmt.Fprintf(out, "run id:   %s\n", r.RunID)
	fmt.Fprintf(out, "target:   %s\n", r.Target)
	fmt.Fprintln(out, "")

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CHECK\tSTATUS\tREASON")
	for _, c := range r.Checks {
		reason := c.Reason
		if reason == "" {
			reason = "-"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\n", c.Name, c.Status, truncate(reason, 100))
	}
	_ = tw.Flush()

	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "summary:  %s — passed=%d  failed=%d  skipped=%d  total=%d\n",
		r.Summary.Overall, r.Summary.Passed, r.Summary.Failed, r.Summary.Skipped, r.Summary.Total)
	abs, _ := filepath.Abs(path)
	fmt.Fprintf(out, "report:   %s\n", abs)
}

func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "…"
}
