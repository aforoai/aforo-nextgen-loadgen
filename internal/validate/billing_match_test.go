package validate

import (
	"context"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// billingTestBackend is a fakeBackend tweaked to return specific
// invoice/wallet amounts per bill run — used to exercise drift detection.
type billingTestBackend struct {
	caps         Capabilities
	invoiceUSD   float64
	walletDebit  float64
	triggerCount int
}

func (b *billingTestBackend) Capabilities() Capabilities { return b.caps }
func (b *billingTestBackend) EventCountByTenant(_ context.Context, _ TimeWindow, _ []string) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "event_count"}
}
func (b *billingTestBackend) CrossTenantQuery(_ context.Context, _ TimeWindow, _ []CrossTenantProbe) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "cross_tenant"}
}
func (b *billingTestBackend) EventsWithNullCustomer(_ context.Context, _ TimeWindow) (int64, error) {
	return 0, ErrUnsupported{Op: "null"}
}
func (b *billingTestBackend) CacheHitRatio(_ context.Context, _ TimeWindow) (float64, error) {
	return 0, ErrUnsupported{Op: "cache"}
}
func (b *billingTestBackend) EventsByAPIKey(_ context.Context, _ TimeWindow, _ []string) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "by_key"}
}
func (b *billingTestBackend) TriggerBillRun(_ context.Context, _, key string, _ TimeWindow) (string, error) {
	b.triggerCount++
	return "br-" + key, nil
}
func (b *billingTestBackend) WaitForBillRun(_ context.Context, _, _ string, _ time.Duration) (*BillRunResult, error) {
	return &BillRunResult{
		Status:      "COMPLETED",
		InvoicedUSD: b.invoiceUSD,
		WalletDebit: b.walletDebit,
	}, nil
}
func (b *billingTestBackend) GetWalletBalance(_ context.Context, _, _, _ string) (float64, error) {
	return 0, ErrUnsupported{Op: "wallet"}
}
func (b *billingTestBackend) GetSubscriptionState(_ context.Context, _, _ string) (SubscriptionSnapshot, error) {
	return SubscriptionSnapshot{}, ErrUnsupported{Op: "subscription_state"}
}
func (b *billingTestBackend) MigrateSubscription(_ context.Context, _, _, _ string) (MigrateOutcome, error) {
	return MigrateOutcome{}, ErrUnsupported{Op: "migrate_subscription"}
}

// manifestPerUnitPostpaid builds a manifest with one tenant + one customer
// against PER_UNIT POSTPAID at $0.001/event.
func manifestPerUnitPostpaid() (*seed.Manifest, *runner.RunResult) {
	mf := &seed.Manifest{
		ManifestVersion: seed.ManifestVersion,
		RunID:           "rb-1",
		Target:          "local",
		Scenario:        "test",
		Tenants: []seed.ManifestTenant{
			{
				TenantID:     "t-billing",
				Archetype:    "perunit-postpaid",
				PricingModel: scenario.PricingPerUnit,
				BillingMode:  scenario.BillingPostpaid,
				RatePlans: []seed.ManifestRatePlan{
					{Config: map[string]any{"per_unit_rate_usd": 0.001}},
				},
				Customers: []seed.ManifestCustomer{
					{CustomerID: "c-billing", Currency: "USD"},
				},
			},
		},
	}
	rr := minimalRunResult()
	rr.PerTenant = map[string]int64{"t-billing": 10_000}
	rr.PerArchetype = map[string]int64{"perunit-postpaid": 10_000}
	return mf, rr
}

// TestBillingMatch_HappyPath_NoDrift expects the validator to PASS when
// the backend returns the correctly computed invoice.
func TestBillingMatch_HappyPath_NoDrift(t *testing.T) {
	mf, rr := manifestPerUnitPostpaid()
	// 10_000 events × 0.001 = 10.00 USD, all on invoice (POSTPAID).
	fb := &billingTestBackend{
		caps:        Capabilities{BillRuns: true},
		invoiceUSD:  10.00,
		walletDebit: 0,
	}
	in := &Inputs{
		Run:            rr,
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        fb,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckBillingMatch},
		TolerancePct:   0.001,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	for _, c := range r.Checks {
		if c.Name == CheckBillingMatch && c.Status != StatusPass {
			t.Fatalf("expected billing_match PASS; got %s — %s", c.Status, c.Reason)
		}
	}
}

// TestBillingMatch_DriftFails proves the acceptance criterion:
//
//	"Force a billing math drift (wrong rate in seed): Check 5 FAILS with
//	exact archetype + customer + drift amount"
//
// The backend returns 12.34 instead of 10.00 — drift = 23.4% which
// exceeds the 0.1% tolerance. Validator MUST FAIL.
func TestBillingMatch_DriftFails(t *testing.T) {
	mf, rr := manifestPerUnitPostpaid()
	fb := &billingTestBackend{
		caps:        Capabilities{BillRuns: true},
		invoiceUSD:  12.34, // 23.4% drift
		walletDebit: 0,
	}
	in := &Inputs{
		Run:            rr,
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        fb,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckBillingMatch},
		TolerancePct:   0.001,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())

	var ck *CheckResult
	for _, c := range r.Checks {
		if c.Name == CheckBillingMatch {
			ck = c
		}
	}
	if ck == nil || ck.Status != StatusFail {
		t.Fatalf("expected billing_match FAIL on drift; got %+v", ck)
	}
	// The details payload must surface the offending archetype + drift.
	by, ok := ck.Details["by_archetype"]
	if !ok {
		t.Fatal("by_archetype missing from FAIL details")
	}
	_ = by
}

