package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sort"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/scenarios"
)

type seedFlags struct {
	scenarioFlag      string
	target            string
	out               string
	dryRun            bool
	clean             bool
	cleanFromFile     string
	archetypesOnly    []string
	concurrency       int
	maxConcurrency    int
	minIntervalMS     int
	tokenEnv          string
	provisionWebhooks bool
	reuseTenantID     string
}

// newSeedCommand wires `aforo-loadgen seed`. The body is intentionally thin —
// each branch (dry-run, clean, real run) calls into internal/seed/.
func newSeedCommand(_ *GlobalFlags) *cobra.Command {
	var f seedFlags
	cmd := &cobra.Command{
		Use:   "seed",
		Short: "Provision tenants per archetype via Aforo's REST APIs",
		Long: `Seed materializes a scenario into the Aforo platform — one tenant
per archetype slot, each with products, billable units, a rate plan, an
offering, customers, subscriptions (including CANCELLED + EXPIRED for
stale-key tests), wallets, payment methods, discounts, and API keys.

Outputs a manifest.json that downstream subcommands (run, replay, lifecycle)
read to drive traffic at the seeded population.

Examples:
  aforo-loadgen seed --scenario scenarios/matrix-billing.yaml --target local
  aforo-loadgen seed --scenario scenarios/walk-realistic-50t.yaml --dry-run
  aforo-loadgen seed --scenario scenarios/matrix-billing.yaml --archetypes-only mtx-perunit-postpaid,mtx-flat-postpaid
  aforo-loadgen seed --clean --out manifest.json --target local
`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runSeed(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.scenarioFlag, "scenario", "", "path to scenario YAML (built-in name also accepted, e.g. matrix-billing)")
	cmd.Flags().StringVar(&f.target, "target", "local", "target environment: local, staging, prod, ci, or a full URL")
	cmd.Flags().StringVar(&f.out, "out", "manifest.json", "manifest.json output path")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "print per-archetype counts and intended API calls without sending them")
	cmd.Flags().BoolVar(&f.clean, "clean", false, "archive entities recorded in --out manifest, then exit")
	cmd.Flags().StringVar(&f.cleanFromFile, "clean-from", "", "alternative manifest path to clean (defaults to --out)")
	cmd.Flags().StringSliceVar(&f.archetypesOnly, "archetypes-only", nil, "comma-separated archetype names (subset)")
	cmd.Flags().IntVar(&f.concurrency, "concurrency", 4, "archetype-level worker concurrency")
	cmd.Flags().IntVar(&f.maxConcurrency, "max-http-concurrency", 50, "max concurrent in-flight HTTP requests")
	cmd.Flags().IntVar(&f.minIntervalMS, "min-interval-ms", 200, "minimum interval between any two HTTP requests")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding the bearer token")
	cmd.Flags().BoolVar(&f.provisionWebhooks, "provision-webhooks", false, "create one webhook ingest source per tenant via /api/v1/webhook-sources and write the bundle alongside the manifest (Session 8)")
	cmd.Flags().StringVar(&f.reuseTenantID, "reuse-tenant-id", "",
		"reuse an existing tenant id instead of minting a fresh `loadgen-tenant-...` per run. "+
			"Use this when the seeded products / customers / events need to be visible to "+
			"an operator UI session already authenticated as that tenant (e.g. `aforo_dev`). "+
			"Requires a single-tenant scenario; multi-tenant scenarios are rejected because "+
			"the workers would race on the same tenant id. Env: AFORO_LOADGEN_REUSE_TENANT_ID.")
	return cmd
}

func runSeed(ctx context.Context, out, errOut io.Writer, f *seedFlags) error {
	target, err := aforo.ResolveTarget(f.target)
	if err != nil {
		return err
	}
	token := os.Getenv(f.tokenEnv)

	clientCfg := seed.ClientConfig{
		Target:         target,
		BearerToken:    token,
		MaxConcurrency: f.maxConcurrency,
		MinInterval:    time.Duration(f.minIntervalMS) * time.Millisecond,
		DryRun:         f.dryRun,
	}
	c, err := seed.NewClient(clientCfg)
	if err != nil {
		return err
	}
	defer func() { _ = c.Close() }()

	// Wire SIGINT/SIGTERM to cancel the run gracefully — partial manifest is
	// still saved on cancel.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if f.clean {
		return runCleanFlow(ctx, out, errOut, c, f)
	}

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

	// Print per-archetype counts up front — for both dry and live runs.
	if err := printArchetypeAllocations(out, doc.Scenario, f.archetypesOnly); err != nil {
		return err
	}
	if f.dryRun {
		fmt.Fprintln(out, "(dry run — no API calls sent)")
	}

	manifestPath := f.out
	if manifestPath == "" {
		manifestPath = "manifest.json"
	}

	// --reuse-tenant-id: prefer flag, fall back to env. Reject when the
	// scenario allocates more than one tenant slot — multi-archetype runs
	// would race workers on the same tenant id, producing nonsense.
	reuseTenantID := f.reuseTenantID
	if reuseTenantID == "" {
		reuseTenantID = os.Getenv("AFORO_LOADGEN_REUSE_TENANT_ID")
	}
	if reuseTenantID != "" {
		// Single-tenant guard. Scenario.Tenants is a struct with .Count
		// (total) + .Archetypes (per-archetype distribution); reuse mode
		// is only safe when the total is 1, otherwise the goroutines in
		// Seeder.Run would race on the same tenant id.
		if doc.Scenario.Tenants.Count > 1 {
			return fmt.Errorf("--reuse-tenant-id requires a single-tenant scenario (got tenants.count=%d in %q); "+
				"multi-tenant runs would race on the same tenant id. Use a scenario with tenants.count=1.",
				doc.Scenario.Tenants.Count, f.scenarioFlag)
		}
		fmt.Fprintf(out, "reusing existing tenant id: %s (skipping tenant provisioning)\n", reuseTenantID)
	}

	seeder, err := seed.NewSeeder(seed.SeederConfig{
		Client:         c,
		Scenario:       doc.Scenario,
		OnlyArchetypes: f.archetypesOnly,
		ManifestPath:   manifestPath,
		Concurrency:    f.concurrency,
		ReuseTenantID:  reuseTenantID,
	})
	if err != nil {
		return err
	}

	res, err := seeder.Run(ctx)
	if err != nil {
		return err
	}
	if res != nil && res.Manifest != nil {
		printSummary(out, res.Manifest, manifestPath, len(res.Errors))
	}

	// Session 8 — optional webhook source provisioning. Run after the
	// main seed so we know the tenant population is in place.
	if f.provisionWebhooks && !f.dryRun && res != nil && res.Manifest != nil {
		bundle, errs := seed.ProvisionWebhookSources(ctx, c, res.Manifest)
		if path, saveErr := seed.SaveWebhookSources(manifestPath, bundle); saveErr == nil {
			fmt.Fprintf(out, "webhook sources: %d provisioned → %s\n", len(bundle), path)
		} else {
			fmt.Fprintf(errOut, "webhook sources: failed to save bundle: %v\n", saveErr)
		}
		for _, e := range errs {
			fmt.Fprintln(errOut, "webhook source: "+e.Error())
		}
	}

	if len(res.Errors) > 0 {
		for _, e := range res.Errors {
			fmt.Fprintln(errOut, e.Error())
		}
		return fmt.Errorf("seed completed with %d error(s)", len(res.Errors))
	}
	return nil
}

