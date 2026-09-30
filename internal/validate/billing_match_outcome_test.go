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
