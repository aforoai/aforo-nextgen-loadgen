package seed

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// Wire-shape tests for OUTCOME_BASED and STAIRCASE — the field names match
// pricing-service CreateRatePlanRequest.MetricConfigRequest (model, rate,
// includedFree, blockSize, minFee, outcomeWeights, tiers[]) and
// MetricTierRequest (tierStart, tierEnd, unitPrice, flatFee, sortOrder).

func TestBuildRatePlanRequest_OutcomeBased(t *testing.T) {
	a := scenario.TenantArchetype{
		Name:         "outcome",
		PricingModel: scenario.PricingOutcomeBased,
		BillingMode:  scenario.BillingPostpaid,
		RateConfig: scenario.RateConfig{
			PerUnitRateUSD:    0.02,
			IncludedFreeUnits: 100,
			BlockSizeUnits:    10,
			MinFeeUSD:         5,
			OutcomeWeights:    map[string]float64{"success": 1, " Partial ": 0.5, "HITL_REQUIRED": 0},
		},
	}
	metrics := []ManifestMetric{
		{ID: "m-count", Name: "Agent Sessions", AggregationType: "COUNT"},
		{ID: "m-sum", Name: "Tokens Consumed", AggregationType: "SUM"},
		{ID: "m-max", Name: "Active Agents", AggregationType: "MAX"},
		{ID: "m-unknown", Name: "Legacy"},
	}
	req := buildRatePlanRequest(a, []string{"p-1"}, metrics)
	if req.PricingModel != "OUTCOME_BASED" {
		t.Errorf("plan pricingModel = %s", req.PricingModel)
	}
	byName := map[string]metricConfigRequest{}
	for _, mc := range req.MetricConfigs {
		byName[mc.MetricName] = mc
	}
	for _, name := range []string{"Agent Sessions", "Tokens Consumed", "Legacy"} {
		mc := byName[name]
		if mc.Model != scenario.PricingOutcomeBased {
			t.Errorf("%s: model %s; want OUTCOME_BASED", name, mc.Model)
		}
		if mc.Rate == nil || *mc.Rate != 0.02 || mc.IncludedFree != 100 || mc.BlockSize != 10 || mc.MinFee != 5 {
			t.Errorf("%s: linear knobs not sent: %+v", name, mc)
		}
		// Keys trimmed + upper-cased; zero weights kept (they're meaningful).
		want := map[string]float64{"SUCCESS": 1, "PARTIAL": 0.5, "HITL_REQUIRED": 0}
		if len(mc.OutcomeWeights) != len(want) {
			t.Fatalf("%s: weights %v", name, mc.OutcomeWeights)
		}
		for k, v := range want {
			if got, ok := mc.OutcomeWeights[k]; !ok || got != v {
				t.Errorf("%s: weight %s = %v (present=%v); want %v", name, k, got, ok, v)
			}
		}
	}
	// MAX can't carry OUTCOME_BASED (pricing would reject the plan) → PER_UNIT, no weights.
	mx := byName["Active Agents"]
	if mx.Model != scenario.PricingPerUnit || mx.OutcomeWeights != nil || mx.Rate == nil || *mx.Rate != 0.02 {
		t.Errorf("MAX metric should fall back to PER_UNIT without weights: %+v", mx)
	}

	raw, _ := json.Marshal(byName["Agent Sessions"])
	if !strings.Contains(string(raw), `"outcomeWeights":{`) || !strings.Contains(string(raw), `"model":"OUTCOME_BASED"`) {
		t.Errorf("wire body missing outcomeWeights/model: %s", raw)
	}
	raw, _ = json.Marshal(mx)
	if strings.Contains(string(raw), "outcomeWeights") {
		t.Errorf("PER_UNIT fallback must not send outcomeWeights: %s", raw)
	}
}

func TestBuildRatePlanRequest_OutcomeOverride(t *testing.T) {
	a := scenario.TenantArchetype{
		Name: "mixed", PricingModel: scenario.PricingPerUnit, BillingMode: scenario.BillingPostpaid,
		RateConfig: scenario.RateConfig{PerUnitRateUSD: 0.001},
		MetricConfigs: map[string]scenario.MetricOverride{
			"api calls": {PricingModel: scenario.PricingOutcomeBased, PerUnitRateUSD: 0.01,
				OutcomeWeights: map[string]float64{"blocked": 0}},
		},
	}
	req := buildRatePlanRequest(a, []string{"p"}, []ManifestMetric{{ID: "m", Name: "API Calls", AggregationType: "COUNT"}})
	mc := req.MetricConfigs[0]
	if mc.Model != scenario.PricingOutcomeBased || mc.OutcomeWeights["BLOCKED"] != 0 || len(mc.OutcomeWeights) != 1 {
		t.Errorf("override not applied: %+v", mc)
	}
}

