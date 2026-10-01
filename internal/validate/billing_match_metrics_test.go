package validate

import (
	"context"
	"math"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

func activeCustomer(id, offeringID string) seed.ManifestCustomer {
	return seed.ManifestCustomer{CustomerID: id, Subscriptions: []seed.ManifestSubscription{{
		OfferingID: offeringID,
		APIKeys:    []seed.ManifestAPIKey{{KeyID: id + "-k"}},
	}}}
}

// multiMetricFixture is one tenant on a single-card plan whose metrics each
// carry different pricing: allowance, min fee, graduated tiers, staircase
// bands and a block-priced quota.
func multiMetricFixture() (*seed.Manifest, *scenario.Scenario, *runner.RunResult) {
	sc := &scenario.Scenario{Tenants: scenario.Tenants{Archetypes: []scenario.TenantArchetype{{
		Name:         "arch",
		ProductTypes: []scenario.ProductType{scenario.ProductAPI},
		PricingModel: scenario.PricingPerUnit,
		BillingMode:  scenario.BillingPostpaid,
		RateCards: []scenario.RateCardSpec{{
			Name:         "pro",
			PricingModel: scenario.PricingPerUnit,
			RateConfig:   scenario.RateConfig{PerUnitRateUSD: 0.01, IncludedFreeUnits: 100},
			MetricConfigs: map[string]scenario.MetricOverride{
				"Webhooks": {PricingModel: scenario.PricingPerUnit, PerUnitRateUSD: 0.001, MinFeeUSD: 3},
				"tokens": {PricingModel: scenario.PricingGraduated, GraduatedTiers: []scenario.TierBand{
					{UpToUnits: 1000, UnitPriceUSD: 0.002}, {UnitPriceUSD: 0.001}}},
				"Sessions": {PricingModel: scenario.PricingStaircase, IncludedFreeUnits: 5, StaircaseBands: []scenario.StaircaseBand{
					{FromUnits: 1, ToUnits: 10, PriceUSD: 20}, {FromUnits: 11, PriceUSD: 50}}},
				"Data Transfer": {PricingModel: scenario.PricingIncludedQuota, PerUnitRateUSD: 0.05,
					IncludedFreeUnits: 500, BlockSizeUnits: 100},
			},
		}},
	}}}}
	stale := seed.ManifestCustomer{CustomerID: "c-stale", Subscriptions: []seed.ManifestSubscription{{
		OfferingID: "off", Stale: true, APIKeys: []seed.ManifestAPIKey{{KeyID: "k-stale", Revoked: true}}}}}
	discounted := activeCustomer("c-disc", "off")
	discounted.Discount = &seed.ManifestDiscount{Type: "PERCENTAGE", Value: 0.10}
	m := &seed.Manifest{Tenants: []seed.ManifestTenant{{
		TenantID: "t1", Archetype: "arch",
		PricingModel: scenario.PricingPerUnit, BillingMode: scenario.BillingPostpaid,
		Products: []seed.ManifestProduct{{ProductType: scenario.ProductAPI, Metrics: []seed.ManifestMetric{
			{ID: "m1", Name: "API Calls", AggregationType: "COUNT"},
			{ID: "m2", Name: "Webhooks", AggregationType: "COUNT"},
			{ID: "m3", Name: "Tokens", AggregationType: "SUM"},
			{ID: "m4", Name: "Sessions", AggregationType: "COUNT"},
			{ID: "m5", Name: "Data Transfer", AggregationType: "SUM"},
			{ID: "m6", Name: "Unused", AggregationType: "COUNT"},
		}}},
		RatePlans: []seed.ManifestRatePlan{{RatePlanID: "rp", Name: seed.CardRatePlanName("arch", "pro"),
			Config: map[string]any{"pricing_model": "PER_UNIT", "per_unit_rate_usd": 0.01, "included_free_units": int64(100)}}},
		Offerings: []seed.ManifestOffering{{OfferingID: "off", RatePlanID: "rp", BillingMode: scenario.BillingPostpaid}},
		Customers: []seed.ManifestCustomer{activeCustomer("c-full", "off"), discounted, stale},
	}}}
	// Tenant totals; the two active customers each get half.
	run := &runner.RunResult{
		PerTenant: map[string]int64{"t1": 1310},
		PerTenantMetric: map[string]map[string]runner.MetricUsage{"t1": {
			"API Calls":     {Events: 1000, Quantity: 1000},
			"Webhooks":      {Events: 200, Quantity: 200},
			"Tokens":        {Events: 40, Quantity: 5000},
			"Sessions":      {Events: 40, Quantity: 40},
			"Data Transfer": {Events: 30, Quantity: 1500},
		}},
	}
	return m, sc, run
}

// TestExpectedFor_MultiMetricPlan: every metric is priced with its own
// allowance / min fee / tiers / bands and the lines are summed.
//
//	API Calls      500 − 100 free = 400 × $0.01            =  $4.00
//	Webhooks       100 × $0.001 = $0.10 → min fee           =  $3.00
//	Tokens         2500: 1000 × $0.002 + 1500 × $0.001      =  $3.50
//	Sessions       20 − 5 free = 15 → band [11, ∞)          = $50.00
//	Data Transfer  750 − 500 free = 250 → 3 × 100 × $0.05   = $15.00
//	Unused         no usage → no line (no allowance, no fee) =  $0.00
//	                                                    total $75.50
func TestExpectedFor_MultiMetricPlan(t *testing.T) {
	m, sc, run := multiMetricFixture()
	bucket := groupByArchetype(m)["arch"]

	res, lines, err := expectedFor(bucket, bucket.customers[0], 436, run, sc)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"API Calls": 4.00, "Webhooks": 3.00, "Tokens": 3.50, "Sessions": 50.00, "Data Transfer": 15.00, "Unused": 0}
	if len(lines) != len(want) {
		t.Fatalf("got %d metric lines, want %d: %+v", len(lines), len(want), lines)
	}
	for _, l := range lines {
		if math.Abs(l.ChargeUSD-want[l.Metric]) > 1e-9 {
			t.Errorf("%s (%s, %v units) = $%v; want $%v", l.Metric, l.PricingModel, l.Units, l.ChargeUSD, want[l.Metric])
		}
	}
	if math.Abs(res.Subtotal-75.50) > 1e-9 || math.Abs(res.Total-75.50) > 1e-9 || res.InvoiceUSD != res.Total {
		t.Errorf("result = %+v; want $75.50 invoiced", res)
	}

	// Same usage, 10% discount on the summed subtotal: $75.50 − $7.55 = $67.95.
	disc, _, err := expectedFor(bucket, bucket.customers[1], 436, run, sc)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(disc.Subtotal-75.50) > 1e-9 || math.Abs(disc.Total-67.95) > 1e-9 {
		t.Errorf("discounted = %+v; want subtotal 75.50 total 67.95", disc)
	}

	// A customer with no usable key gets no traffic, so nothing is billed.
	none, noneLines, err := expectedFor(bucket, bucket.customers[2], 436, run, sc)
	if err != nil {
		t.Fatal(err)
	}
	if none.Total != 0 || noneLines == nil {
		t.Errorf("stale customer = %+v (lines %v); want $0 priced per metric", none, noneLines)
	}

	// The single-metric estimate of the same customer is a different number
	// (436 events − 100 free = 336 × $0.01 = $3.36) — what billing_match
	// used to expect for this plan.
	legacy, err := computeExpected(bucket, bucket.customers[0], 436, sc)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(legacy.Total-3.36) > 1e-9 {
		t.Errorf("legacy estimate = %v; want 3.36", legacy.Total)
	}
}