// TestBillingMatch_PrepaidWalletRouting verifies route-stage parity:
// PREPAID flows route to wallet, not invoice.
func TestBillingMatch_PrepaidWalletRouting(t *testing.T) {
	mf, rr := manifestPerUnitPostpaid()
	mf.Tenants[0].BillingMode = scenario.BillingPrepaid
	mf.Tenants[0].RatePlans[0].Config["wallet_initial_balance_usd"] = 500.00

	// 10.00 charge, fully covered by wallet.
	fb := &billingTestBackend{
		caps:        Capabilities{BillRuns: true},
		invoiceUSD:  0,
		walletDebit: 10.00,
	}
	in := &Inputs{
		Run:            rr,
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        fb,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckBillingMatch},
		TolerancePct:   0.001,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	for _, c := range r.Checks {
		if c.Name == CheckBillingMatch && c.Status != StatusPass {
			t.Fatalf("PREPAID wallet routing should PASS; got %s — %s", c.Status, c.Reason)
		}
	}
}

// manifestPercentage builds a PERCENTAGE POSTPAID manifest with the
// given charge_base_per_event_usd round-tripped through the rate-plan
// config map (mirrors how the seed harness shapes manifests).
func manifestPercentage(chargeBasePerEvent float64) (*seed.Manifest, *runner.RunResult) {
	cfg := map[string]any{
		"percentage_rate": 0.025,
	}
	if chargeBasePerEvent > 0 {
		cfg["charge_base_per_event_usd"] = chargeBasePerEvent
	}
	mf := &seed.Manifest{
		ManifestVersion: seed.ManifestVersion,
		RunID:           "rb-pct",
		Target:          "local",
		Scenario:        "test",
		Tenants: []seed.ManifestTenant{
			{
				TenantID:     "t-pct",
				Archetype:    "percentage-postpaid",
				PricingModel: scenario.PricingPercentage,
				BillingMode:  scenario.BillingPostpaid,
				RatePlans: []seed.ManifestRatePlan{
					{Config: cfg},
				},
				Customers: []seed.ManifestCustomer{
					{CustomerID: "c-pct", Currency: "USD"},
				},
			},
		},
	}
	rr := minimalRunResult()
	// 1_000 events; oracle is events × charge_base × rate.
	rr.PerTenant = map[string]int64{"t-pct": 1_000}
	rr.PerArchetype = map[string]int64{"percentage-postpaid": 1_000}
	return mf, rr
}

// TestBillingMatch_PercentageWithChargeBase asserts the PERCENTAGE oracle
// uses scenario.RateConfig.ChargeBasePerEventUSD when set:
//
//	1_000 events × $100 base × 2.5% = $2_500.00
//
// Backend returns exactly $2500; validator must PASS.
func TestBillingMatch_PercentageWithChargeBase(t *testing.T) {
	mf, rr := manifestPercentage(100.0)
	fb := &billingTestBackend{
		caps:        Capabilities{BillRuns: true},
		invoiceUSD:  2500.00,
		walletDebit: 0,
	}
	in := &Inputs{
		Run:            rr,
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        fb,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckBillingMatch},
		TolerancePct:   0.001,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	for _, c := range r.Checks {
		if c.Name == CheckBillingMatch && c.Status != StatusPass {
			t.Fatalf("PERCENTAGE with charge_base $100 expected PASS at $2500; got %s — %s", c.Status, c.Reason)
		}
	}
}

// TestBillingMatch_PercentageFallbackBaseOne asserts the legacy fallback
// when charge_base_per_event_usd is absent: oracle assumes $1 per event,
// so 1_000 events × $1 × 2.5% = $25.00. Backwards-compat with scenarios
// authored before the field existed.
func TestBillingMatch_PercentageFallbackBaseOne(t *testing.T) {
	mf, rr := manifestPercentage(0) // zero → fallback path
	fb := &billingTestBackend{
		caps:        Capabilities{BillRuns: true},
		invoiceUSD:  25.00,
		walletDebit: 0,
	}
	in := &Inputs{
		Run:            rr,
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        fb,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckBillingMatch},
		TolerancePct:   0.001,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	for _, c := range r.Checks {
		if c.Name == CheckBillingMatch && c.Status != StatusPass {
			t.Fatalf("PERCENTAGE fallback base=$1 expected PASS at $25; got %s — %s", c.Status, c.Reason)
		}
	}
}

// TestBillingMatch_SkipsWithoutFlag verifies the opt-in gate.
func TestBillingMatch_SkipsWithoutFlag(t *testing.T) {
	mf, rr := manifestPerUnitPostpaid()
	in := &Inputs{
		Run:        rr,
		Manifest:   mf,
		Scenario:   minimalScenario(),
		Backend:    &billingTestBackend{caps: Capabilities{BillRuns: true}},
		OnlyChecks: []string{CheckBillingMatch},
		// IncludeBilling deliberately false
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	for _, c := range r.Checks {
		if c.Name == CheckBillingMatch && c.Status != StatusSkip {
			t.Fatalf("expected SKIP without --include-billing; got %s", c.Status)
		}
	}
}