func TestBuildRatePlanRequest_Staircase(t *testing.T) {
	a := scenario.TenantArchetype{
		Name: "stairs", PricingModel: scenario.PricingStaircase, BillingMode: scenario.BillingPostpaid,
		RateConfig: scenario.RateConfig{
			IncludedFreeUnits: 50,
			StaircaseBands: []scenario.StaircaseBand{
				{FromUnits: 1, ToUnits: 100, PriceUSD: 10},
				{FromUnits: 101, PriceUSD: 30},
			},
		},
	}
	req := buildRatePlanRequest(a, []string{"p"}, []ManifestMetric{{ID: "m", Name: "API Calls"}})
	mc := req.MetricConfigs[0]
	if mc.Model != scenario.PricingStaircase || mc.Rate != nil || mc.IncludedFree != 50 {
		t.Errorf("staircase knobs: %+v", mc)
	}
	if len(mc.Tiers) != 2 {
		t.Fatalf("tiers %+v", mc.Tiers)
	}
	t0, t1 := mc.Tiers[0], mc.Tiers[1]
	if t0.TierStart != 1 || t0.TierEnd == nil || *t0.TierEnd != 100 || t0.FlatFee != 10 || t0.UnitPrice != 0 || t0.SortOrder != 0 {
		t.Errorf("tier 0: %+v", t0)
	}
	if t1.TierStart != 101 || t1.TierEnd != nil || t1.FlatFee != 30 || t1.SortOrder != 1 {
		t.Errorf("tier 1: %+v", t1)
	}
	raw, _ := json.Marshal(mc)
	// Open-ended step must be null (pricing's -1 sentinel would fail "end < start").
	if !strings.Contains(string(raw), `"tierStart":101,"tierEnd":null`) {
		t.Errorf("open-ended step not null: %s", raw)
	}
	if strings.Contains(string(raw), `"rate"`) {
		t.Errorf("STAIRCASE must not send a rate: %s", raw)
	}
}

func TestBuildRatePlanRequest_PerUnitSendsBlockAndMinFee(t *testing.T) {
	a := scenario.TenantArchetype{
		Name: "pu", PricingModel: scenario.PricingPerUnit, BillingMode: scenario.BillingPostpaid,
		RateConfig: scenario.RateConfig{PerUnitRateUSD: 0.01, BlockSizeUnits: 100, MinFeeUSD: 20},
	}
	mc := buildRatePlanRequest(a, []string{"p"}, []ManifestMetric{{ID: "m", Name: "API Calls"}}).MetricConfigs[0]
	if mc.BlockSize != 100 || mc.MinFee != 20 {
		t.Errorf("PER_UNIT block/min fee not sent: %+v", mc)
	}
}

func TestRateConfigSummary_OutcomeAndStaircase(t *testing.T) {
	out := rateConfigSummary(scenario.TenantArchetype{
		PricingModel: scenario.PricingOutcomeBased,
		RateConfig: scenario.RateConfig{PerUnitRateUSD: 0.02, MinFeeUSD: 5,
			OutcomeWeights: map[string]float64{"success": 1}},
	})
	w, _ := out["outcome_weights"].(map[string]any)
	if out["per_unit_rate_usd"] != 0.02 || out["min_fee_usd"] != 5.0 || w["SUCCESS"] != 1.0 {
		t.Errorf("outcome summary: %v", out)
	}
	out = rateConfigSummary(scenario.TenantArchetype{
		PricingModel: scenario.PricingStaircase,
		RateConfig:   scenario.RateConfig{StaircaseBands: []scenario.StaircaseBand{{FromUnits: 1, PriceUSD: 9}}},
	})
	bands, _ := out["staircase_bands"].([]map[string]any)
	if len(bands) != 1 || bands[0]["price_usd"] != 9.0 {
		t.Errorf("staircase summary: %v", out)
	}
}

func TestCatalogRejectsUnsupportedModels(t *testing.T) {
	var errs []string
	add := func(f string, a ...any) { errs = append(errs, f) }
	for _, m := range []string{"STAIRCASE", "OUTCOME_BASED"} {
		errs = nil
		validateRateConfig(add, "ratePlans[0]", m, CatalogRateConfig{})
		if len(errs) != 1 || !strings.Contains(errs[0], "not supported in catalog mode") {
			t.Errorf("%s: %v", m, errs)
		}
	}
}
