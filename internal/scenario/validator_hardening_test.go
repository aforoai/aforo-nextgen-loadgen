package scenario

import (
	"math"
	"strings"
	"testing"
)

// baseArchetype returns an archetype that passes validation on its own —
// tests then MUTATE the specific field they want to check.
func baseArchetype() TenantArchetype {
	return TenantArchetype{
		Name:                 "hardening-base",
		Weight:               1.0,
		PricingModel:         PricingPerUnit,
		BillingMode:          BillingPostpaid,
		ProductTypes:         []ProductType{ProductAPI},
		CustomerCount:        1,
		ProductsPerType:      1,
		SubscriptionStateMix: map[SubscriptionState]float64{StateActive: 1.0},
		CurrencyMix:          map[string]float64{"USD": 1.0},
		RateConfig:           RateConfig{PerUnitRateUSD: 0.001},
	}
}

func baseScenario(a TenantArchetype) *Scenario {
	s := &Scenario{
		SchemaVersion: CurrentSchemaVersion,
		Name:          "test-scenario",
		TargetTPS:     10,
		Duration:      Duration(1_000_000_000), // 1s
		Tenants: Tenants{
			Count:      1,
			Archetypes: []TenantArchetype{a},
		},
		ProductMix:     ProductMix{API: 1.0},
		IngestionPaths: IngestionPaths{RestDirect: 1.0},
	}
	// Apply defaults so the low-stakes fields (distribution, time pattern,
	// payload variation, tax engine) don't drown the validator output —
	// this mirrors LoadFromBytes's path.
	applyDefaults(s)
	return s
}

// TestValidate_MetricOverride_FlatRateWithFeeRejected — Regression for
// HIGH #1 in the 2026-07-22 review: MetricOverride.FlatFeeUSD has no wire
// mapping in pricing-service's metricConfigRequest, so a non-zero fee
// silently drops. Validator MUST reject it before the seeder builds the
// wire body.
func TestValidate_MetricOverride_FlatRateWithFeeRejected(t *testing.T) {
	a := baseArchetype()
	a.MetricConfigs = map[string]MetricOverride{
		"Tokens Consumed": {
			PricingModel: PricingFlatRate,
			FlatFeeUSD:   99.0, // <-- silently-dropped value
		},
	}
	errs := Validate(&Document{Scenario: baseScenario(a)})
	if !errs.HasErrors() {
		t.Fatal("expected validation error for MetricOverride FLAT_RATE with non-zero fee, got clean pass")
	}
	if !strings.Contains(errs.Error(), "flat_fee_usd") {
		t.Errorf("error should mention flat_fee_usd; got %s", errs.Error())
	}
}

// TestValidate_MetricOverride_FlatRateZeroFeeAccepted — the "zero out this
// metric" use case is legit: FLAT_RATE per-metric with fee=0 sends Rate=0
// on the wire and contributes nothing to the bill. Validator must allow it.
func TestValidate_MetricOverride_FlatRateZeroFeeAccepted(t *testing.T) {
	a := baseArchetype()
	a.MetricConfigs = map[string]MetricOverride{
		"Active Users": {
			PricingModel: PricingFlatRate,
			FlatFeeUSD:   0, // documented "don't bill this metric" pattern
		},
	}
	if errs := Validate(&Document{Scenario: baseScenario(a)}); errs.HasErrors() {
		t.Fatalf("MetricOverride FLAT_RATE with fee=0 should pass validation; got %s", errs.Error())
	}
}

// TestValidate_DimensionPricing_RejectsNaN — MED #6: NaN comparisons are
// always false, so a naive `<= 0` check lets NaN through. It would then
// fail JSON marshaling mid-run.
func TestValidate_DimensionPricing_RejectsNaN(t *testing.T) {
	a := baseArchetype()
	a.DimensionPricing = map[string]float64{"web_search": math.NaN()}
	errs := Validate(&Document{Scenario: baseScenario(a)})
	if !errs.HasErrors() {
		t.Fatal("expected validation error for NaN dimension_pricing multiplier")
	}
	if !strings.Contains(errs.Error(), "finite number") {
		t.Errorf("error should mention finite number; got %s", errs.Error())
	}
}

