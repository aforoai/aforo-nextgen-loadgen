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
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/driver"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

type runFlags struct {
	scenarioFlag     string
	target           string
	manifest         string
	out              string
	duration         string
	workers          int
	partitions       int
	bufferSize       int
	metricsAddr      string
	pprofPort        int
	tokenEnv         string
	fairnessMinShare float64
	iKnowWhatImDoing bool
}

// newRunCommand wires `aforo-loadgen run`. The body delegates to the
// runner package; this file is the thin CLI adapter.
func newRunCommand(_ *GlobalFlags) *cobra.Command {
	var f runFlags
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Drive a load-test scenario against the platform",
		Long: `Run executes a load-test scenario against a manifest of seeded tenants.

Each tick, the generator samples a tenant per the configured distribution,
samples a product per ProductMix, samples a customer/sub/key from the
manifest, builds a per-product-type event, optionally injects a
negative-path fault (late, future, malformed, wrong_auth, stale_key,
oversize), and dispatches via the chosen ingestion path.

Examples:
  aforo-loadgen run --scenario ci-smoke --manifest manifest.json --target local --out runs/$(date +%s)
  aforo-loadgen run --scenario walk-realistic-50t --manifest manifest.json --target local --duration 5m --pprof-port 6060
  aforo-loadgen run --scenario matrix-billing --manifest manifest.json --target https://staging.aforo.io --workers 64
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRun(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.scenarioFlag, "scenario", "", "path to scenario YAML (built-in name also accepted, e.g. ci-smoke)")
	cmd.Flags().StringVar(&f.target, "target", "local", "target environment: local, staging, prod, ci, or full URL")
	cmd.Flags().StringVar(&f.manifest, "manifest", "manifest.json", "path to manifest from `aforo-loadgen seed`")
	cmd.Flags().StringVar(&f.out, "out", "", "run output directory (default: runs/<run-id>)")
	cmd.Flags().StringVar(&f.duration, "duration", "", "override scenario.duration (e.g. 30s, 5m)")
	cmd.Flags().IntVar(&f.workers, "workers", 32, "driver worker pool size (per partition)")
	cmd.Flags().IntVar(&f.partitions, "partitions", 1, "number of tenant-partitioned runners (Session 8 — distributed mode)")
	cmd.Flags().IntVar(&f.bufferSize, "buffer-size", 4096, "events channel depth between generator and driver")
	cmd.Flags().StringVar(&f.metricsAddr, "metrics-addr", ":9095", "host:port for /metrics; pass empty to disable")
	cmd.Flags().IntVar(&f.pprofPort, "pprof-port", 0, "if > 0, /debug/pprof/* is served on the metrics-addr port")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding an admin bearer token (used as fallback when an event has no per-key token)")
	cmd.Flags().Float64Var(&f.fairnessMinShare, "fairness-min-share", 0, "tenant fairness scheduler — fraction of uniform-fair share each tenant is guaranteed (0 disables)")
	cmd.Flags().BoolVar(&f.iKnowWhatImDoing, "i-know-what-im-doing", false, "bypass safety checks (e.g. running long walk-tier scenarios against --target=local)")
	return cmd
}

// localLongRunGuardThreshold is the duration at which a --target=local run
// requires the explicit --i-know-what-im-doing acknowledgement. The walk-
// tier scenarios are sized for staging; running them locally for >1h will
// thrash a laptop's CPU + disk and can corrupt running services in the
// docker-compose stack.
const localLongRunGuardThreshold = time.Hour

func runRun(ctx context.Context, out, errOut io.Writer, f *runFlags) error {
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

	// Session 8 guardrail — long walk-tier runs against --target=local will
	// melt a laptop and risk corrupting the docker-compose backend. Require
	// an explicit acknowledgement.
	effectiveDuration := doc.Scenario.Duration.Std()
	if durationOverride > 0 {
		effectiveDuration = durationOverride
	}
	if target.Name == "local" && effectiveDuration > localLongRunGuardThreshold && !f.iKnowWhatImDoing {
		return fmt.Errorf("refusing to run %s against --target=local for %s (>%s).\n  Walk-tier scenarios are sized for staging; running locally for this duration\n  will saturate the laptop and risk corrupting the docker-compose backend.\n  Re-run with --i-know-what-im-doing to override, or shorten with --duration",
			doc.Scenario.Name, effectiveDuration, localLongRunGuardThreshold)
	}

	if f.partitions < 1 {
		return fmt.Errorf("--partitions must be >= 1")
	}
	if f.partitions > len(manifest.Tenants) {
		return fmt.Errorf("--partitions %d exceeds tenant count %d in manifest", f.partitions, len(manifest.Tenants))
	}

	outDir := f.out
	if outDir == "" {
		outDir = filepath.Join("runs", fmt.Sprintf("%s-%d", doc.Scenario.Name, time.Now().Unix()))
	}

	cfg := runner.Config{
		Scenario:                 doc.Scenario,
		Manifest:                 manifest,
		Target:                   target,
		OutputDir:                outDir,
		Workers:                  f.workers,
		DurationOverride:         durationOverride,
		BufferSize:               f.bufferSize,
		AdminToken:               os.Getenv(f.tokenEnv),
		MetricsAddr:              f.metricsAddr,
		PprofPort:                f.pprofPort,
		FairnessMinShareFraction: f.fairnessMinShare,
		WebhookSources:           loadWebhookSources(f.manifest),
	}

	// SIGINT/SIGTERM cancels the run; the runner drains in-flight and
	// writes partial output before returning.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if f.partitions > 1 {
		// Distributed mode — N tenant-partitioned runners in parallel.
		// Each handles its own slice of the manifest and writes per-
		// partition artifacts; the merged run.json lands at the parent
		// outDir for downstream consumers.
		fmt.Fprintf(out, "scenario:    %s\n", doc.Scenario.Name)
		fmt.Fprintf(out, "target:      %s\n", target.Name)
		fmt.Fprintf(out, "partitions:  %d (distributed mode)\n", f.partitions)
		fmt.Fprintf(out, "manifest:    %d tenants\n", len(manifest.Tenants))
		fmt.Fprintf(out, "duration:    %s\n", effectiveDuration)
		fmt.Fprintf(out, "target_tps:  %d (split across %d partitions)\n", doc.Scenario.TargetTPS, f.partitions)
		fmt.Fprintf(out, "out:         %s\n\n", outDir)

		res, err := runner.RunDistributed(ctx, cfg, runner.DistributedConfig{Partitions: f.partitions})
		if err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(errOut, "run error: %v\n", err)
		}
		if res != nil {
			printRunSummary(out, res, outDir)
		}
		return nil
	}

	r, err := runner.New(cfg)
	if err != nil {
		return err
	}
	printRunHeader(out, r, &cfg, doc.Scenario)
	res, err := r.Run(ctx)
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(errOut, "run error: %v\n", err)
	}
	if res != nil {
		printRunSummary(out, res, outDir)
	}
	return nil
}

// loadWebhookSources reads the per-tenant webhook source bundle from the
// directory containing manifestPath. Session 8 — the seed harness writes
// a sibling file `webhook_sources.json` when --provision-webhooks is
// passed; the run command transparently picks it up.
//
// Returns nil when no webhook sources file is present — webhook traffic
// then exercises the synthetic 404 fallback baked into the webhook driver.
func loadWebhookSources(manifestPath string) map[string]driver.WebhookSource {
	if manifestPath == "" {
		return nil
	}
	bundle, err := seed.LoadWebhookSources(manifestPath)
	if err != nil || len(bundle) == 0 {
		return nil
	}
	out := make(map[string]driver.WebhookSource, len(bundle))
	for k, v := range bundle {
		out[k] = driver.WebhookSource{
			SourceID:     v.SourceID,
			TenantID:     v.TenantID,
			Secret:       v.Secret,
			HeaderName:   v.HeaderName,
			Algorithm:    v.Algorithm,
			SignaturePfx: v.SignaturePfx,
		}
	}
	return out
}

func printRunHeader(out io.Writer, r *runner.Runner, cfg *runner.Config, s *scenario.Scenario) {
	fmt.Fprintf(out, "scenario:    %s\n", s.Name)
	fmt.Fprintf(out, "target:      %s\n", cfg.Target.Name)
	fmt.Fprintf(out, "manifest:    %d tenants\n", len(cfg.Manifest.Tenants))
	dur := cfg.Scenario.Duration.Std()
	if cfg.DurationOverride > 0 {
		dur = cfg.DurationOverride
	}
	fmt.Fprintf(out, "duration:    %s\n", dur)
	fmt.Fprintf(out, "target_tps:  %d\n", s.TargetTPS)
	fmt.Fprintf(out, "workers:     %d\n", cfg.Workers)
	fmt.Fprintf(out, "out:         %s\n", cfg.OutputDir)
	if addr := r.MetricsAddr(); addr != "" {
		fmt.Fprintf(out, "metrics:     http://%s/metrics\n", addr)
		if cfg.PprofPort > 0 {
			fmt.Fprintf(out, "pprof:       http://%s/debug/pprof/\n", addr)
		}
	}
	if s.NegativePaths.StaleKeysPct > 0 {
		fmt.Fprintf(out, "stale_keys:  injecting from %d stale subscriptions\n",
			countStaleSubs(cfg.Manifest))
	}
	fmt.Fprintln(out, "")
}

func printRunSummary(out io.Writer, res *runner.RunResult, outDir string) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "run complete")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "duration\t%s\n", res.Duration)
	fmt.Fprintf(tw, "events_generated\t%d\n", res.EventsGenerated)
	fmt.Fprintf(tw, "events_submitted\t%d\n", res.EventsSubmitted)
	fmt.Fprintf(tw, "events_succeeded\t%d (2xx)\n", res.EventsSucceeded)
	fmt.Fprintf(tw, "client_errors\t%d (4xx)\n", res.ClientErrors)
	fmt.Fprintf(tw, "server_errors\t%d (5xx)\n", res.ServerErrors)
	fmt.Fprintf(tw, "transport_failures\t%d\n", res.TransportFailures)
	fmt.Fprintf(tw, "circuit_open_skipped\t%d\n", res.CircuitOpenSkipped)
	fmt.Fprintf(tw, "expected_failures\t%d (negative-path induced)\n", res.ExpectedFailures)
	fmt.Fprintf(tw, "p50/p90/p99\t%.1f / %.1f / %.1f ms\n", res.LatencyP50ms, res.LatencyP90ms, res.LatencyP99ms)
	_ = tw.Flush()

	if len(res.NegativePathCounts) > 0 {
		fmt.Fprintln(out, "\nnegative paths:")
		twn := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, kind := range generator.AllNegativePaths {
			n := res.NegativePathCounts[kind]
			if n == 0 {
				continue
			}
			fmt.Fprintf(twn, "  %s\t%d\n", kind, n)
		}
		_ = twn.Flush()
	}
	fmt.Fprintf(out, "\nartifacts written to: %s/\n", outDir)
}

func countStaleSubs(m *seed.Manifest) int {
	if m == nil {
		return 0
	}
	count := 0
	for _, t := range m.Tenants {
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				if s.Stale {
					count++
				}
			}
		}
	}
	return count
}
