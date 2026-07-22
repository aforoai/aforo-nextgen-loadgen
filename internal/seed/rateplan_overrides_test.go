package seed

import (
	"encoding/json"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// TestBuildRatePlanRequest_MetricConfigsOverride locks the heterogeneous-
// pricing-model behavior: a metric named in archetype.MetricConfigs takes
// the override's pricing model + rate, while unnamed metrics fall back to
// the archetype-wide default.
func TestBuildRatePlanRequest_MetricConfigsOverride(t *testing.T) {
	a := scenario.TenantArchetype{
		Name:         "hetero-test",
		PricingModel: scenario.PricingPerUnit,
		RateConfig:   scenario.RateConfig{PerUnitRateUSD: 0.001},
		MetricConfigs: map[string]scenario.MetricOverride{
			"Tokens Consumed": {
				PricingModel: scenario.PricingGraduated,
				GraduatedTiers: []scenario.TierBand{
					{UpToUnits: 1_000_000, UnitPriceUSD: 0.001},
					{UpToUnits: 0, UnitPriceUSD: 0.0005},
				},
			},
			"gpu hours": { // case-insensitive on the map key
				PricingModel: scenario.PricingFlatRate,
				FlatFeeUSD:   25.0,
			},
		},
	}
	metrics := []ManifestMetric{
		{ID: "m1", Name: "Tokens Consumed"},
		{ID: "m2", Name: "GPU Hours"},
		{ID: "m3", Name: "Agent Sessions"}, // no override → archetype default
	}

	body := buildRatePlanRequest(a, []string{"p1"}, metrics)

	byName := map[string]metricConfigRequest{}
	for _, mc := range body.MetricConfigs {
		byName[mc.MetricName] = mc
	}

	// Tokens Consumed: override → GRADUATED with 2 tiers
	if mc := byName["Tokens Consumed"]; mc.Model != scenario.PricingGraduated || len(mc.Tiers) != 2 {
		t.Errorf("Tokens Consumed: model=%q tiers=%d, want GRADUATED w/ 2 tiers (got %+v)", mc.Model, len(mc.Tiers), mc)
	}

	// GPU Hours: case-insensitive override → FLAT_RATE (rate=0)
	if mc := byName["GPU Hours"]; mc.Model != scenario.PricingFlatRate {
		t.Errorf("GPU Hours: model=%q, want FLAT_RATE — did case-insensitive matching regress? mc=%+v", mc.Model, mc)
	}

	// Agent Sessions: falls back to archetype PER_UNIT + PerUnitRateUSD
	mc := byName["Agent Sessions"]
	if mc.Model != scenario.PricingPerUnit {
		t.Errorf("Agent Sessions: model=%q, want archetype default PER_UNIT (%+v)", mc.Model, mc)
	}
	if mc.Rate == nil || *mc.Rate != 0.001 {
		t.Errorf("Agent Sessions: rate=%v, want archetype 0.001", mc.Rate)
	}
}

// TestBuildRatePlanRequest_DimensionPricingRoundTrip locks Rule 21
// dimension_pricing: values from the archetype must land in the wire body
// exactly, and empty maps must be dropped so no `dimensionPricing: {}` is
// sent to pricing-service.
func TestBuildRatePlanRequest_DimensionPricingRoundTrip(t *testing.T) {
	a := scenario.TenantArchetype{
		Name:         "mcp-pricing",
		PricingModel: scenario.PricingPerUnit,
		RateConfig:   scenario.RateConfig{PerUnitRateUSD: 0.01},
		DimensionPricing: map[string]float64{
			"web_search":     1.5,
			"generate_image": 3.0,
			"vector_search":  2.0,
		},
	}
	metrics := []ManifestMetric{{ID: "m1", Name: "MCP Tool Calls"}}

	body := buildRatePlanRequest(a, []string{"p1"}, metrics)
	if body.DimensionPricing == nil {
		t.Fatal("DimensionPricing should be populated when the archetype supplies it")
	}
	if got := body.DimensionPricing["web_search"]; got != 1.5 {
		t.Errorf("web_search multiplier = %v, want 1.5", got)
	}
	if got := body.DimensionPricing["generate_image"]; got != 3.0 {
		t.Errorf("generate_image multiplier = %v, want 3.0", got)
	}
	if got := body.DimensionPricing["vector_search"]; got != 2.0 {
		t.Errorf("vector_search multiplier = %v, want 2.0", got)
	}

	// JSON-shape check: `dimensionPricing` MUST serialize as a map (object),
	// not an array, per Rule 21's canonical shape.
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Parse it back generically to check the shape.
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	dp, ok := back["dimensionPricing"].(map[string]any)
	if !ok {
		t.Fatalf("dimensionPricing was not serialized as a JSON object; got type %T (raw=%s)", back["dimensionPricing"], raw)
	}
	if len(dp) != 3 {
		t.Errorf("dimensionPricing object length = %d, want 3", len(dp))
	}
}

// TestBuildRatePlanRequest_EmptyDimensionPricingDroppedFromWire —
// omitempty on the map ensures no `dimensionPricing` key at all lands on
// the wire when the archetype supplies none. Sending an empty object would
// cause pricing-service to overwrite any existing map on a PATCH — we
// don't want that risk on this pre-launch code path.
func TestBuildRatePlanRequest_EmptyDimensionPricingDroppedFromWire(t *testing.T) {
	a := scenario.TenantArchetype{
		Name:         "no-dim",
		PricingModel: scenario.PricingPerUnit,
		RateConfig:   scenario.RateConfig{PerUnitRateUSD: 0.01},
	}
	metrics := []ManifestMetric{{ID: "m1", Name: "API Calls"}}

	body := buildRatePlanRequest(a, []string{"p1"}, metrics)
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]any
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := back["dimensionPricing"]; present {
		t.Errorf("dimensionPricing key should be omitted when empty; raw=%s", raw)
	}
}
