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
//  1. Compute expected revenue: each metric of the customer's plan priced
//     with its own config from the run's per-metric usage, summed
//     (expectedFor); run results without per-metric usage fall back to
//     the customer's estimated event count × the plan-level formula.
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
		CustomerID string `json:"customer_id"`
		// PricingModel / BillingMode are the customer's own rate card
		// (multi-card archetypes differ per customer).
		PricingModel string  `json:"pricing_model,omitempty"`
		BillingMode  string  `json:"billing_mode,omitempty"`
		ExpectedUSD  float64 `json:"expected_usd"`
		ActualUSD    float64 `json:"actual_usd"`
		WalletUSD    float64 `json:"wallet_usd"`
		InvoiceUSD   float64 `json:"invoice_usd"`
		DriftPct     float64 `json:"drift_pct"`
		Match        bool    `json:"match"`
		Reason       string  `json:"reason,omitempty"`
		// Method is "per_metric" when the expectation was priced metric by
		// metric from the run's per-metric usage; empty for the event-count
		// estimate (run results without per_tenant_metric).
		Method  string         `json:"method,omitempty"`
		Metrics []metricExpect `json:"metrics,omitempty"`
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
			expected, lines, err := expectedFor(bucket, cust, eventsPerCust[cust.CustomerID], v.in.Run, v.in.Scenario)
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

			plan := bucket.planFor(cust)
			actualTotal := runResult.InvoicedUSD + runResult.WalletDebit
			drift := relativeDrift(expected.Total, actualTotal)
			match := drift <= tolerance &&
				modeMatches(plan.billingMode, expected, runResult)

			oc := custOutcome{
				CustomerID:   cust.CustomerID,
				PricingModel: string(plan.pricingModel),
				BillingMode:  string(plan.billingMode),
				ExpectedUSD:  expected.Total,
				ActualUSD:    actualTotal,
				WalletUSD:    runResult.WalletDebit,
				InvoiceUSD:   runResult.InvoicedUSD,
				DriftPct:     drift,
				Match:        match,
			}
			if lines != nil {
				oc.Method = "per_metric"
				oc.Metrics = lines
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
	// archetype is the scenario archetype name the bucket was built for.
	archetype string
	// productType is the archetype's first product type — the outcome mix
	// the OUTCOME_BASED estimate splits events by, unless the customer's
	// rate card names its own product_filter (see outcomeProductType).
	productType scenario.ProductType
	// tenants indexes each tenant's rate-plan metrics and traffic shares;
	// customerTenant maps a customer to its tenant. Both feed the
	// per-metric expectation (perMetricInputs).
	tenants        map[string]*bucketTenant
	customerTenant map[string]string
	// plans / offerings index every rate plan and offering of the
	// archetype's tenants so planFor can price each customer with the
	// card their subscription is on (a multi-card archetype has one rate
	// plan per card, possibly with different models and billing modes).
	plans     map[string]bucketPlan
	offerings map[string]seed.ManifestOffering
}

// bucketPlan is one rate plan's model + config, parsed from the manifest.
type bucketPlan struct {
	name         string
	pricingModel scenario.PricingModel
	rate         scenario.RateConfig
}

// bucketTenant is one tenant's view for the per-metric expectation.
type bucketTenant struct {
	// metrics are the billable units every rate plan of the tenant prices
	// (seed.RatePlanMetrics).
	metrics []seed.ManifestMetric
	// weights is each customer's share of the tenant's traffic: the number
	// of usable keys (non-revoked, on a non-stale subscription) — the
	// generator picks uniformly among those. totalWeight is their sum.
	weights     map[string]int
	totalWeight int
}

// customerPlan is the pricing a customer is billed under.
type customerPlan struct {
	// name is the manifest rate plan's name; empty when the plan was not
	// resolved through an offering (archetype fallback).
	name         string
	pricingModel scenario.PricingModel
	billingMode  scenario.BillingMode
	rate         scenario.RateConfig
}

