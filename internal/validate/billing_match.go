package validate

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate/billing"
)

// runBillingMatch is Check 5 — for each archetype × each customer:
//
//  1. Compute expected revenue from events × pricing-model formula.
//  2. Apply discount per customer.discount.
//  3. Apply tax per scenario.tax.
//  4. Trigger a bill run for the tenant (idempotent on archetype).
//  5. Wait for completion and compare invoice against expected.
//  6. PREPAID: confirm wallet debit equals invoice amount.
//  7. HYBRID:  confirm wallet portion + invoice portion = total.
//
// Opt-in via --include-billing because step 4-5 are slow (seconds per
// tenant). When disabled, the check is SKIPped with a clear reason.
func (v *Validator) runBillingMatch(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckBillingMatch)

	if !v.in.IncludeBilling {
		return res.Skip("--include-billing not set")
	}
	if !v.in.Backend.Capabilities().BillRuns {
		return res.Skip("backend cannot trigger bill runs (offline mode or capability missing)")
	}

	tolerance := v.in.TolerancePct
	if v.in.Scenario.Assertions.InvoiceRevenueDriftPctMax > 0 {
		// scenario assertion is the ceiling — a stricter --tolerance-pct
		// is honored even when the scenario allows more drift.
		ceiling := v.in.Scenario.Assertions.InvoiceRevenueDriftPctMax
		if tolerance > ceiling {
			tolerance = ceiling
		}
	}

	// Group customers by archetype so we can report per-archetype roll-ups.
	byArchetype := groupByArchetype(v.in.Manifest)

	type custOutcome struct {
		CustomerID  string  `json:"customer_id"`
		ExpectedUSD float64 `json:"expected_usd"`
		ActualUSD   float64 `json:"actual_usd"`
		WalletUSD   float64 `json:"wallet_usd"`
		InvoiceUSD  float64 `json:"invoice_usd"`
		DriftPct    float64 `json:"drift_pct"`
		Match       bool    `json:"match"`
		Reason      string  `json:"reason,omitempty"`
	}
	type archOutcome struct {
		Archetype       string        `json:"archetype"`
		PricingModel    string        `json:"pricing_model"`
		BillingMode     string        `json:"billing_mode"`
		TenantsChecked  int           `json:"tenants_checked"`
		CustomersTested int           `json:"customers_tested"`
		AllMatch        bool          `json:"all_match"`
		MaxDriftPct     float64       `json:"max_drift_pct"`
		Customers       []custOutcome `json:"customers,omitempty"`
	}

	var allArch []archOutcome
	overallPass := true

	// Stable iteration order — manifest summary's by_archetype keys sorted.
	archNames := make([]string, 0, len(byArchetype))
	for name := range byArchetype {
		archNames = append(archNames, name)
	}
	sort.Strings(archNames)

	for _, name := range archNames {
		if !v.ArchetypeMatches(name) {
			continue
		}
		bucket := byArchetype[name]
		out := archOutcome{
			Archetype:    name,
			PricingModel: string(bucket.pricingModel),
			BillingMode:  string(bucket.billingMode),
			AllMatch:     true,
		}

		eventsPerCust := eventsPerCustomerEstimate(v.in.Run, bucket)

		for _, cust := range bucket.customers {
			expected, err := computeExpected(bucket, cust, eventsPerCust[cust.CustomerID], v.in.Scenario)
			if err != nil {
				out.Customers = append(out.Customers, custOutcome{
					CustomerID: cust.CustomerID,
					Reason:     fmt.Sprintf("compute expected: %v", err),
				})
				out.AllMatch = false
				continue
			}

			runResult, err := v.runOneBillRun(ctx, bucket.tenantID, name, cust.CustomerID)
			if err != nil {
				out.Customers = append(out.Customers, custOutcome{
					CustomerID:  cust.CustomerID,
					ExpectedUSD: expected.Total,
					Reason:      fmt.Sprintf("bill run: %v", err),
				})
				out.AllMatch = false
				continue
			}

			actualTotal := runResult.InvoicedUSD + runResult.WalletDebit
			drift := relativeDrift(expected.Total, actualTotal)
			match := drift <= tolerance &&
				modeMatches(bucket.billingMode, expected, runResult)

			oc := custOutcome{
				CustomerID:  cust.CustomerID,
				ExpectedUSD: expected.Total,
				ActualUSD:   actualTotal,
				WalletUSD:   runResult.WalletDebit,
				InvoiceUSD:  runResult.InvoicedUSD,
				DriftPct:    drift,
				Match:       match,
			}
			if !match {
				oc.Reason = fmt.Sprintf("drift %.6f > tolerance %.6f", drift, tolerance)
				out.AllMatch = false
				overallPass = false
			}
			if drift > out.MaxDriftPct {
				out.MaxDriftPct = drift
			}
			out.CustomersTested++
			out.Customers = append(out.Customers, oc)
		}

		out.TenantsChecked = 1
		allArch = append(allArch, out)
	}

	res.
		Set("archetypes_checked", len(allArch)).
		Set("tolerance_pct", tolerance).
		Set("by_archetype", allArch)

	if !overallPass {
		return res.Fail("one or more archetypes failed billing match — see by_archetype")
	}
	return res.Pass()
}