// TestExpectedFor_FallsBackWithoutPerMetricUsage: a run result written
// before per_tenant_metric existed — or a manifest whose archetype the
// scenario does not define — is estimated exactly the old way.
func TestExpectedFor_FallsBackWithoutPerMetricUsage(t *testing.T) {
	m, sc, run := multiMetricFixture()
	bucket := groupByArchetype(m)["arch"]
	cust := bucket.customers[0]
	legacy, err := computeExpected(bucket, cust, 436, sc)
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		run *runner.RunResult
		sc  *scenario.Scenario
	}{
		"old run.json":              {&runner.RunResult{PerTenant: map[string]int64{"t1": 1310}}, sc},
		"nil run":                   {nil, sc},
		"archetype not in scenario": {run, &scenario.Scenario{}},
		"plan matches none of 2 cards": {run, &scenario.Scenario{Tenants: scenario.Tenants{Archetypes: []scenario.TenantArchetype{{
			Name: "arch", RateCards: []scenario.RateCardSpec{{Name: "a"}, {Name: "b"}}}}}}},
	}
	for name, tc := range cases {
		res, lines, err := expectedFor(bucket, cust, 436, tc.run, tc.sc)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if lines != nil || res.Total != legacy.Total || res.Subtotal != legacy.Subtotal {
			t.Errorf("%s: got %+v (lines %v); want the single-metric estimate %+v", name, res, lines, legacy)
		}
	}

	// A manifest without metric names (written before metrics carried them)
	// also falls back.
	m.Tenants[0].Products[0].Metrics = nil
	bare := groupByArchetype(m)["arch"]
	if _, lines, _ := expectedFor(bare, cust, 436, run, sc); lines != nil {
		t.Errorf("manifest without metrics must fall back, got lines %+v", lines)
	}
}

