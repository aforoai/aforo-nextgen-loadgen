package seed

import (
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// A card whose only rate config is its STAIRCASE bands (or OUTCOME_BASED
// weights) must keep it. The seeder used a private zero-check that didn't
// know those two fields, so such a card inherited the archetype's config —
// the "package" card in scenarios/ci-outcome-based.yaml would have been
// sent to pricing as STAIRCASE with no tiers (400 "requires at least one
// step tier").
func TestDeriveCardArchetype_KeepsModelOnlyConfig(t *testing.T) {
	arch := scenario.TenantArchetype{
		Name:         "a",
		PricingModel: scenario.PricingOutcomeBased,
		RateConfig: scenario.RateConfig{
			PerUnitRateUSD: 0.02,
			OutcomeWeights: map[string]float64{"SUCCESS": 1},
		},
	}
	bands := []scenario.StaircaseBand{{FromUnits: 1, ToUnits: 100, PriceUSD: 10}, {FromUnits: 101, PriceUSD: 30}}
	got := deriveCardArchetype(arch, scenario.RateCardSpec{
		Name:         "package",
		PricingModel: scenario.PricingStaircase,
		RateConfig:   scenario.RateConfig{StaircaseBands: bands},
	})
	if len(got.RateConfig.StaircaseBands) != 2 || got.RateConfig.PerUnitRateUSD != 0 {
		t.Fatalf("staircase card lost its bands / inherited archetype config: %+v", got.RateConfig)
	}
	req := buildRatePlanRequest(got, []string{"p"}, []ManifestMetric{{ID: "m", Name: "API Calls", AggregationType: "COUNT"}})
	if len(req.MetricConfigs[0].Tiers) != 2 {
		t.Fatalf("STAIRCASE plan sent %d tiers, want 2", len(req.MetricConfigs[0].Tiers))
	}

	got = deriveCardArchetype(scenario.TenantArchetype{Name: "b", PricingModel: scenario.PricingPerUnit,
		RateConfig: scenario.RateConfig{PerUnitRateUSD: 0.5}}, scenario.RateCardSpec{
		Name:         "outcome",
		PricingModel: scenario.PricingOutcomeBased,
		RateConfig:   scenario.RateConfig{OutcomeWeights: map[string]float64{"SUCCESS": 1, "ERROR": 0}},
	})
	if len(got.RateConfig.OutcomeWeights) != 2 || got.RateConfig.PerUnitRateUSD != 0 {
		t.Fatalf("outcome card lost its weights / inherited archetype config: %+v", got.RateConfig)
	}
}