// planFor resolves the rate plan a customer is billed under by following
// customer → subscription → offering → rate plan (ManifestOffering.RatePlanID).
// The first subscription whose offering and plan are both known wins. When
// nothing resolves — manifests written before offerings carried their rate
// plan id, or hand-built buckets — it falls back to the archetype's first
// rate plan, which is exact for single-card archetypes.
func (b *archBucket) planFor(cust seed.ManifestCustomer) customerPlan {
	for _, sub := range cust.Subscriptions {
		off, ok := b.offerings[sub.OfferingID]
		if !ok || off.RatePlanID == "" {
			continue
		}
		p, ok := b.plans[off.RatePlanID]
		if !ok {
			continue
		}
		mode := off.BillingMode
		if mode == "" {
			mode = b.billingMode
		}
		model := p.pricingModel
		if model == "" {
			model = b.pricingModel
		}
		return customerPlan{name: p.name, pricingModel: model, billingMode: mode, rate: p.rate}
	}
	return customerPlan{pricingModel: b.pricingModel, billingMode: b.billingMode, rate: b.rate}
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
				archetype:    t.Archetype,
				tenantID:     t.TenantID,
				pricingModel: t.PricingModel,
				billingMode:  t.BillingMode,
				rate:         findRateConfig(t),
			}
			if len(t.Products) > 0 {
				b.productType = t.Products[0].ProductType
			}
			b.plans = map[string]bucketPlan{}
			b.offerings = map[string]seed.ManifestOffering{}
			b.tenants = map[string]*bucketTenant{}
			b.customerTenant = map[string]string{}
			out[t.Archetype] = b
		}
		for _, rp := range t.RatePlans {
			model, _ := rp.Config["pricing_model"].(string)
			b.plans[rp.RatePlanID] = bucketPlan{
				name:         rp.Name,
				pricingModel: scenario.PricingModel(model),
				rate:         rateConfigFromMap(rp.Config),
			}
		}
		for _, off := range t.Offerings {
			b.offerings[off.OfferingID] = off
		}
		b.customers = append(b.customers, t.Customers...)

		bt := &bucketTenant{metrics: seed.RatePlanMetrics(t.Products), weights: map[string]int{}}
		for _, c := range t.Customers {
			w := usableKeys(c)
			bt.weights[c.CustomerID] = w
			bt.totalWeight += w
			b.customerTenant[c.CustomerID] = t.TenantID
		}
		b.tenants[t.TenantID] = bt
	}
	return out
}

// usableKeys counts the keys the generator can send a customer's traffic
// with: non-revoked keys on non-stale subscriptions (generator.indexTenants).
func usableKeys(c seed.ManifestCustomer) int {
	n := 0
	for _, sub := range c.Subscriptions {
		if sub.Stale {
			continue
		}
		for _, k := range sub.APIKeys {
			if !k.Revoked {
				n++
			}
		}
	}
	return n
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
	if v, ok := m["outcome_weights"].(map[string]any); ok {
		r.OutcomeWeights = make(map[string]float64, len(v))
		for k := range v {
			r.OutcomeWeights[k] = floatField(v, k)
		}
	}
	// The manifest config holds []map[string]any in memory (straight from
	// the seeder) and []any after a JSON round-trip — accept both.
	var bandMaps []map[string]any
	switch v := m["staircase_bands"].(type) {
	case []map[string]any:
		bandMaps = v
	case []any:
		for _, item := range v {
			bm, _ := item.(map[string]any)
			bandMaps = append(bandMaps, bm)
		}
	}
	for _, bm := range bandMaps {
		r.StaircaseBands = append(r.StaircaseBands, scenario.StaircaseBand{
			FromUnits: int64Field(bm, "from_units"),
			ToUnits:   int64Field(bm, "to_units"),
			PriceUSD:  floatField(bm, "price_usd"),
		})
	}
	return r
}