// archBucket groups all customers + tenant info for one archetype.
type archBucket struct {
	tenantID     string
	pricingModel scenario.PricingModel
	billingMode  scenario.BillingMode
	rate         scenario.RateConfig
	customers    []seed.ManifestCustomer
}

// groupByArchetype returns the manifest's archetype → bucket index.
//
// matrix-billing has multiple tenants per archetype (one per archetype
// definition + customer_count fan-out); this collapses them so the report
// summarizes "per archetype" not "per tenant per archetype".
func groupByArchetype(m *seed.Manifest) map[string]*archBucket {
	out := map[string]*archBucket{}
	for _, t := range m.Tenants {
		b, ok := out[t.Archetype]
		if !ok {
			b = &archBucket{
				tenantID:     t.TenantID,
				pricingModel: t.PricingModel,
				billingMode:  t.BillingMode,
				rate:         findRateConfig(t),
			}
			out[t.Archetype] = b
		}
		b.customers = append(b.customers, t.Customers...)
	}
	return out
}

// findRateConfig pulls the RateConfig from the first rate plan we find for
// the tenant. The seed harness writes the same config to every plan inside
// an archetype, so any of them is fine.
func findRateConfig(t seed.ManifestTenant) scenario.RateConfig {
	for _, rp := range t.RatePlans {
		// rp.Config is a map[string]any — the seed harness produced it from
		// the scenario.RateConfig and json-encoded numerics.
		return rateConfigFromMap(rp.Config)
	}
	return scenario.RateConfig{}
}

func rateConfigFromMap(m map[string]any) scenario.RateConfig {
	r := scenario.RateConfig{}
	r.FlatFeeUSD = floatField(m, "flat_fee_usd")
	r.PerUnitRateUSD = floatField(m, "per_unit_rate_usd")
	r.PercentageRate = floatField(m, "percentage_rate")
	r.ChargeBasePerEventUSD = floatField(m, "charge_base_per_event_usd")
	r.MinFeeUSD = floatField(m, "min_fee_usd")
	r.IncludedFreeUnits = int64Field(m, "included_free_units")
	r.BlockSizeUnits = int64Field(m, "block_size_units")
	r.WalletInitialBalanceUSD = floatField(m, "wallet_initial_balance_usd")
	if v, ok := m["graduated_tiers"]; ok {
		r.GraduatedTiers = tiersFromAny(v)
	}
	if v, ok := m["volume_tiers"]; ok {
		r.VolumeTiers = tiersFromAny(v)
	}
	return r
}

func tiersFromAny(v any) []scenario.TierBand {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]scenario.TierBand, 0, len(arr))
	for _, item := range arr {
		m, _ := item.(map[string]any)
		out = append(out, scenario.TierBand{
			UpToUnits:    int64Field(m, "up_to_units"),
			UnitPriceUSD: floatField(m, "unit_price_usd"),
			FlatFeeUSD:   floatField(m, "flat_fee_usd"),
		})
	}
	return out
}

func floatField(m map[string]any, key string) float64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return x
		case float32:
			return float64(x)
		case int:
			return float64(x)
		case int64:
			return float64(x)
		}
	}
	return 0
}

func int64Field(m map[string]any, key string) int64 {
	if v, ok := m[key]; ok {
		switch x := v.(type) {
		case float64:
			return int64(x)
		case int:
			return int64(x)
		case int64:
			return x
		}
	}
	return 0
}

// eventsPerCustomerEstimate splits the tenant's run-level event count
// evenly across its customers. The matrix-billing scenarios write ACTIVE
// state to every customer in an archetype, so even split is the right
// approximation. Zero-event customers are zeros.
//
// When per-customer telemetry lands (Session 6+), this becomes exact.
func eventsPerCustomerEstimate(run *runner.RunResult, bucket *archBucket) map[string]int64 {
	out := map[string]int64{}
	if len(bucket.customers) == 0 {
		return out
	}
	tenantTotal := int64(0)
	if run != nil && run.PerTenant != nil {
		tenantTotal = run.PerTenant[bucket.tenantID]
	}
	per := tenantTotal / int64(len(bucket.customers))
	for _, c := range bucket.customers {
		out[c.CustomerID] = per
	}
	return out
}

