// Session 6: lifecycle subcommand. Drives subscription state-machine
// transitions during a load run — upgrades, downgrades, pause/resume, trial
// conversions, migrations, retry-payment, and dunning escalation. The
// transition log under <out>/transitions.jsonl feeds the validator's
// new lifecycle checks (Checks 9-11).
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

type lifecycleFlags struct {
	scenarioFlag     string
	target           string
	manifest         string
	out              string
	duration         string
	workers          int
	bufferSize       int
	tokenEnv         string
	pauseResumeDelay string
	maxAttempts      int
	noRunner         bool
}

func newLifecycleCommand(_ *GlobalFlags) *cobra.Command {
	var f lifecycleFlags
	cmd := &cobra.Command{
		Use:   "lifecycle",
		Short: "Drive subscription lifecycle transitions alongside an active run",
		Long: `Lifecycle runs the Session-4 event generator AND a parallel lifecycle
agent that fires real subscription state-machine transitions per the
scenario's lifecycle.* fields:

  upgrades_per_hour_pct       — POST /subscriptions/{id}/upgrade
  downgrades_per_hour_pct     — POST /subscriptions/{id}/downgrade
  pause_resume_per_hour_pct   — POST /pause + scheduled /resume
  trial_conversion_per_hour_pct — POST /convert-trial
  trial_cancel_per_hour_pct   — POST /cancel on TRIALING subs
  migrate_per_hour_pct        — POST /migrate-with-proration (stable-id)
  retry_payment_per_hour_pct  — POST /retry-payment + dunning walker

Every transition is logged to <out>/transitions.jsonl as a JSON line so
the validator can cross-check post-states (Check 9), state-machine legality
(Check 10), and bill-run-vs-migrate concurrency (Check 11).

Examples:
  aforo-loadgen lifecycle --scenario lifecycle-stress --manifest manifest.json --target local
  aforo-loadgen lifecycle --scenario scenarios/lifecycle-stress.yaml --target staging --duration 30m
  aforo-loadgen lifecycle --scenario lifecycle-stress --target local --no-runner    # only fire transitions, skip event load

The --no-runner flag is for focused lifecycle testing — useful when you
already have a separate run engine driving traffic and only want this
process to manage state transitions.
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runLifecycle(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.scenarioFlag, "scenario", "", "path to scenario YAML or built-in name (lifecycle-stress)")
	cmd.Flags().StringVar(&f.target, "target", "local", "target environment: local, staging, prod, or full URL")
	cmd.Flags().StringVar(&f.manifest, "manifest", "manifest.json", "path to manifest from `aforo-loadgen seed`")
	cmd.Flags().StringVar(&f.out, "out", "", "run output directory (default: runs/<scenario>-<unix>)")
	cmd.Flags().StringVar(&f.duration, "duration", "", "override scenario.duration (e.g. 30s, 5m)")
	cmd.Flags().IntVar(&f.workers, "workers", 32, "driver worker pool size for the event generator")
	cmd.Flags().IntVar(&f.bufferSize, "buffer-size", 4096, "events channel depth between generator and driver")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding the admin bearer token")
	cmd.Flags().StringVar(&f.pauseResumeDelay, "pause-resume-delay", "30s", "compress real customer pause windows; resume fires this long after pause")
	cmd.Flags().IntVar(&f.maxAttempts, "dunning-max-attempts", 3, "platform dunning.max-attempts mirror — used to assert escalation")
	cmd.Flags().BoolVar(&f.noRunner, "no-runner", false, "skip the event generator; only fire lifecycle transitions")
	return cmd
}

func runLifecycle(ctx context.Context, out, errOut io.Writer, f *lifecycleFlags) error {
	if f.scenarioFlag == "" {
		return errors.New("--scenario is required (path or built-in name)")
	}
	if f.manifest == "" {
		return errors.New("--manifest is required")
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

	manifest, err := seed.LoadManifest(f.manifest)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}

	target, err := aforo.ResolveTarget(f.target)
	if err != nil {
		return err
	}

	var durationOverride time.Duration
	if f.duration != "" {
		d, err := time.ParseDuration(f.duration)
		if err != nil {
			return fmt.Errorf("--duration: %w", err)
		}
		durationOverride = d
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
		outDir = filepath.Join("runs", fmt.Sprintf("%s-lifecycle-%d", doc.Scenario.Name, time.Now().Unix()))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	if !doc.Scenario.Lifecycle.Enabled {
		fmt.Fprintln(errOut, "warning: scenario.lifecycle.enabled = false — lifecycle agent will idle. Pass --scenario lifecycle-stress or set the field.")
	}

	// Open the transition log first so a startup failure leaves an empty
	// (but parseable) artifact behind.
	tlog, err := lifecycle.NewTransitionLog(outDir)
	if err != nil {
		return err
	}
	defer tlog.Close()

	token := os.Getenv(f.tokenEnv)
	client, err := lifecycle.NewClient(lifecycle.ClientConfig{
		Target: target,
		Token:  token,
	})
	if err != nil {
		return fmt.Errorf("lifecycle client: %w", err)
	}

	picker := lifecycle.NewPicker(manifest, doc.Scenario.Seed)
	agent, err := lifecycle.NewAgent(lifecycle.AgentConfig{
		Scenario:         doc.Scenario,
		Manifest:         manifest,
		Log:              tlog,
		Client:           client,
		Picker:           picker,
		RunID:            "",
		PauseResumeDelay: pauseResumeDelay,
		DunningConfig:    lifecycle.DunningConfig{MaxRetries: f.maxAttempts, EscalateAfterRetries: f.maxAttempts},
		Logger:           out,
	})
	if err != nil {
		return err
	}

	// Build the runner only when --no-runner is off.
	var r *runner.Runner
	if !f.noRunner {
		cfg := runner.Config{
			Scenario:         doc.Scenario,
			Manifest:         manifest,
			Target:           target,
			OutputDir:        outDir,
			Workers:          f.workers,
			DurationOverride: durationOverride,
			BufferSize:       f.bufferSize,
			AdminToken:       token,
			MetricsAddr:      "",
		}
		r, err = runner.New(cfg)
		if err != nil {
			return err
		}
	}

	// SIGINT/SIGTERM: cancel ctx, then both the runner and the agent drain.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	printLifecycleHeader(out, doc.Scenario, target, outDir, manifest, len(picker.Subjects()), f.noRunner)

	wg := sync.WaitGroup{}
	var runErr error
	var runRes *runner.RunResult

	if r != nil {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := r.Run(ctx)
			if err != nil && !errors.Is(err, context.Canceled) {
				runErr = err
			}
			runRes = res
			// First component to exit cancels the other so the agent
			// doesn't outlive the run engine when the latter completes.
			cancel()
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = agent.Run(ctx)
	}()

	// Honor scenario.duration when --no-runner is set (no run engine to
	// terminate the context for us).
	if r == nil {
		dur := doc.Scenario.Duration.Std()
		if durationOverride > 0 {
			dur = durationOverride
		}
		if dur > 0 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case <-ctx.Done():
				case <-time.After(dur):
					cancel()
				}
			}()
		}
	}

	wg.Wait()

	if runRes != nil {
		printRunSummary(out, runRes, outDir)
	}
	printLifecycleSummary(out, agent.LogSnapshot(), tlog.Count(), outDir)
	if runErr != nil {
		fmt.Fprintf(errOut, "run error: %v\n", runErr)
	}
	return nil
}

func printLifecycleHeader(out io.Writer, s *scenario.Scenario, t aforo.Target, outDir string, m *seed.Manifest, eligibleSubs int, noRunner bool) {
	fmt.Fprintf(out, "scenario:        %s\n", s.Name)
	fmt.Fprintf(out, "target:          %s\n", t.Name)
	fmt.Fprintf(out, "manifest:        %d tenants, %d eligible subs\n", len(m.Tenants), eligibleSubs)
	if noRunner {
		fmt.Fprintln(out, "runner:          disabled (--no-runner)")
	}
	lc := s.Lifecycle
	fmt.Fprintf(out, "lifecycle:       enabled=%t  upgrade=%.3f/h  downgrade=%.3f/h  pause/resume=%.3f/h  trial-conv=%.3f/h  trial-cancel=%.3f/h  migrate=%.3f/h  retry-payment=%.3f/h\n",
		lc.Enabled, lc.UpgradesPerHourPct, lc.DowngradesPerHourPct, lc.PauseResumePerHourPct,
		lc.TrialConversionPerHourPct, lc.TrialCancelPerHourPct, lc.MigratePerHourPct, lc.RetryPaymentPerHourPct,
	)
	fmt.Fprintf(out, "out:             %s\n", outDir)
	fmt.Fprintln(out, "")
}

func printLifecycleSummary(out io.Writer, snap lifecycle.Snapshot, total int, outDir string) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "lifecycle complete")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "transitions total\t%d\n", total)
	for _, k := range lifecycle.AllTransitionKinds {
		if n := snap.ByKind[k]; n > 0 {
			fmt.Fprintf(tw, "  %s\t%d\n", k, n)
		}
	}
	for status, n := range snap.ByStatus {
		fmt.Fprintf(tw, "  status=%s\t%d\n", status, n)
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\nartifacts written to: %s/transitions.jsonl\n", outDir)
}
