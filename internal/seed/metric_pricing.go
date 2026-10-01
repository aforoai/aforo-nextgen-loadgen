package seed

import (
	"fmt"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// MetricPricing is how one billable unit is priced on a seeded rate plan —
// the scenario-level view of the metricConfigRequest buildRatePlanRequest
// sends. The billing validator prices each metric's usage with it.
//
// Rate carries only the fields Model reads (per-unit rate, allowance, block,
// min fee, tiers, bands, weights, flat fee, percentage).
type MetricPricing struct {
	MetricName      string
	AggregationType string
	Model           scenario.PricingModel
	Rate            scenario.RateConfig
}

// CardRatePlanName is the name of the rate plan the seeder creates for one
// rate card of an archetype. The validator uses it to find the card a
// manifest rate plan was built from.
func CardRatePlanName(archetype, card string) string {
	return fmt.Sprintf("Loadgen Rate Plan %s - %s", archetype, card)
}

// CardArchetype is the archetype a rate card is seeded with: the archetype
// with the card's pricing model, billing mode, rate config, metric overrides
// and dimension pricing applied (see deriveCardArchetype).
func CardArchetype(a scenario.TenantArchetype, card scenario.RateCardSpec) scenario.TenantArchetype {
	return deriveCardArchetype(a, card)
}

// RatePlanMetrics returns the metrics an archetype-seeded rate plan prices:
// every metric of the tenant's products, in product order, de-duplicated by
// name (case-insensitive) — the list seedOne hands to buildRatePlanRequest.
// Metrics without a name are skipped.
func RatePlanMetrics(products []ManifestProduct) []ManifestMetric {
	var out []ManifestMetric
	seen := map[string]struct{}{}
	for _, p := range products {
		for _, m := range p.Metrics {
			key := strings.ToLower(strings.TrimSpace(m.Name))
			if key == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, m)
		}
	}
	return out
}

// ResolveMetricPricing returns the pricing of every metric on the rate plan
// buildRatePlanRequest builds for the archetype (a card-scoped archetype for
// multi-card scenarios), in the same order. It applies the same rules:
//
//   - a metric named in metric_configs (case-insensitive) takes that
//     override; a FLAT_RATE override is sent as PER_UNIT at $0;
//   - otherwise the archetype's model and rate config;
//   - OUTCOME_BASED on a metric whose aggregation is not SUM / COUNT is
//     priced PER_UNIT with the same rate, allowance, block and min fee;
//   - FLAT_RATE puts the fee on the first non-overridden metric and prices
//     the others PER_UNIT at $0.
//
// TestResolveMetricPricingMatchesRequest locks it against the request.
func ResolveMetricPricing(a scenario.TenantArchetype, metrics []ManifestMetric) []MetricPricing {
	overrides := make(map[string]scenario.MetricOverride, len(a.MetricConfigs))
	for k, v := range a.MetricConfigs {
		overrides[strings.ToLower(strings.TrimSpace(k))] = v
	}
	out := make([]MetricPricing, 0, len(metrics))
	flatAssigned := false
	for _, m := range metrics {
		mp := MetricPricing{MetricName: m.Name, AggregationType: m.AggregationType}
		if ov, ok := overrides[strings.ToLower(strings.TrimSpace(m.Name))]; ok {
			mp.Model, mp.Rate = overridePricing(ov)
			out = append(out, mp)
			continue
		}
		model := a.PricingModel
		if model == scenario.PricingOutcomeBased && !outcomeWeightable(m.AggregationType) {
			model = scenario.PricingPerUnit
		}
		if model == scenario.PricingFlatRate {
			if flatAssigned {
				mp.Model = scenario.PricingPerUnit
				out = append(out, mp)
				continue
			}
			flatAssigned = true
		}
		mp.Model = model
		mp.Rate = a.RateConfig
		if model != scenario.PricingOutcomeBased {
			mp.Rate.OutcomeWeights = nil
		}
		out = append(out, mp)
	}
	return out
}

// overridePricing maps a metric_configs entry to the model and rate config
// applyMetricOverride sends for it.
func overridePricing(ov scenario.MetricOverride) (scenario.PricingModel, scenario.RateConfig) {
	if ov.PricingModel == scenario.PricingFlatRate {
		// "Don't bill this metric": sent as PER_UNIT at $0.
		return scenario.PricingPerUnit, scenario.RateConfig{}
	}
	return ov.PricingModel, scenario.RateConfig{
		PerUnitRateUSD:    ov.PerUnitRateUSD,
		PercentageRate:    ov.PercentageRate,
		MinFeeUSD:         ov.MinFeeUSD,
		IncludedFreeUnits: ov.IncludedFreeUnits,
		BlockSizeUnits:    ov.BlockSizeUnits,
		GraduatedTiers:    ov.GraduatedTiers,
		VolumeTiers:       ov.VolumeTiers,
		OutcomeWeights:    ov.OutcomeWeights,
		StaircaseBands:    ov.StaircaseBands,
	}
}
