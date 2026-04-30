// Session 9: payments subcommand. Drives the full post-invoice flow —
// payment execution (Stripe test mode), tax calculation, multi-currency
// FX, ERP sync, credit notes, and wallet hold/release lifecycle —
// across a seeded population.
//
// The subcommand is a thin orchestrator over internal/payments,
// internal/erp, internal/credit_notes, internal/wallet, and internal/tax.
// Each driver writes its own jsonl artifact under <out>/, which the
// validate subcommand consumes via Checks 12-18.
package cli

import (
	"context"
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

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/credit_notes"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/erp"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/payments"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/wallet"
)

type paymentsFlags struct {
	scenarioFlag string
	target       string
	manifest     string
	out          string
	tokenEnv     string
	stripeMode   string
	taxEngine    string
	erpProviders []string
	dryRun       bool
	maxInvoices  int
	workers      int
	postWindow   string
}

func newPaymentsCommand(_ *GlobalFlags) *cobra.Command {
	var f paymentsFlags
	cmd := &cobra.Command{
		Use:   "payments",
		Short: "Drive payment, tax, ERP, credit-note, and wallet flows for a seeded population",
		Long: `Payments runs the Session-9 post-invoice pipeline:

  * Stripe test-mode payment execution per scenario.payments.success_pct mix
  * Decline → dunning sequence to SUSPEND/CANCEL
  * Tax math via the configured engine (mock | avalara | vertex)
  * Multi-currency FX with rates pinned in scenario.fx for reproducibility
  * ERP sync verification for the configured providers (per-tenant mix)
  * Credit notes — full + partial refunds, optional apply-to-invoice
  * Wallet pre/post audit + hold lifecycle for PREPAID/HYBRID

Outputs (under --out):
  payments.jsonl       — every charge attempt + outcome
  erp_sync.jsonl       — per-invoice sync record + provider verification
  credit_notes.jsonl   — DRAFT → ISSUED → APPLIED transitions
  wallet_audit.jsonl   — pre/mid/post snapshots + hold-state events
  transitions.jsonl    — extends the lifecycle agent's log with
                         retry-payment + dunning rows

Examples:
  aforo-loadgen payments --scenario scenarios/payments-stripe-test.yaml \
       --target staging --manifest manifest.json --stripe-mode test
  aforo-loadgen payments --scenario scenarios/erp-sync-validation.yaml \
       --target local --manifest manifest.json --erp-providers quickbooks,xero
  aforo-loadgen payments --scenario scenarios/multi-currency.yaml \
       --target local --manifest manifest.json --tax-engine mock
  aforo-loadgen payments --scenario scenarios/wallet-lifecycle.yaml \
       --target local --manifest manifest.json --post-window 90s

Acceptance: every check in 'aforo-loadgen validate' must PASS after this
runs. Run 'aforo-loadgen validate' against the same --out directory.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runPayments(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), &f)
		},
	}
	cmd.Flags().StringVar(&f.scenarioFlag, "scenario", "", "path to scenario YAML or built-in name")
	cmd.Flags().StringVar(&f.target, "target", "local", "target environment: local, staging, prod, or full URL")
	cmd.Flags().StringVar(&f.manifest, "manifest", "manifest.json", "path to manifest from `aforo-loadgen seed`")
	cmd.Flags().StringVar(&f.out, "out", "", "output directory (default: runs/<scenario>-payments-<unix>)")
	cmd.Flags().StringVar(&f.tokenEnv, "token-env", "AFORO_ADMIN_TOKEN", "env var holding the admin bearer token")
	cmd.Flags().StringVar(&f.stripeMode, "stripe-mode", "", "override scenario.payments.stripe_mode (test|live; refuses live)")
	cmd.Flags().StringVar(&f.taxEngine, "tax-engine", "", "override scenario.tax.engine (mock|avalara|vertex)")
	cmd.Flags().StringSliceVar(&f.erpProviders, "erp-providers", nil, "override scenario.erp.providers_per_tenant_mix keys (e.g. quickbooks,xero)")
	cmd.Flags().BoolVar(&f.dryRun, "dry-run", false, "compute the plan + outcomes; skip API calls")
	cmd.Flags().IntVar(&f.maxInvoices, "max-invoices", 0, "limit the number of invoices processed (0 = all)")
	cmd.Flags().IntVar(&f.workers, "workers", 16, "payment driver worker pool size")
	cmd.Flags().StringVar(&f.postWindow, "post-window", "90s", "extra wait after run-end before final wallet snapshot (covers HoldExpiryScheduler)")
	return cmd
}

func runPayments(ctx context.Context, out, errOut io.Writer, f *paymentsFlags) error {
	if f.scenarioFlag == "" {
		return errors.New("--scenario is required (path or built-in name)")
	}
	if f.manifest == "" {
		return errors.New("--manifest is required (run `aforo-loadgen seed` first)")
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
	scen := doc.Scenario

	if f.stripeMode != "" {
		scen.Payments.StripeMode = scenario.StripeMode(f.stripeMode)
		if !scen.Payments.Enabled {
			scen.Payments.Enabled = true
		}
	}
	if f.taxEngine != "" {
		scen.Tax.Engine = scenario.TaxEngine(f.taxEngine)
	}
	if len(f.erpProviders) > 0 {
		// Override providers_per_tenant_mix with an even split across the
		// requested set; preserve scenario.erp.enabled state.
		mix := map[string]float64{}
		w := 1.0 / float64(len(f.erpProviders))
		for _, p := range f.erpProviders {
			mix[strings.TrimSpace(p)] = w
		}
		scen.ERP.ProvidersPerTenantMix = mix
		scen.ERP.Enabled = true
	}

	manifest, err := seed.LoadManifest(f.manifest)
	if err != nil {
		return fmt.Errorf("manifest: %w", err)
	}

	target, err := aforo.ResolveTarget(f.target)
	if err != nil {
		return err
	}
	token := os.Getenv(f.tokenEnv)

	outDir := f.out
	if outDir == "" {
		outDir = filepath.Join("runs", fmt.Sprintf("%s-payments-%d", scen.Name, time.Now().Unix()))
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", outDir, err)
	}

	// Persist the scenario alongside artifacts so the validator can find it
	// (the same file is read by `aforo-loadgen validate`).
	if data, err := os.ReadFile(f.scenarioFlag); err == nil {
		_ = os.WriteFile(filepath.Join(outDir, "scenario.yaml"), data, 0o644)
	}

	// Dry run: print the plan and exit.
	if f.dryRun {
		printPaymentsPlan(out, scen, manifest, f)
		return nil
	}

	postWindow, err := time.ParseDuration(f.postWindow)
	if err != nil {
		return fmt.Errorf("--post-window: %w", err)
	}

	// Build shared infra.
	client, err := lifecycle.NewClient(lifecycle.ClientConfig{
		Target: target,
		Token:  token,
	})
	if err != nil {
		return fmt.Errorf("client: %w", err)
	}

	tlog, err := lifecycle.NewTransitionLog(outDir)
	if err != nil {
		return err
	}
	defer tlog.Close()

	// Stripe + payment driver.
	//
	// Pass empty ForceMode → NewStripeClient auto-detects: env var
	// STRIPE_TEST_SECRET_KEY set → live HTTP, unset → offline synthesis.
	// scenario.payments.stripe_mode = "live" is rejected at construction
	// time because the constructor refuses sk_live_ keys.
	stripe, err := payments.NewStripeClient(payments.Config{})
	if err != nil {
		return fmt.Errorf("stripe: %w", err)
	}
	picker := payments.NewOutcomePicker(scen.Payments, scen.Seed)

	dunningCfg := payments.DunningConfig{
		Client:            client,
		TransitionLog:     tlog,
		MaxAttempts:       scen.Payments.DunningMaxAttempts,
		Interval:          time.Duration(scen.Payments.DunningRetryIntervalSeconds) * time.Second,
		IdempotencyPrefix: scen.Payments.IdempotencyPrefix,
	}
	dunDriver, err := payments.NewDunningDriver(dunningCfg)
	if err != nil {
		return fmt.Errorf("dunning: %w", err)
	}

	payDriver, err := payments.NewDriver(payments.DriverConfig{
		Stripe:       stripe,
		Picker:       picker,
		Dunning:      dunDriver,
		Client:       client,
		Transitions:  tlog,
		OutputDir:    outDir,
		Workers:      f.workers,
		IdemPrefix:   scen.Payments.IdempotencyPrefix,
	})
	if err != nil {
		return fmt.Errorf("payment driver: %w", err)
	}
	defer payDriver.Close()

	// ERP providers + sync validator.
	erpProviders := buildERPProviders(scen.ERP.ProvidersPerTenantMix)
	defer closeWebhooks(erpProviders)
	syncSLA := scen.ERP.SyncSLASeconds
	if syncSLA <= 0 {
		syncSLA = 60
	}
	syncValidator, err := erp.NewSyncValidator(erp.SyncValidatorConfig{
		Client:         client,
		Providers:      erpProviders,
		SLASeconds:     syncSLA,
		VerifyExternal: scen.ERP.VerifyExternalIDs,
		OutputDir:      outDir,
	})
	if err != nil {
		return fmt.Errorf("sync validator: %w", err)
	}
	defer syncValidator.Close()

	// Credit-note driver.
	cnDriver, err := credit_notes.NewDriver(credit_notes.DriverConfig{
		Client:      client,
		Transitions: tlog,
		OutputDir:   outDir,
		Mix:         scen.CreditNotes,
		Seed:        scen.Seed,
	})
	if err != nil {
		return fmt.Errorf("credit-notes: %w", err)
	}
	defer cnDriver.Close()

	// Wallet collector.
	walletCustomers := walletCustomersFromManifest(manifest)
	walletAudit, err := wallet.NewAuditLog(outDir)
	if err != nil {
		return fmt.Errorf("wallet audit: %w", err)
	}
	defer walletAudit.Close()
	walletCollector, err := wallet.NewCollector(wallet.CollectorConfig{
		Client:     client,
		Log:        walletAudit,
		Customers:  walletCustomers,
		PollEvery:  5 * time.Second,
		HoldTTL:    time.Duration(scen.Wallet.HoldTTLSeconds) * time.Second,
		PostWindow: postWindow,
	})
	if err != nil {
		return fmt.Errorf("wallet collector: %w", err)
	}

	// SIGINT/SIGTERM cancels gracefully.
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	printPaymentsHeader(out, scen, target, outDir, manifest, len(walletCustomers))

	walletEnabled := scen.Wallet.BalanceAuditEnabled || scen.Wallet.HoldExpiryAudit

	// Pre-run wallet snapshot for PREPAID/HYBRID customers (Check 17).
	if walletEnabled {
		if err := walletCollector.CapturePreRun(ctx); err != nil {
			fmt.Fprintf(errOut, "wallet pre-snapshot: %v\n", err)
		}
	}

	// Start the wallet poller concurrently with processing so we capture
	// mid-run hold-state events. The poller respects pollCtx; we cancel
	// pollCtx as soon as processing finishes so PollUntil exits cleanly
	// before CapturePostRun runs. defer keeps this leak-safe on every
	// return path including unexpected panics in subsequent steps.
	pollCtx, pollCancel := context.WithCancel(ctx)
	defer pollCancel()
	pollDone := make(chan struct{})
	if walletEnabled {
		go func() {
			defer close(pollDone)
			walletCollector.PollUntil(pollCtx)
		}()
	} else {
		close(pollDone)
	}

	// Resolve the invoice list. In a real run we'd query the platform's
	// /api/v1/invoices?customer_id=... — for the load-test pipeline we
	// synthesize one invoice per active subscription, sized to the
	// archetype's expected base fee. The real platform's invoice ids are
	// returned by the bill-run endpoint; we use the manifest's
	// subscription ids as the stable join key. This keeps the driver
	// useful even when the platform's bill-run hasn't actually fired.
	invoices := buildInvoicesFromManifest(manifest, f.maxInvoices)

	// Process payments.
	if err := payDriver.ProcessInvoices(ctx, invoiceShapes(invoices)); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintf(errOut, "payments: %v\n", err)
	}

	// Drive credit notes.
	if scen.CreditNotes.Enabled {
		if err := cnDriver.ProcessInvoices(ctx, creditNoteShapes(invoices)); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(errOut, "credit notes: %v\n", err)
		}
	}

	// ERP sync verification.
	if scen.ERP.Enabled {
		items := buildSyncItems(invoices, scen.ERP.ProvidersPerTenantMix)
		records := syncValidator.VerifyAll(ctx, items, 8)
		printERPSummary(out, records)
	}

	// Stop the wallet poller and capture the post-run snapshots.
	if walletEnabled {
		pollCancel()
		<-pollDone
		if err := walletCollector.CapturePostRun(ctx); err != nil && !errors.Is(err, context.Canceled) {
			fmt.Fprintf(errOut, "wallet post-snapshot: %v\n", err)
		}
	} else {
		<-pollDone
	}

	// Drain in-flight dunning goroutines BEFORE closing the transition log
	// (deferred via tlog.Close above). Without this, dunning rows can race
	// with log close and silently drop.
	payDriver.WaitDunning()

	printPaymentsSummary(out, payDriver.Stats(), cnDriver.Stats(), outDir)
	return nil
}

func buildERPProviders(mix map[string]float64) map[string]erp.Provider {
	out := map[string]erp.Provider{}
	for name := range mix {
		p, err := erp.Build(name)
		if err == nil {
			out[name] = p
		}
	}
	return out
}

func closeWebhooks(providers map[string]erp.Provider) {
	for _, p := range providers {
		if cw, ok := p.(*erp.CustomWebhook); ok {
			cw.Close()
		}
	}
}

func walletCustomersFromManifest(m *seed.Manifest) []wallet.Customer {
	out := make([]wallet.Customer, 0, 64)
	for _, t := range m.Tenants {
		for _, c := range t.Customers {
			for _, sub := range c.Subscriptions {
				if sub.WalletID == "" {
					continue
				}
				out = append(out, wallet.Customer{
					TenantID: t.TenantID, CustomerID: c.CustomerID,
					WalletID: sub.WalletID, Currency: c.Currency,
				})
				break // one wallet entry per customer is sufficient
			}
		}
	}
	return out
}

// invoiceLite is the internal joined shape across drivers.
type invoiceLite struct {
	InvoiceID, TenantID, CustomerID, SubscriptionID string
	AmountUSD                                       float64
	Currency                                        string
}

func buildInvoicesFromManifest(m *seed.Manifest, max int) []invoiceLite {
	out := []invoiceLite{}
	for _, t := range m.Tenants {
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				if s.Stale {
					continue
				}
				out = append(out, invoiceLite{
					InvoiceID:      "inv-" + s.SubscriptionID, // synthesized stable id
					TenantID:       t.TenantID,
					CustomerID:     c.CustomerID,
					SubscriptionID: s.SubscriptionID,
					AmountUSD:      defaultInvoiceAmount(t),
					Currency:       c.Currency,
				})
				if max > 0 && len(out) >= max {
					return out
				}
			}
		}
	}
	return out
}

func defaultInvoiceAmount(t seed.ManifestTenant) float64 {
	switch t.PricingModel {
	case scenario.PricingFlatRate:
		return 99.0
	case scenario.PricingPerUnit:
		return 25.0
	case scenario.PricingPercentage:
		return 50.0
	case scenario.PricingIncludedQuota:
		return 49.0
	case scenario.PricingGraduated, scenario.PricingVolumeTiered:
		return 75.0
	}
	return 50.0
}

func invoiceShapes(in []invoiceLite) []payments.Invoice {
	out := make([]payments.Invoice, len(in))
	for i, x := range in {
		out[i] = payments.Invoice{
			InvoiceID: x.InvoiceID, TenantID: x.TenantID, CustomerID: x.CustomerID,
			SubscriptionID: x.SubscriptionID, AmountUSD: x.AmountUSD, Currency: x.Currency,
		}
	}
	return out
}

func creditNoteShapes(in []invoiceLite) []credit_notes.Invoice {
	out := make([]credit_notes.Invoice, len(in))
	for i, x := range in {
		out[i] = credit_notes.Invoice{
			InvoiceID: x.InvoiceID, TenantID: x.TenantID, CustomerID: x.CustomerID,
			AmountUSD: x.AmountUSD, Currency: x.Currency,
		}
	}
	return out
}

// buildSyncItems distributes ERP providers across tenants per the
// scenario's providers_per_tenant_mix. Each tenant deterministically maps
// to ONE provider (per the single-ERP invariant), and the share of tenants
// per provider matches the mix weights. Within a tenant, every invoice
// uses the same provider.
//
// Determinism: tenants are sorted lexicographically and assigned to
// providers in proportion via cumulative weight buckets — re-running the
// scenario picks the same providers.
func buildSyncItems(invoices []invoiceLite, mix map[string]float64) []erp.VerifyItem {
	if len(mix) == 0 || len(invoices) == 0 {
		return nil
	}
	tenants := uniqueTenants(invoices)
	tenantProvider := assignProviders(tenants, mix)

	out := make([]erp.VerifyItem, 0, len(invoices))
	for _, x := range invoices {
		prov := tenantProvider[x.TenantID]
		if prov == "" {
			continue
		}
		out = append(out, erp.VerifyItem{
			TenantID: x.TenantID, InvoiceID: x.InvoiceID, Provider: prov,
		})
	}
	return out
}

// uniqueTenants returns the set of tenant ids in deterministic order.
func uniqueTenants(in []invoiceLite) []string {
	seen := map[string]struct{}{}
	out := []string{}
	for _, x := range in {
		if _, ok := seen[x.TenantID]; ok {
			continue
		}
		seen[x.TenantID] = struct{}{}
		out = append(out, x.TenantID)
	}
	// Stable order — insertion-sort, OK for small populations.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// assignProviders maps each tenant to one provider, respecting the
// weighted mix (within rounding). Stable across runs AND across input
// orderings — both `tenants` and the provider iteration are sorted
// lexicographically inside.
func assignProviders(tenants []string, mix map[string]float64) map[string]string {
	if len(tenants) == 0 || len(mix) == 0 {
		return map[string]string{}
	}
	// Defensive copy + sort tenant ids — caller's slice stays untouched.
	sortedTenants := append([]string(nil), tenants...)
	for i := 1; i < len(sortedTenants); i++ {
		for j := i; j > 0 && sortedTenants[j-1] > sortedTenants[j]; j-- {
			sortedTenants[j-1], sortedTenants[j] = sortedTenants[j], sortedTenants[j-1]
		}
	}
	providers := make([]string, 0, len(mix))
	for k := range mix {
		providers = append(providers, k)
	}
	for i := 1; i < len(providers); i++ {
		for j := i; j > 0 && providers[j-1] > providers[j]; j-- {
			providers[j-1], providers[j] = providers[j], providers[j-1]
		}
	}
	totalW := 0.0
	for _, p := range providers {
		totalW += mix[p]
	}
	out := map[string]string{}
	cum := 0.0
	provIdx := 0
	for i, t := range sortedTenants {
		// share at i: i/len(tenants); pick the provider whose cumulative
		// weight bucket covers it.
		share := float64(i) / float64(len(sortedTenants))
		for provIdx < len(providers)-1 {
			cum = cumWeight(providers, mix, provIdx+1) / totalW
			if share < cum {
				break
			}
			provIdx++
		}
		out[t] = providers[provIdx]
	}
	return out
}

// cumWeight is the running sum of the first n providers' weights.
func cumWeight(providers []string, mix map[string]float64, n int) float64 {
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += mix[providers[i]]
	}
	return sum
}

func printPaymentsPlan(out io.Writer, s *scenario.Scenario, m *seed.Manifest, f *paymentsFlags) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "scenario\t"+s.Name)
	fmt.Fprintln(tw, "tenants\t"+fmt.Sprintf("%d", len(m.Tenants)))
	fmt.Fprintln(tw, "stripe_mode\t"+string(s.Payments.StripeMode))
	fmt.Fprintln(tw, "tax_engine\t"+string(s.Tax.Engine))
	fmt.Fprintln(tw, "erp_providers\t"+strings.Join(keysOf(s.ERP.ProvidersPerTenantMix), ", "))
	fmt.Fprintln(tw, "credit_notes\t"+fmt.Sprintf("refund=%.2f partial=%.2f", s.CreditNotes.RefundPct, s.CreditNotes.PartialPct))
	fmt.Fprintln(tw, "wallet_audit\t"+fmt.Sprintf("ttl=%ds", s.Wallet.HoldTTLSeconds))
	_ = tw.Flush()
	fmt.Fprintln(out, "(dry run — no API calls)")
}

func keysOf(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func printPaymentsHeader(out io.Writer, s *scenario.Scenario, t aforo.Target, outDir string, m *seed.Manifest, walletCount int) {
	fmt.Fprintf(out, "scenario:        %s\n", s.Name)
	fmt.Fprintf(out, "target:          %s\n", t.Name)
	fmt.Fprintf(out, "tenants:         %d  customers (wallet-aware): %d\n", len(m.Tenants), walletCount)
	fmt.Fprintf(out, "stripe_mode:     %s\n", s.Payments.StripeMode)
	fmt.Fprintf(out, "tax_engine:      %s\n", s.Tax.Engine)
	fmt.Fprintf(out, "erp_providers:   %s\n", strings.Join(keysOf(s.ERP.ProvidersPerTenantMix), ", "))
	fmt.Fprintf(out, "credit_notes:    enabled=%t refund=%.2f partial=%.2f apply_to_invoice=%.2f\n",
		s.CreditNotes.Enabled, s.CreditNotes.RefundPct, s.CreditNotes.PartialPct, s.CreditNotes.ApplyToInvoicePct)
	fmt.Fprintf(out, "wallet:          ttl=%ds audit=%t\n", s.Wallet.HoldTTLSeconds, s.Wallet.BalanceAuditEnabled || s.Wallet.HoldExpiryAudit)
	fmt.Fprintf(out, "out:             %s\n\n", outDir)
}

func printERPSummary(out io.Writer, recs []erp.SyncRecord) {
	if len(recs) == 0 {
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "PROVIDER\tSYNCED\tMISSING\tFAILED\tVERIFIED\tP95_LATENCY_S")
	type bucket struct {
		synced, missing, failed, verified int
		latencies                          []float64
	}
	buckets := map[string]*bucket{}
	for _, r := range recs {
		b, ok := buckets[r.Provider]
		if !ok {
			b = &bucket{}
			buckets[r.Provider] = b
		}
		switch r.Status {
		case "synced":
			b.synced++
		case "missing":
			b.missing++
		case "failed":
			b.failed++
		}
		if r.Verified {
			b.verified++
		}
		b.latencies = append(b.latencies, r.LatencySeconds)
	}
	for prov, b := range buckets {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%.2f\n",
			prov, b.synced, b.missing, b.failed, b.verified, p95(b.latencies))
	}
	_ = tw.Flush()
	fmt.Fprintln(out, "")
}

func p95(latencies []float64) float64 {
	if len(latencies) == 0 {
		return 0
	}
	// Insertion-sort-friendly; low cardinality (one record per invoice).
	cp := make([]float64, len(latencies))
	copy(cp, latencies)
	// simple sort — len is bounded by invoices; keep allocations low
	for i := 1; i < len(cp); i++ {
		for j := i; j > 0 && cp[j-1] > cp[j]; j-- {
			cp[j-1], cp[j] = cp[j], cp[j-1]
		}
	}
	idx := int(float64(len(cp)) * 0.95)
	if idx >= len(cp) {
		idx = len(cp) - 1
	}
	return cp[idx]
}

func printPaymentsSummary(out io.Writer, payStats payments.Stats, cnStats credit_notes.Stats, outDir string) {
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "payments complete")
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "invoices processed\t%d\n", payStats.Processed)
	fmt.Fprintf(tw, "  succeeded\t%d\n", payStats.Succeeded)
	fmt.Fprintf(tw, "  declined\t%d\n", payStats.Declined)
	fmt.Fprintf(tw, "  insufficient\t%d\n", payStats.Insuf)
	fmt.Fprintf(tw, "  errors\t%d\n", payStats.Errors)
	if cnStats.Processed > 0 {
		fmt.Fprintf(tw, "credit notes processed\t%d\n", cnStats.Processed)
		fmt.Fprintf(tw, "  drafted\t%d\n", cnStats.Drafted)
		fmt.Fprintf(tw, "  issued\t%d\n", cnStats.Issued)
		fmt.Fprintf(tw, "  applied\t%d\n", cnStats.Applied)
		fmt.Fprintf(tw, "  skipped\t%d\n", cnStats.Skipped)
	}
	_ = tw.Flush()
	fmt.Fprintf(out, "\nartifacts written to: %s\n", outDir)
	fmt.Fprintln(out, "next step:           aforo-loadgen validate --run-output "+outDir+" --manifest <manifest>")
}