func runCleanFlow(ctx context.Context, out, errOut io.Writer, c *seed.Client, f *seedFlags) error {
	path := f.cleanFromFile
	if path == "" {
		path = f.out
	}
	if path == "" {
		path = "manifest.json"
	}
	m, err := seed.LoadManifest(path)
	if err != nil {
		return fmt.Errorf("load manifest %s: %w", path, err)
	}
	res := seed.Clean(ctx, c, m)
	fmt.Fprintf(out, "clean: archived %d tenants, %d customers, %d offerings, %d rate plans, %d products, %d wallets; canceled %d subscriptions\n",
		res.TenantsArchived, res.CustomersArchived, res.OfferingsArchived, res.RatePlansArchived, res.ProductsArchived, res.WalletsArchived, res.SubscriptionsCanceled)
	if len(res.Errors) > 0 {
		for _, e := range res.Errors {
			fmt.Fprintln(errOut, e.Error())
		}
		return fmt.Errorf("clean completed with %d error(s)", len(res.Errors))
	}
	return nil
}

func printArchetypeAllocations(out io.Writer, s *scenario.Scenario, only []string) error {
	allocs := seed.AllocateTenants(s)
	if len(only) > 0 {
		allocs = seed.FilterArchetypes(allocs, only)
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if _, err := fmt.Fprintln(tw, "ARCHETYPE\tCOUNT\tPRICING\tBILLING\tCUSTOMERS"); err != nil {
		return err
	}
	for _, a := range allocs {
		if _, err := fmt.Fprintf(tw, "%s\t%d\t%s\t%s\t%d\n",
			a.Archetype.Name, a.Count, a.Archetype.PricingModel, a.Archetype.BillingMode, a.Archetype.CustomerCount); err != nil {
			return err
		}
	}
	return tw.Flush()
}

func printSummary(out io.Writer, m *seed.Manifest, path string, errCount int) {
	s := m.Summary
	fmt.Fprintf(out, "\nseed complete: tenants=%d customers=%d subscriptions=%d stale_keys=%d errors=%d\n",
		s.TotalTenants, s.TotalCustomers, s.TotalSubs, s.StaleKeysCount, errCount)
	fmt.Fprintf(out, "manifest: %s (run_id=%s)\n", path, m.RunID)

	// Per-archetype breakdown.
	if len(s.ByArchetype) > 0 {
		fmt.Fprintln(out, "\nby archetype:")
		keys := sortedMapKeys(s.ByArchetype)
		tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
		for _, k := range keys {
			fmt.Fprintf(tw, "  %s\t%d\n", k, s.ByArchetype[k])
		}
		_ = tw.Flush()
	}
	if len(s.ByPricingModel) > 0 {
		fmt.Fprintln(out, "\nby pricing model:")
		keys := sortedMapKeys(s.ByPricingModel)
		for _, k := range keys {
			fmt.Fprintf(out, "  %s: %d\n", k, s.ByPricingModel[k])
		}
	}
	if len(s.ByBillingMode) > 0 {
		fmt.Fprintln(out, "\nby billing mode:")
		keys := sortedMapKeys(s.ByBillingMode)
		for _, k := range keys {
			fmt.Fprintf(out, "  %s: %d\n", k, s.ByBillingMode[k])
		}
	}
}

func sortedMapKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// loadScenario accepts either a filesystem path or a built-in scenario name.
func loadScenario(arg string) (*scenario.Document, error) {
	// Built-in scenario name? Try the embedded catalog first.
	if !strings.ContainsAny(arg, "/\\") && !strings.HasSuffix(arg, ".yaml") && !strings.HasSuffix(arg, ".yml") {
		data, readErr := scenarios.Read(arg)
		if readErr == nil {
			return scenario.LoadFromBytes(data)
		}
		// fall through to filesystem
	}
	return scenario.LoadFromFile(arg)
}