// outcomeFixture is an API-first archetype with two OUTCOME_BASED cards:
// "api" prices everything, "agents" is bound to the AI_AGENT product.
func outcomeFixture() (*seed.Manifest, *scenario.Scenario) {
	rate := scenario.RateConfig{PerUnitRateUSD: 0.01, OutcomeWeights: map[string]float64{"SUCCESS": 1, "ERROR": 0}}
	sc := &scenario.Scenario{
		OutcomeMixByProduct: map[scenario.ProductType]map[string]float64{
			scenario.ProductAPI:     {"SUCCESS": 0.5, "ERROR": 0.5},
			scenario.ProductAIAgent: {"SUCCESS": 0.9, "ERROR": 0.1},
		},
		Tenants: scenario.Tenants{Archetypes: []scenario.TenantArchetype{{
			Name:         "mixed",
			ProductTypes: []scenario.ProductType{scenario.ProductAPI, scenario.ProductAIAgent},
			RateCards: []scenario.RateCardSpec{
				{Name: "api", PricingModel: scenario.PricingOutcomeBased, RateConfig: rate},
				{Name: "agents", PricingModel: scenario.PricingOutcomeBased, RateConfig: rate,
					ProductFilter: []scenario.ProductType{scenario.ProductAIAgent}},
			},
		}}},
	}
	cfg := map[string]any{"pricing_model": "OUTCOME_BASED", "per_unit_rate_usd": 0.01,
		"outcome_weights": map[string]any{"SUCCESS": 1.0, "ERROR": 0.0}}
	m := &seed.Manifest{Tenants: []seed.ManifestTenant{{
		TenantID: "t1", Archetype: "mixed",
		PricingModel: scenario.PricingOutcomeBased, BillingMode: scenario.BillingPostpaid,
		Products: []seed.ManifestProduct{
			{ProductType: scenario.ProductAPI, Metrics: []seed.ManifestMetric{{ID: "m1", Name: "API Calls", AggregationType: "COUNT"}}},
			{ProductType: scenario.ProductAIAgent, Metrics: []seed.ManifestMetric{{ID: "m2", Name: "Agent Runs", AggregationType: "COUNT"}}},
		},
		RatePlans: []seed.ManifestRatePlan{
			{RatePlanID: "rp-api", Name: seed.CardRatePlanName("mixed", "api"), Config: cfg},
			{RatePlanID: "rp-agents", Name: seed.CardRatePlanName("mixed", "agents"), Config: cfg},
		},
		Offerings: []seed.ManifestOffering{
			{OfferingID: "off-api", RatePlanID: "rp-api", BillingMode: scenario.BillingPostpaid},
			{OfferingID: "off-agents", RatePlanID: "rp-agents", BillingMode: scenario.BillingPostpaid},
		},
		Customers: []seed.ManifestCustomer{activeCustomer("c-api", "off-api"), activeCustomer("c-agents", "off-agents")},
	}}}
	return m, sc
}

