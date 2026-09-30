package validate

import (
	"encoding/json"
	"math"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// roundTrip mimics a manifest written to disk and read back: the rate-plan
// config comes back as map[string]any with []any / float64 values.
func roundTrip(t *testing.T, m map[string]any) map[string]any {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestComputeExpected_OutcomeBased(t *testing.T) {
	cfg := roundTrip(t, map[string]any{
		"pricing_model":     "OUTCOME_BASED",
		"per_unit_rate_usd": 0.02,
		"outcome_weights":   map[string]any{"SUCCESS": 1.0, "PARTIAL": 0.5, "HITL_REQUIRED": 0.0},
	})
	bucket := &archBucket{
		pricingModel: scenario.PricingOutcomeBased,
		billingMode:  scenario.BillingPostpaid,
		rate:         rateConfigFromMap(cfg),
		productType:  scenario.ProductAIAgent,
	}
	sc := &scenario.Scenario{OutcomeMix: map[string]float64{"SUCCESS": 0.9, "PARTIAL": 0.05, "HITL_REQUIRED": 0.05}}
	res, err := computeExpected(bucket, seed.ManifestCustomer{CustomerID: "c"}, 1000, sc)
	if err != nil {
		t.Fatal(err)
	}
	// 1000 events split 900/50/50 → 900×1 + 50×0.5 + 50×0 = 925 units × $0.02 = $18.50
	if math.Abs(res.Subtotal-18.50) > 1e-9 {
		t.Errorf("subtotal = %v; want 18.50", res.Subtotal)
	}

	// No configured mix → the AI_AGENT default (SUCCESS .92, others weightless → 1.0):
	// 920×1 + 80×1 = 1000 units × $0.02 = $20.00 (only SUCCESS/PARTIAL/HITL are weighted)
	res, _ = computeExpected(bucket, seed.ManifestCustomer{CustomerID: "c"}, 1000, &scenario.Scenario{})
	if math.Abs(res.Subtotal-20.00) > 1e-9 {
		t.Errorf("default-mix subtotal = %v; want 20.00", res.Subtotal)
	}
}

func TestComputeExpected_Staircase(t *testing.T) {
	cfg := roundTrip(t, map[string]any{
		"pricing_model": "STAIRCASE",
		"staircase_bands": []map[string]any{
			{"from_units": 1, "to_units": 100, "price_usd": 10.0},
			{"from_units": 101, "to_units": 0, "price_usd": 30.0},
		},
	})
	bucket := &archBucket{
		pricingModel: scenario.PricingStaircase,
		billingMode:  scenario.BillingPostpaid,
		rate:         rateConfigFromMap(cfg),
	}
	// 150 events land in [101, ∞) → $30
	res, err := computeExpected(bucket, seed.ManifestCustomer{CustomerID: "c"}, 150, &scenario.Scenario{})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(res.Subtotal-30) > 1e-9 {
		t.Errorf("subtotal = %v; want 30", res.Subtotal)
	}
	// In-memory shape ([]map[string]any, int64) parses too.
	r := rateConfigFromMap(map[string]any{"staircase_bands": []map[string]any{{"from_units": int64(1), "price_usd": 5.0}}})
	if len(r.StaircaseBands) != 1 || r.StaircaseBands[0].FromUnits != 1 || r.StaircaseBands[0].PriceUSD != 5 {
		t.Errorf("in-memory bands: %+v", r.StaircaseBands)
	}
}

// TestComputeExpected_PerCard: a two-card archetype prices each customer
// with the card their subscription is on (customer → subscription →
// offering → rate plan). Before, every customer was estimated with the
// tenant's FIRST rate plan, so staircase customers were expected to pay
// the outcome price.
func TestComputeExpected_PerCard(t *testing.T) {
	m := &seed.Manifest{Tenants: []seed.ManifestTenant{{
		TenantID: "t1", Archetype: "arch",
		PricingModel: scenario.PricingOutcomeBased, BillingMode: scenario.BillingPostpaid,
		Products: []seed.ManifestProduct{{ProductType: scenario.ProductAIAgent}},
		RatePlans: []seed.ManifestRatePlan{
			{RatePlanID: "rp-outcome", Config: map[string]any{
				"pricing_model": "OUTCOME_BASED", "per_unit_rate_usd": 0.02,
				"outcome_weights": map[string]any{"SUCCESS": 1.0}}},
			{RatePlanID: "rp-package", Config: map[string]any{
				"pricing_model": "STAIRCASE",
				"staircase_bands": []map[string]any{
					{"from_units": int64(1), "to_units": int64(100), "price_usd": 10.0},
					{"from_units": int64(101), "to_units": int64(0), "price_usd": 30.0},
				}}},
		},
		Offerings: []seed.ManifestOffering{
			{OfferingID: "off-outcome", RatePlanID: "rp-outcome", BillingMode: scenario.BillingPostpaid},
			{OfferingID: "off-package", RatePlanID: "rp-package", BillingMode: scenario.BillingPrepaid},
		},
		Customers: []seed.ManifestCustomer{
			{CustomerID: "c-out", Subscriptions: []seed.ManifestSubscription{{OfferingID: "off-outcome"}}},
			{CustomerID: "c-pkg", Subscriptions: []seed.ManifestSubscription{{OfferingID: "off-package"}}},
		},
	}}}
	bucket := groupByArchetype(m)["arch"]
	sc := &scenario.Scenario{OutcomeMix: map[string]float64{"SUCCESS": 1}}

	// 150 events, outcome card: 150 × 1 × $0.02 = $3.00, POSTPAID.
	out, err := computeExpected(bucket, bucket.customers[0], 150, sc)
	if err != nil {
		t.Fatal(err)
	}
	if out.Subtotal != 3.00 || out.InvoiceUSD != 3.00 || out.WalletDebit != 0 {
		t.Errorf("outcome card: %+v; want $3.00 invoiced", out)
	}
	// 150 events, package card: band [101, ∞) → $30.00, PREPAID (wallet).
	pkg, err := computeExpected(bucket, bucket.customers[1], 150, sc)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.Subtotal != 30.00 || pkg.WalletDebit != 30.00 || pkg.InvoiceUSD != 0 {
		t.Errorf("package card: %+v; want $30.00 from the wallet", pkg)
	}
	if p := bucket.planFor(bucket.customers[1]); p.billingMode != scenario.BillingPrepaid || p.pricingModel != scenario.PricingStaircase {
		t.Errorf("planFor(package customer) = %+v", p)
	}
	// A customer whose offering is unknown falls back to the first plan.
	if p := bucket.planFor(seed.ManifestCustomer{CustomerID: "x"}); p.pricingModel != scenario.PricingOutcomeBased {
		t.Errorf("fallback plan = %+v", p)
	}
}

// TestComputeExpected_GraduatedInMemoryManifest: the orchestrator hands the
// validator the seeder's in-memory manifest, whose tier config is
// []map[string]any. It used to parse as "no tiers" → $0 expected.
// 15,000 events over [≤1000 @ .010, ≤10000 @ .007, rest @ .005] = $98.00.
func TestComputeExpected_GraduatedInMemoryManifest(t *testing.T) {
	cfg := map[string]any{
		"pricing_model": "GRADUATED",
		"graduated_tiers": []map[string]any{
			{"up_to_units": int64(1000), "unit_price_usd": 0.010},
			{"up_to_units": int64(10000), "unit_price_usd": 0.007},
			{"up_to_units": int64(0), "unit_price_usd": 0.005},
		},
	}
	for name, c := range map[string]map[string]any{"in-memory": cfg, "json": roundTrip(t, cfg)} {
		bucket := &archBucket{
			pricingModel: scenario.PricingGraduated,
			billingMode:  scenario.BillingPostpaid,
			rate:         rateConfigFromMap(c),
		}
		res, err := computeExpected(bucket, seed.ManifestCustomer{CustomerID: "c"}, 15000, &scenario.Scenario{})
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(res.Subtotal-98.00) > 1e-9 {
			t.Errorf("%s: subtotal = %v; want 98.00", name, res.Subtotal)
		}
	}
}
