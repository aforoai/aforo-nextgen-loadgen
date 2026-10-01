package seed

import (
	"reflect"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func pricingTestMetrics() []ManifestMetric {
	return []ManifestMetric{
		{ID: "m1", Name: "API Calls", AggregationType: "COUNT"},
		{ID: "m2", Name: "Tokens Consumed", AggregationType: "SUM"},
		{ID: "m3", Name: "Peak Connections", AggregationType: "MAX"},
		{ID: "m4", Name: "Agent Sessions", AggregationType: "COUNT"},
	}
}

// TestResolveMetricPricingMatchesRequest locks ResolveMetricPricing (what
// the billing validator prices with) against buildRatePlanRequest (what the
// seeder sends): same metrics, same order, same model, same linear knobs.
func TestResolveMetricPricingMatchesRequest(t *testing.T) {
	cases := map[string]scenario.TenantArchetype{
		"per_unit": {Name: "a", PricingModel: scenario.PricingPerUnit,
			RateConfig: scenario.RateConfig{PerUnitRateUSD: 0.01, IncludedFreeUnits: 100, BlockSizeUnits: 10, MinFeeUSD: 5}},
		"flat": {Name: "a", PricingModel: scenario.PricingFlatRate,
			RateConfig: scenario.RateConfig{FlatFeeUSD: 99}},
		"outcome": {Name: "a", PricingModel: scenario.PricingOutcomeBased,
			RateConfig: scenario.RateConfig{PerUnitRateUSD: 0.02, MinFeeUSD: 1,
				OutcomeWeights: map[string]float64{"success": 1, "ERROR": 0}}},
		"overrides": {Name: "a", PricingModel: scenario.PricingPerUnit,
			RateConfig: scenario.RateConfig{PerUnitRateUSD: 0.01},
			MetricConfigs: map[string]scenario.MetricOverride{
				"tokens consumed": {PricingModel: scenario.PricingGraduated,
					GraduatedTiers: []scenario.TierBand{{UpToUnits: 1000, UnitPriceUSD: 0.002}, {UnitPriceUSD: 0.001}}},
				"Peak Connections": {PricingModel: scenario.PricingFlatRate},
				"Agent Sessions": {PricingModel: scenario.PricingStaircase, IncludedFreeUnits: 5,
					StaircaseBands: []scenario.StaircaseBand{{FromUnits: 1, ToUnits: 10, PriceUSD: 20}, {FromUnits: 11, PriceUSD: 50}}},
			}},
	}
	for name, a := range cases {
		t.Run(name, func(t *testing.T) {
			metrics := pricingTestMetrics()
			req := buildRatePlanRequest(a, []string{"p1"}, metrics)
			got := ResolveMetricPricing(a, metrics)
			if len(got) != len(req.MetricConfigs) {
				t.Fatalf("resolved %d metrics, request has %d", len(got), len(req.MetricConfigs))
			}
			for i, mc := range req.MetricConfigs {
				mp := got[i]
				if mp.MetricName != mc.MetricName || mp.Model != mc.Model {
					t.Errorf("[%d] resolved %s/%s, request %s/%s", i, mp.MetricName, mp.Model, mc.MetricName, mc.Model)
				}
				if mp.AggregationType != metrics[i].AggregationType {
					t.Errorf("[%d] aggregation %q, want %q", i, mp.AggregationType, metrics[i].AggregationType)
				}
				switch mc.Model {
				case scenario.PricingPerUnit, scenario.PricingOutcomeBased:
					if mc.Rate == nil || *mc.Rate != mp.Rate.PerUnitRateUSD ||
						mc.IncludedFree != mp.Rate.IncludedFreeUnits || mc.BlockSize != mp.Rate.BlockSizeUnits || mc.MinFee != mp.Rate.MinFeeUSD {
						t.Errorf("[%d] %s linear knobs differ: request %+v, resolved %+v", i, mc.MetricName, mc, mp.Rate)
					}
					if !reflect.DeepEqual(mc.OutcomeWeights, upperCaseWeights(mp.Rate.OutcomeWeights)) {
						t.Errorf("[%d] %s weights: request %v, resolved %v", i, mc.MetricName, mc.OutcomeWeights, mp.Rate.OutcomeWeights)
					}
				case scenario.PricingFlatRate:
					if mc.Rate == nil || *mc.Rate != mp.Rate.FlatFeeUSD {
						t.Errorf("[%d] flat fee: request %v, resolved %v", i, mc.Rate, mp.Rate.FlatFeeUSD)
					}
				case scenario.PricingGraduated:
					if !reflect.DeepEqual(mc.Tiers, tiersFromBands(mp.Rate.GraduatedTiers)) {
						t.Errorf("[%d] graduated tiers differ", i)
					}
				case scenario.PricingStaircase:
					if !reflect.DeepEqual(mc.Tiers, tiersFromStaircase(mp.Rate.StaircaseBands)) || mc.IncludedFree != mp.Rate.IncludedFreeUnits {
						t.Errorf("[%d] staircase bands differ", i)
					}
				}
			}
		})
	}
}

func TestResolveMetricPricing_Rules(t *testing.T) {
	metrics := pricingTestMetrics()

	// FLAT_RATE: the fee rides on the first metric only.
	flat := ResolveMetricPricing(scenario.TenantArchetype{
		PricingModel: scenario.PricingFlatRate, RateConfig: scenario.RateConfig{FlatFeeUSD: 99}}, metrics)
	if flat[0].Model != scenario.PricingFlatRate || flat[0].Rate.FlatFeeUSD != 99 {
		t.Errorf("first metric = %+v; want FLAT_RATE $99", flat[0])
	}
	for _, mp := range flat[1:] {
		if mp.Model != scenario.PricingPerUnit || mp.Rate.PerUnitRateUSD != 0 || mp.Rate.FlatFeeUSD != 0 {
			t.Errorf("%s = %+v; want PER_UNIT $0", mp.MetricName, mp)
		}
	}

	// OUTCOME_BASED: a MAX metric is priced PER_UNIT without weights.
	out := ResolveMetricPricing(scenario.TenantArchetype{
		PricingModel: scenario.PricingOutcomeBased,
		RateConfig:   scenario.RateConfig{PerUnitRateUSD: 0.02, OutcomeWeights: map[string]float64{"SUCCESS": 1}}}, metrics)
	if out[0].Model != scenario.PricingOutcomeBased || len(out[0].Rate.OutcomeWeights) != 1 {
		t.Errorf("COUNT metric = %+v; want OUTCOME_BASED with weights", out[0])
	}
	if out[2].Model != scenario.PricingPerUnit || out[2].Rate.OutcomeWeights != nil || out[2].Rate.PerUnitRateUSD != 0.02 {
		t.Errorf("MAX metric = %+v; want PER_UNIT $0.02 without weights", out[2])
	}
}

func TestRatePlanMetrics_DedupesByNameInProductOrder(t *testing.T) {
	got := RatePlanMetrics([]ManifestProduct{
		{Metrics: []ManifestMetric{{ID: "a", Name: "API Calls"}, {ID: "b", Name: "Data Transfer"}}},
		{Metrics: []ManifestMetric{{ID: "c", Name: " data transfer "}, {ID: "d", Name: "Tokens"}, {ID: "e"}}},
	})
	want := []ManifestMetric{{ID: "a", Name: "API Calls"}, {ID: "b", Name: "Data Transfer"}, {ID: "d", Name: "Tokens"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestCardRatePlanName(t *testing.T) {
	if got := CardRatePlanName("arch", "pro"); got != "Loadgen Rate Plan arch - pro" {
		t.Errorf("got %q", got)
	}
}