// computeExpected runs the validator's billing model for one customer.
func computeExpected(bucket *archBucket, cust seed.ManifestCustomer, events int64, sc *scenario.Scenario) (billing.CalcResult, error) {
	tiers := convertTiers(bucket.rate)
	var graduated, volume []billing.Tier
	if bucket.pricingModel == scenario.PricingGraduated {
		graduated = tiers
	}
	if bucket.pricingModel == scenario.PricingVolumeTiered {
		volume = tiers
	}

	// PERCENTAGE archetypes bill (events × charge_base × rate). Pull the
	// per-event charge base from the scenario when set, fall back to 1.0
	// (which reduces PERCENTAGE to events × rate) so older scenarios that
	// don't author the field continue to behave as before.
	chargeBasePerEvent := bucket.rate.ChargeBasePerEventUSD
	if chargeBasePerEvent <= 0 {
		chargeBasePerEvent = 1.0
	}
	in := billing.CalcInputs{
		Events:        events,
		ChargeBaseUSD: float64(events) * chargeBasePerEvent,
		Model:         billing.PricingModel(bucket.pricingModel),
		Mode:          billing.Mode(bucket.billingMode),
		Rate: billing.RateConfig{
			FlatFeeUSD:        bucket.rate.FlatFeeUSD,
			PerUnitRateUSD:    bucket.rate.PerUnitRateUSD,
			PercentageRate:    bucket.rate.PercentageRate,
			MinFeeUSD:         bucket.rate.MinFeeUSD,
			IncludedFreeUnits: bucket.rate.IncludedFreeUnits,
			BlockSizeUnits:    bucket.rate.BlockSizeUnits,
			GraduatedTiers:    graduated,
			VolumeTiers:       volume,
		},
		Discount: convertDiscount(cust.Discount),
		TaxPct:   resolveTaxPct(sc, cust),
	}

	walletAvailable := bucket.rate.WalletInitialBalanceUSD
	return billing.Calculate(in, walletAvailable)
}

func convertTiers(r scenario.RateConfig) []billing.Tier {
	src := r.GraduatedTiers
	if len(src) == 0 {
		src = r.VolumeTiers
	}
	out := make([]billing.Tier, 0, len(src))
	for _, t := range src {
		upTo := t.UpToUnits
		if upTo <= 0 {
			upTo = math.MaxInt64
		}
		out = append(out, billing.Tier{
			UpToUnits: upTo,
			UnitPrice: t.UnitPriceUSD,
			FlatFee:   t.FlatFeeUSD,
		})
	}
	return out
}

func convertDiscount(d *seed.ManifestDiscount) *billing.Discount {
	if d == nil {
		return nil
	}
	return &billing.Discount{Type: d.Type, Value: d.Value}
}

func resolveTaxPct(sc *scenario.Scenario, cust seed.ManifestCustomer) float64 {
	if sc.Tax.Engine == "" || sc.Tax.Engine == scenario.TaxMock {
		return 0
	}
	if sc.Tax.Jurisdictions == nil {
		return 0
	}
	// Use customer.currency as a coarse jurisdiction key — replace with a
	// real country/region key once seed.ManifestCustomer carries country.
	if pct, ok := sc.Tax.Jurisdictions[cust.Currency]; ok {
		return pct
	}
	return 0
}

// runOneBillRun calls the backend, waits for completion, returns the result.
func (v *Validator) runOneBillRun(ctx context.Context, tenantID, archetype, customerID string) (*BillRunResult, error) {
	idemKey := fmt.Sprintf("validate-%s-%s-%s", v.in.Run.RunID, archetype, customerID)
	billRunID, err := v.in.Backend.TriggerBillRun(ctx, tenantID, idemKey, v.runWindow())
	if err != nil {
		return nil, err
	}
	return v.in.Backend.WaitForBillRun(ctx, tenantID, billRunID, 5*time.Minute)
}

// modeMatches enforces the routing invariant per billing mode.
func modeMatches(mode scenario.BillingMode, expected billing.CalcResult, actual *BillRunResult) bool {
	const eps = 0.01
	switch mode {
	case scenario.BillingPostpaid:
		return abs(actual.WalletDebit) < eps && abs(actual.InvoicedUSD-expected.Total) < eps
	case scenario.BillingPrepaid:
		return abs(actual.WalletDebit-expected.Total) < eps && abs(actual.InvoicedUSD) < eps
	case scenario.BillingHybrid:
		return abs((actual.WalletDebit+actual.InvoicedUSD)-expected.Total) < eps
	default:
		return false
	}
}

func relativeDrift(expected, actual float64) float64 {
	if expected == 0 {
		if actual == 0 {
			return 0
		}
		return math.Inf(1)
	}
	return math.Abs(expected-actual) / math.Abs(expected)
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
