package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/coord"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/cost"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

type coordFlags struct {
	scenarioFlag     string
	target           string
	manifest         string
	out              string
	workersCSV       string
	heartbeat        string
	dropoutTimeout   string
	durationOverride string
	yes              bool

	// mTLS
	tlsCert string
	tlsKey  string
	tlsCA   string
	tlsName string
}

// newCoordinatorCommand wires `aforo-loadgen coordinator`.
//
// The headline command for Session 11 — multi-machine distributed mode.
// Each --workers entry is a worker host:port; the coordinator partitions
// the manifest's tenants by stable fnv hash and POSTs an Assignment to
// each worker over HTTP/2 + mTLS.
func newCoordinatorCommand(_ *GlobalFlags) *cobra.Command {
	var f coordFlags
	cmd := &cobra.Command{
		Use:   "coordinator",
		Short: "Run a scenario across N worker nodes (multi-machine distributed mode)",
		Long: `Coordinator partitions tenants across the worker fleet and orchestrates the run.

The headline command for the run-tier phase: 15K TPS sustained across 8
worker nodes, with chaos events, soak monitoring, and cost tracking.

Pre-flight: prints the projected event count, estimated AWS cost, and
prompts to confirm before dispatch. Pass --yes to skip the prompt for
automated runs.

Workers: each --workers entry is host:port reachable from the coordinator.
The coordinator dials every worker before assigning to surface bad certs
or unreachable hosts up-front.

Examples:
  aforo-loadgen coordinator \
    --scenario scenarios/run-15k-7day.yaml \
    --target perf-aws \
    --manifest manifest.json \
    --workers 10.0.0.10:7070,10.0.0.11:7070,10.0.0.12:7070 \
    --tls-cert tls/coord.pem --tls-key tls/coord.key --tls-ca tls/ca.pem \
    --duration 30m   # subset run for the integration test
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runCoordinator(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.scenarioFlag, "scenario", "", "scenario YAML path or built-in name (required)")
	cmd.Flags().StringVar(&f.target, "target", "", "target environment name (must be in chaos allow-list, e.g. perf-aws)")
	cmd.Flags().StringVar(&f.manifest, "manifest", "manifest.json", "path to manifest from `aforo-loadgen seed`")
	cmd.Flags().StringVar(&f.out, "out", "", "coordinator output dir (default: runs/<scenario>-<unix>)")
	cmd.Flags().StringVar(&f.workersCSV, "workers", "", "comma-separated worker addresses (host:port,...)")
	cmd.Flags().StringVar(&f.heartbeat, "heartbeat", "5s", "heartbeat poll interval")
	cmd.Flags().StringVar(&f.dropoutTimeout, "dropout-timeout", "30s", "time without heartbeat after which a worker is declared dropped")
	cmd.Flags().StringVar(&f.durationOverride, "duration", "", "override scenario.duration (e.g. 30m)")
	cmd.Flags().BoolVar(&f.yes, "yes", false, "skip pre-flight cost confirmation prompt")
	cmd.Flags().StringVar(&f.tlsCert, "tls-cert", "", "coordinator client cert PEM")
	cmd.Flags().StringVar(&f.tlsKey, "tls-key", "", "coordinator client key PEM")
	cmd.Flags().StringVar(&f.tlsCA, "tls-ca", "", "CA bundle PEM (verifies workers' certs)")
	cmd.Flags().StringVar(&f.tlsName, "tls-server-name", "", "expected server name on worker certs (defaults to host portion of each --workers entry)")
	return cmd
}

func runCoordinator(ctx context.Context, out, errOut io.Writer, f *coordFlags) error {
	if f.scenarioFlag == "" {
		return errors.New("--scenario is required")
	}
	if f.target == "" {
		return errors.New("--target is required (e.g. perf-aws)")
	}
	if f.workersCSV == "" {
		return errors.New("--workers is required (comma-separated host:port list)")
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
	manifestBytes, err := manifest.MarshalManifest()
	if err != nil {
		return fmt.Errorf("manifest marshal: %w", err)
	}

	// Resolve target — Target.Name should be the literal --target string
	// when the chaos allow list is matching ("perf-aws"); the
	// coordinator does NOT need URLs because it never produces traffic.
	if !looksLikePerfTarget(f.target) {
		return fmt.Errorf("--target %q is not a perf-* target; coordinator refuses to drive a 15K TPS run against a non-perf environment", f.target)
	}

	workerAddrs := parseCSV(f.workersCSV)
	if len(workerAddrs) == 0 {
		return errors.New("--workers must list at least one host:port")
	}
	if len(workerAddrs) > len(manifest.Tenants) {
		return fmt.Errorf("--workers count %d exceeds manifest tenant count %d", len(workerAddrs), len(manifest.Tenants))
	}

	heartbeatDur, err := time.ParseDuration(f.heartbeat)
	if err != nil {
		return fmt.Errorf("--heartbeat: %w", err)
	}
	dropoutDur, err := time.ParseDuration(f.dropoutTimeout)
	if err != nil {
		return fmt.Errorf("--dropout-timeout: %w", err)
	}
	var durationOverride time.Duration
	if f.durationOverride != "" {
		durationOverride, err = time.ParseDuration(f.durationOverride)
		if err != nil {
			return fmt.Errorf("--duration: %w", err)
		}
	}

	mtls := coord.MTLSConfig{
		CertFile:   f.tlsCert,
		KeyFile:    f.tlsKey,
		CAFile:     f.tlsCA,
		ServerName: f.tlsName,
	}

	// Pre-flight: project events + cost, prompt unless --yes.
	scn := *doc.Scenario
	effectiveDuration := scn.Duration.Std()
	if durationOverride > 0 {
		effectiveDuration = durationOverride
	}
	preflight := cost.PreflightEstimate(cost.DefaultRates, scn.TargetTPS, effectiveDuration, len(workerAddrs))
	printPreflight(out, &scn, f.target, len(workerAddrs), effectiveDuration, preflight)
	if !f.yes {
		ok, err := promptYesNo(out, errOut, fmt.Sprintf(
			"\nAbout to send %d TPS to %s. Generates ~%s events / %s.\nEstimated cost: $%.2f. Continue? [yes/NO] ",
			scn.TargetTPS, f.target, prettyEvents(preflight.EventsIngested), effectiveDuration, preflight.TotalUSD))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(out, "aborted")
			return nil
		}
	}

	// Construct the coordinator (this dials every worker — fail-closed).
	c, err := coord.NewCoordinator(coord.CoordinatorConfig{
		WorkerAddrs:       workerAddrs,
		MTLS:              mtls,
		HeartbeatInterval: heartbeatDur,
		DropoutTimeout:    dropoutDur,
		Logger:            func(format string, args ...any) { fmt.Fprintf(out, format+"\n", args...) },
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}
	defer c.Close()

	// Marshal scenario for transport.
	scenarioBytes, err := yaml.Marshal(doc.Scenario)
	if err != nil {
		return fmt.Errorf("scenario marshal: %w", err)
	}

	// Tenant ID list straight from the manifest, in deterministic order.
	tenantIDs := make([]string, 0, len(manifest.Tenants))
	for _, t := range manifest.Tenants {
		tenantIDs = append(tenantIDs, t.TenantID)
	}

	// SIGINT/SIGTERM cancels the run; the coordinator drains and
	// emits a partial report.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	fmt.Fprintf(out, "\ncoordinator dispatching %d tenants to %d worker(s)...\n", len(tenantIDs), len(workerAddrs))
	if _, err := c.Dispatch(ctx, coord.PartitionConfig{
		TenantIDs:        tenantIDs,
		ScenarioYAML:     string(scenarioBytes),
		ManifestJSON:     string(manifestBytes),
		TargetName:       f.target,
		TotalTargetTPS:   scn.TargetTPS,
		DurationOverride: durationOverride,
	}); err != nil {
		fmt.Fprintf(errOut, "dispatch: %v\n", err)
		// Fall through to abort + report.
	}

	// Poll until done. Returns when ctx cancels or all workers terminal.
	pollErr := c.PollUntilDone(ctx)
	if pollErr != nil && !errors.Is(pollErr, context.Canceled) {
		fmt.Fprintf(errOut, "poll: %v\n", pollErr)
	}

	// Aggregate + write run.json.
	agg := c.AggregateReports(context.Background())

	// Build cost tracker from aggregate.
	tracker := cost.NewTracker(cost.DefaultRates)
	tracker.SetWorkerCount(len(workerAddrs))
	tracker.Start(agg.StartedAt)
	tracker.Stop(agg.StoppedAt)
	tracker.AddEventsIngested(agg.EventsSucceeded)
	tracker.AddEventsAttempted(agg.EventsSubmitted)
	tracker.IncludeStorage = effectiveDuration >= 24*time.Hour
	costBreakdown := tracker.Estimate()

	outDir := f.out
	if outDir == "" {
		outDir = filepath.Join("runs", fmt.Sprintf("%s-coord-%d", scn.Name, time.Now().Unix()))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}
	doc2 := struct {
		Aggregate coord.AggregateResult `json:"aggregate"`
		Cost      cost.Breakdown        `json:"cost_estimate"`
	}{Aggregate: agg, Cost: costBreakdown}
	buf, err := json.MarshalIndent(doc2, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.WriteFile(filepath.Join(outDir, "run.json"), buf, 0o644); err != nil {
		return fmt.Errorf("write run.json: %w", err)
	}

	printAggregateSummary(out, agg, costBreakdown, outDir)
	if pollErr != nil && !errors.Is(pollErr, context.Canceled) {
		// Surface the poll error as a non-zero exit so CI catches it.
		return pollErr
	}
	return nil
}

// printPreflight renders the pre-flight summary table.
func printPreflight(out io.Writer, s *scenario.Scenario, target string, workers int, duration time.Duration, bd cost.Breakdown) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "═══ pre-flight check ═══")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "scenario\t%s\n", s.Name)
	fmt.Fprintf(tw, "target\t%s\n", target)
	fmt.Fprintf(tw, "workers\t%d\n", workers)
	fmt.Fprintf(tw, "tenants\t%d\n", s.Tenants.Count)
	fmt.Fprintf(tw, "target_tps\t%d\n", s.TargetTPS)
	fmt.Fprintf(tw, "duration\t%s\n", duration)
	fmt.Fprintf(tw, "projected events\t%s\n", prettyEvents(bd.EventsIngested))
	fmt.Fprintf(tw, "estimated cost\t$%.2f USD (%s)\n", bd.TotalUSD, bd.Region)
	fmt.Fprintf(tw, "  worker compute\t$%.2f\n", bd.WorkerComputeUSD)
	fmt.Fprintf(tw, "  msk + redis\t$%.2f\n", bd.KafkaMSKUSD+bd.RedisElasticacheUSD)
	fmt.Fprintf(tw, "  egress (%.0f GB)\t$%.2f\n", bd.EgressGB, bd.EgressUSD)
	if bd.ClickHouseStorageGB > 0 {
		fmt.Fprintf(tw, "  ch storage (apportioned)\t$%.2f\n", bd.ClickHouseStorageUSDPerMonth*duration.Hours()/720.0)
	}
	fmt.Fprintf(tw, "per million events\t$%.4f\n", bd.PerMillionEventsUSD)
	fmt.Fprintf(tw, "note\t%s\n", bd.EstimateNote)
	_ = tw.Flush()
}

// promptYesNo reads a line from stdin and returns true on "yes" (case-
// insensitive). All other inputs (including empty / EOF) → false.
func promptYesNo(out, errOut io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	if err != nil && err != io.EOF {
		return false, fmt.Errorf("read stdin: %w", err)
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "yes" || answer == "y", nil
}

// prettyEvents renders an event count in human-friendly form
// (1.23M, 4.5B, etc.).
func prettyEvents(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}

// looksLikePerfTarget approves "perf-*" target names so the coordinator
// refuses to fire chaos-bearing scenarios at non-perf clusters. The
// chaos package re-checks at the worker side; this is the up-front
// fail-closed.
func looksLikePerfTarget(name string) bool {
	return strings.HasPrefix(name, "perf-")
}

// parseCSV splits a comma-separated list, trimming whitespace.
func parseCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func printAggregateSummary(out io.Writer, agg coord.AggregateResult, bd cost.Breakdown, outDir string) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "═══ aggregate run summary ═══")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "run_id\t%s\n", agg.RunID)
	fmt.Fprintf(tw, "duration\t%s\n", agg.StoppedAt.Sub(agg.StartedAt))
	fmt.Fprintf(tw, "workers\t%d/%d reporting\n", agg.WorkersReported, agg.Workers)
	if len(agg.WorkersDropped) > 0 {
		fmt.Fprintf(tw, "dropped\t%v\n", agg.WorkersDropped)
	}
	fmt.Fprintf(tw, "events_succeeded\t%d\n", agg.EventsSucceeded)
	fmt.Fprintf(tw, "events_failed\t%d\n", agg.EventsFailed)
	fmt.Fprintf(tw, "p50/p90/p99\t%.1f / %.1f / %.1f ms\n", agg.LatencyP50Ms, agg.LatencyP90Ms, agg.LatencyP99Ms)
	fmt.Fprintf(tw, "estimated cost\t$%.2f (%s)\n", bd.TotalUSD, bd.Region)
	fmt.Fprintf(tw, "per million events\t$%.4f\n", bd.PerMillionEventsUSD)
	_ = tw.Flush()
	fmt.Fprintf(out, "\nartifacts written to: %s/run.json\n", outDir)
	fmt.Fprintf(out, "for ground truth billing data, see: %s\n", bd.AWSCostExplorerURL)
}