// tiersFromAny reads graduated/volume bands from a manifest rate-plan config.
// The seeder's in-memory manifest holds []map[string]any (rateConfigSummary
// → tierBandsAsMaps) and a JSON round-trip yields []any — accept both. It
// used to accept only []any, so a validator fed the orchestrator's
// in-memory manifest saw no tiers and expected $0 for every GRADUATED /
// VOLUME_TIERED customer.
func tiersFromAny(v any) []scenario.TierBand {
	var maps []map[string]any
	switch arr := v.(type) {
	case []map[string]any:
		maps = arr
	case []any:
		for _, item := range arr {
			m, _ := item.(map[string]any)
			maps = append(maps, m)
		}
	default:
		return nil
	}
	out := make([]scenario.TierBand, 0, len(maps))
	for _, m := range maps {
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
//
// The customer is priced with their own rate card (bucket.planFor), so a
// multi-card archetype — e.g. OUTCOME_BASED + STAIRCASE cards on one
// product — is estimated per card rather than as if every customer were on
// the first one.
func computeExpected(bucket *archBucket, cust seed.ManifestCustomer, events int64, sc *scenario.Scenario) (billing.CalcResult, error) {
	plan := bucket.planFor(cust)
	rc := plan.rate
	// PERCENTAGE archetypes bill (events × charge_base × rate). Pull the
	// per-event charge base from the scenario when set, fall back to 1.0
	// (which reduces PERCENTAGE to events × rate) so older scenarios that
	// don't author the field continue to behave as before.
	chargeBasePerEvent := rc.ChargeBasePerEventUSD
	if chargeBasePerEvent <= 0 {
		chargeBasePerEvent = 1.0
	}
	in := billing.CalcInputs{
		Events:        events,
		ChargeBaseUSD: float64(events) * chargeBasePerEvent,
		Model:         billing.PricingModel(plan.pricingModel),
		Mode:          billing.Mode(plan.billingMode),
		Rate:          billingRate(plan.pricingModel, rc),
		Discount:      convertDiscount(cust.Discount),
		TaxPct:        resolveTaxPct(sc, cust),
	}
	if plan.pricingModel == scenario.PricingOutcomeBased {
		// Only an event count is known per customer, so split it by the
		// outcome mix the generator emits for this product type and weight
		// it as a COUNT unit (one unit per event) — billing's
		// OutcomeWeighting on the expected per-status counts.
		mix, _ := sc.EffectiveOutcomeMix(bucket.outcomeProductType(plan, sc))
		in.Outcomes = billing.ExpectedOutcomes(events, mix)
		in.Aggregation = "COUNT"
	}

	walletAvailable := rc.WalletInitialBalanceUSD
	return billing.Calculate(in, walletAvailable)
}

// billingRate converts a scenario rate config into the billing model's,
// keeping only the tier list the pricing model reads.
func billingRate(model scenario.PricingModel, rc scenario.RateConfig) billing.RateConfig {
	tiers := convertTiers(rc)
	var graduated, volume []billing.Tier
	if model == scenario.PricingGraduated {
		graduated = tiers
	}
	if model == scenario.PricingVolumeTiered {
		volume = tiers
	}
	return billing.RateConfig{
		FlatFeeUSD:        rc.FlatFeeUSD,
		PerUnitRateUSD:    rc.PerUnitRateUSD,
		PercentageRate:    rc.PercentageRate,
		MinFeeUSD:         rc.MinFeeUSD,
		IncludedFreeUnits: rc.IncludedFreeUnits,
		BlockSizeUnits:    rc.BlockSizeUnits,
		GraduatedTiers:    graduated,
		VolumeTiers:       volume,
		OutcomeWeights:    rc.OutcomeWeights,
		StaircaseBands:    convertStaircase(rc.StaircaseBands),
	}
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

func convertStaircase(bands []scenario.StaircaseBand) []billing.Band {
	if len(bands) == 0 {
		return nil
	}
	out := make([]billing.Band, len(bands))
	for i, b := range bands {
		out[i] = billing.Band{From: b.FromUnits, To: b.ToUnits, Price: b.PriceUSD}
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
