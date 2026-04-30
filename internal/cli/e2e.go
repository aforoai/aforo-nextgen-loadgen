// Session 7: e2e subcommand. Chains doctor → seed → run + lifecycle →
// validate → report → clean against a live target. The CLI layer is
// deliberately thin — flag parsing + signal wiring + invoking
// internal/orchestrator.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/orchestrator"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

type e2eFlags struct {
	scenarioFlag     string
	target           string
	out              string
	tokenEnv         string
	includeBilling   bool
	includeLifecycle bool
	keepData         bool
	skipDoctor       bool
	workers          int
	bufferSize       int
	pauseResumeDelay string
}

func newE2ECommand(_ *GlobalFlags) *cobra.Command {
	var f e2eFlags
	cmd := &cobra.Command{
		Use:   "e2e",
		Short: "Run the end-to-end Aforo flow: doctor → seed → run + lifecycle → validate → report → clean",
		Long: `e2e is the headline workflow for Session 7. It chains every prior
session's deliverable into a single subcommand:

  1. doctor      verify every service is reachable + auth works
  2. seed        provision tenants per archetype
  3. run         drive event traffic at the seeded population
  4. lifecycle   fire subscription state-machine transitions in parallel
  5. validate    assert post-run invariants and per-archetype billing
  6. report      render report.html (offline-safe, no CDN)
  7. clean       archive seeded entities (skipped via --keep-data)

The --include-billing flag enables the full billing-match check
(per-archetype invoice math). The --include-lifecycle flag turns on the
parallel transition agent. --keep-data preserves the seed manifest +
runs/ artifacts after the flow completes — useful when debugging.

Examples:
  aforo-loadgen e2e --scenario crawl-e2e --target local
  aforo-loadgen e2e --scenario scenarios/crawl-e2e.yaml --target local --include-billing --include-lifecycle
  aforo-loadgen e2e --scenario crawl-e2e --target local --keep-data --out e2e-debug-$(date +%s)

Exit codes:
  0  every stage PASS or SKIP
  1  any stage FAIL — see the printed summary + e2e.json for details
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runE2E(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.scenarioFlag, "scenario", "", "path to scenario YAML or built-in name (e.g. crawl-e2e)")
	cmd.Flags().StringVar(&f.target, "target", "local", "target environment: local, staging, prod, or full URL")
	cmd.Flags().StringVar(&f.out, "out", "", "output dir (default: e2e/<scenario>-<unix>)")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding the bearer token")
	cmd.Flags().BoolVar(&f.includeBilling, "include-billing", false, "enable per-archetype billing-match validation (requires backend)")
	cmd.Flags().BoolVar(&f.includeLifecycle, "include-lifecycle", false, "fire subscription transitions during the run")
	cmd.Flags().BoolVar(&f.keepData, "keep-data", false, "skip the clean stage; preserve seeded entities and run artifacts")
	cmd.Flags().BoolVar(&f.skipDoctor, "skip-doctor", false, "skip the pre-flight health check (use only when known healthy)")
	cmd.Flags().IntVar(&f.workers, "workers", 32, "driver worker pool size for the run stage")
	cmd.Flags().IntVar(&f.bufferSize, "buffer-size", 4096, "events channel depth between generator and driver")
	cmd.Flags().StringVar(&f.pauseResumeDelay, "pause-resume-delay", "30s", "lifecycle pause-resume gap (compress real customer windows)")
	return cmd
}

func runE2E(ctx context.Context, out, errOut io.Writer, f *e2eFlags) error {
	if f.scenarioFlag == "" {
		return errors.New("--scenario is required (path or built-in name)")
	}
	doc, err := loadScenario(f.scenarioFlag)
	if err != nil {
		return err
	}
	if errs := scenario.Validate(doc); errs.HasErrors() {
		for _, e := range errs {
			fmt.Fprintln(errOut, e.Error())
		}
		return fmt.Errorf("%s: %d validation error(s)", f.scenarioFlag, len(errs))
	}

	target, err := aforo.ResolveTarget(f.target)
	if err != nil {
		return err
	}

	token := os.Getenv(f.tokenEnv)
	if token == "" {
		// Doctor itself can run without a token (it'll SKIP auth checks),
		// but seed/run cannot. Fail fast with an actionable message.
		return fmt.Errorf("env %s is empty — set your admin bearer token before running e2e", f.tokenEnv)
	}

	pauseResumeDelay := 30 * time.Second
	if f.pauseResumeDelay != "" {
		d, err := time.ParseDuration(f.pauseResumeDelay)
		if err != nil {
			return fmt.Errorf("--pause-resume-delay: %w", err)
		}
		pauseResumeDelay = d
	}

	outDir := f.out
	if outDir == "" {
		outDir = filepath.Join("e2e", fmt.Sprintf("%s-%d", doc.Scenario.Name, time.Now().Unix()))
	}

	cfg := orchestrator.Config{
		Scenario:                  doc.Scenario,
		Target:                    target,
		OutputDir:                 outDir,
		BearerToken:               token,
		IncludeBilling:            f.includeBilling,
		IncludeLifecycle:          f.includeLifecycle,
		KeepData:                  f.keepData,
		SkipDoctor:                f.skipDoctor,
		Workers:                   f.workers,
		BufferSize:                f.bufferSize,
		LifecyclePauseResumeDelay: pauseResumeDelay,
		Stdout:                    out,
		Stderr:                    errOut,
	}
	o, err := orchestrator.New(cfg)
	if err != nil {
		return err
	}

	// SIGINT/SIGTERM cancels the entire e2e flow. The orchestrator drains
	// in-flight stages and writes e2e.json on its way out.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	res, runErr := o.Run(ctx)
	if res != nil {
		res.PrintSummary(out)
	}
	if runErr != nil {
		return runErr
	}
	return nil
}