// TestOutcomeMix_UsesCardProductFilter: the card bound to AI_AGENT products
// is estimated with the AI_AGENT outcome mix, not the mix of the
// archetype's first product type (API).
func TestOutcomeMix_UsesCardProductFilter(t *testing.T) {
	m, sc := outcomeFixture()
	bucket := groupByArchetype(m)["mixed"]
	if bucket.productType != scenario.ProductAPI {
		t.Fatalf("fixture: archetype first product type = %s", bucket.productType)
	}
	apiCust, agentCust := bucket.customers[0], bucket.customers[1]

	if pt := bucket.outcomeProductType(bucket.planFor(agentCust), sc); pt != scenario.ProductAIAgent {
		t.Errorf("agents card product type = %s; want AI_AGENT", pt)
	}
	if pt := bucket.outcomeProductType(bucket.planFor(apiCust), sc); pt != scenario.ProductAPI {
		t.Errorf("api card (no filter) product type = %s; want the archetype's API", pt)
	}
	// Scenario without the archetype → archetype fallback.
	if pt := bucket.outcomeProductType(bucket.planFor(agentCust), &scenario.Scenario{}); pt != scenario.ProductAPI {
		t.Errorf("unresolved card product type = %s; want API", pt)
	}

	// 1000 events at $0.01, ERROR weighs 0.
	// agents card: AI_AGENT mix 90% SUCCESS → 900 × $0.01 = $9.00 (was $5.00 with the API mix).
	agents, err := computeExpected(bucket, agentCust, 1000, sc)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(agents.Subtotal-9.00) > 1e-9 {
		t.Errorf("agents card subtotal = %v; want 9.00", agents.Subtotal)
	}
	// api card: API mix 50% SUCCESS → 500 × $0.01 = $5.00.
	api, err := computeExpected(bucket, apiCust, 1000, sc)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(api.Subtotal-5.00) > 1e-9 {
		t.Errorf("api card subtotal = %v; want 5.00", api.Subtotal)
	}
}

// TestExpectedFor_OutcomeBasedPerMetric: with per-metric usage the recorded
// per-status split is weighted; a metric recorded without a split falls back
// to the mix of the card's product_filter.
func TestExpectedFor_OutcomeBasedPerMetric(t *testing.T) {
	m, sc := outcomeFixture()
	bucket := groupByArchetype(m)["mixed"]
	agentCust := bucket.customers[1]

	// Tenant totals; each of the two customers gets half.
	run := &runner.RunResult{PerTenantMetric: map[string]map[string]runner.MetricUsage{"t1": {
		// Recorded split: 160 SUCCESS / 20 ERROR / 20 without a status.
		"Agent Runs": {Events: 200, Quantity: 200, ByStatus: map[string]runner.StatusUsage{
			"SUCCESS": {Events: 160, Quantity: 160}, "ERROR": {Events: 20, Quantity: 20}, "": {Events: 20, Quantity: 20}}},
		// No split recorded: 400 events → 200 per customer.
		"API Calls": {Events: 400, Quantity: 400},
	}}}
	res, lines, err := expectedFor(bucket, agentCust, 300, run, sc)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]float64{}
	for _, l := range lines {
		got[l.Metric] = l.ChargeUSD
	}
	// Agent Runs: 80 SUCCESS × 1 + 10 ERROR × 0 + 10 no-status × 1 = 90 × $0.01 = $0.90
	if math.Abs(got["Agent Runs"]-0.90) > 1e-9 {
		t.Errorf("Agent Runs = %v; want 0.90", got["Agent Runs"])
	}
	// API Calls: 200 units by the agents card's AI_AGENT mix (90% SUCCESS) = 180 × $0.01 = $1.80
	if math.Abs(got["API Calls"]-1.80) > 1e-9 {
		t.Errorf("API Calls = %v; want 1.80", got["API Calls"])
	}
	if math.Abs(res.Subtotal-2.70) > 1e-9 {
		t.Errorf("subtotal = %v; want 2.70", res.Subtotal)
	}
}