// TestValidate_DimensionPricing_RejectsInfinity — MED #6 sibling: +Inf
// passes the `<= 0` check by satisfying `> 0`, but chokes json.Marshal.
func TestValidate_DimensionPricing_RejectsInfinity(t *testing.T) {
	a := baseArchetype()
	a.DimensionPricing = map[string]float64{"web_search": math.Inf(1)}
	errs := Validate(&Document{Scenario: baseScenario(a)})
	if !errs.HasErrors() {
		t.Fatal("expected validation error for +Inf dimension_pricing multiplier")
	}
}

// TestValidate_DimensionPricing_RejectsNegative — Regression for the
// pre-existing guard.
func TestValidate_DimensionPricing_RejectsNegative(t *testing.T) {
	a := baseArchetype()
	a.DimensionPricing = map[string]float64{"web_search": -1.5}
	if errs := Validate(&Document{Scenario: baseScenario(a)}); !errs.HasErrors() {
		t.Fatal("expected validation error for negative dimension_pricing multiplier")
	}
}

// TestValidate_TierBands_RejectUnorderedGraduated — MED #9: unordered tiers
// silently produce unreachable bands (an earlier tier with up_to > later
// tier). Validator must catch this at parse time.
func TestValidate_TierBands_RejectUnorderedGraduated(t *testing.T) {
	a := baseArchetype()
	a.PricingModel = PricingGraduated
	a.RateConfig.GraduatedTiers = []TierBand{
		{UpToUnits: 100, UnitPriceUSD: 0.01},
		{UpToUnits: 50, UnitPriceUSD: 0.02}, // OUT OF ORDER
		{UpToUnits: 0, UnitPriceUSD: 0.05},
	}
	errs := Validate(&Document{Scenario: baseScenario(a)})
	if !errs.HasErrors() {
		t.Fatal("expected validation error for unordered graduated_tiers")
	}
	if !strings.Contains(errs.Error(), "strictly ascending") {
		t.Errorf("error should mention strictly ascending; got %s", errs.Error())
	}
}

// TestValidate_TierBands_RejectInfinitySentinelMidList — the up_to=0
// sentinel is only valid on the LAST tier; putting it earlier makes every
// subsequent tier unreachable.
func TestValidate_TierBands_RejectInfinitySentinelMidList(t *testing.T) {
	a := baseArchetype()
	a.PricingModel = PricingVolumeTiered
	a.RateConfig.VolumeTiers = []TierBand{
		{UpToUnits: 100, UnitPriceUSD: 0.01},
		{UpToUnits: 0, UnitPriceUSD: 0.001}, // SENTINEL, but not last
		{UpToUnits: 500, UnitPriceUSD: 0.002},
	}
	errs := Validate(&Document{Scenario: baseScenario(a)})
	if !errs.HasErrors() {
		t.Fatal("expected validation error for mid-list infinity sentinel")
	}
	if !strings.Contains(errs.Error(), "LAST") {
		t.Errorf("error should mention LAST-tier constraint; got %s", errs.Error())
	}
}

// TestValidate_TierBands_ProperlyOrderedAccepted — the happy path stays green.
func TestValidate_TierBands_ProperlyOrderedAccepted(t *testing.T) {
	a := baseArchetype()
	a.PricingModel = PricingGraduated
	a.RateConfig.GraduatedTiers = []TierBand{
		{UpToUnits: 1_000, UnitPriceUSD: 0.005},
		{UpToUnits: 100_000, UnitPriceUSD: 0.003},
		{UpToUnits: 0, UnitPriceUSD: 0.001},
	}
	if errs := Validate(&Document{Scenario: baseScenario(a)}); errs.HasErrors() {
		t.Fatalf("well-ordered graduated_tiers should pass; got %s", errs.Error())
	}
}