func TestResolveCard(t *testing.T) {
	_, sc := outcomeFixture()
	if _, c, ok := resolveCard(sc, "mixed", seed.CardRatePlanName("mixed", "agents")); !ok || c.Name != "agents" {
		t.Errorf("by name: %v %+v", ok, c)
	}
	if _, c, ok := resolveCard(sc, "mixed", ""); !ok || c.Name != "api" {
		t.Errorf("unnamed plan → first card: %v %+v", ok, c)
	}
	if _, _, ok := resolveCard(sc, "mixed", "Some Other Plan"); ok {
		t.Error("unknown plan name on a 2-card archetype must not resolve")
	}
	if _, _, ok := resolveCard(sc, "nope", ""); ok {
		t.Error("unknown archetype must not resolve")
	}
	if _, _, ok := resolveCard(nil, "mixed", ""); ok {
		t.Error("nil scenario must not resolve")
	}
	// A bare archetype (no rate_cards) resolves to its own config.
	bare := &scenario.Scenario{Tenants: scenario.Tenants{Archetypes: []scenario.TenantArchetype{{
		Name: "bare", PricingModel: scenario.PricingPerUnit,
		MetricConfigs: map[string]scenario.MetricOverride{"x": {PricingModel: scenario.PricingFlatRate}}}}}}
	if _, c, ok := resolveCard(bare, "bare", "Loadgen Rate Plan bare"); !ok || len(c.MetricConfigs) != 1 {
		t.Errorf("bare archetype: %v %+v", ok, c)
	}
}

// TestBillingMatch_PerMetricEndToEnd runs Check 5 itself: with per-metric
// usage in the run result the $75.50 multi-metric invoice matches; the same
// invoice against a run result without it is compared with the old
// single-metric estimate and fails.
func TestBillingMatch_PerMetricEndToEnd(t *testing.T) {
	m, sc, run := multiMetricFixture()
	m.Tenants[0].Customers = []seed.ManifestCustomer{activeCustomer("c-1", "off"), activeCustomer("c-2", "off")}
	base := minimalRunResult()
	run.RunID, run.StartedAt, run.StoppedAt = base.RunID, base.StartedAt, base.StoppedAt

	check := func(rr *runner.RunResult) *CheckResult {
		t.Helper()
		v, err := New(&Inputs{
			Run: rr, Manifest: m, Scenario: sc,
			Backend:        &billingTestBackend{caps: Capabilities{BillRuns: true}, invoiceUSD: 75.50},
			IncludeBilling: true, OnlyChecks: []string{CheckBillingMatch}, TolerancePct: 0.001,
		})
		if err != nil {
			t.Fatal(err)
		}
		rep, err := v.Run(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for i := range rep.Checks {
			if rep.Checks[i].Name == CheckBillingMatch {
				return rep.Checks[i]
			}
		}
		t.Fatal("billing_match check missing from report")
		return nil
	}

	if c := check(run); c.Status != StatusPass {
		t.Errorf("per-metric run: %s — %s", c.Status, c.Reason)
	}
	old := *run
	old.PerTenantMetric = nil
	if c := check(&old); c.Status != StatusFail {
		t.Errorf("run without per-metric usage: %s; want FAIL (single-metric estimate ≠ $75.50)", c.Status)
	}
}
