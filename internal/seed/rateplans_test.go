package seed

import (
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// Per-pricing-model rate plan body assertions. The scenario validator already
// enforces archetype shape; what we test here is that the seed harness encodes
// each pricing model into the EXACT pricing-service create body the platform
// expects.

func TestBuildRatePlanRequest_PerPricingModel(t *testing.T) {
	productIDs := []string{"prod-1"}
	metricIDs := []string{"metric-1"}

	tests := []struct {
		name   string
		a      scenario.TenantArchetype
		assert func(t *testing.T, req ratePlanCreateRequest)
	}{
		{
			name: "FLAT_RATE sets baseFee, metric rate=0",
			a: scenario.TenantArchetype{
				Name:         "flat",
				PricingModel: scenario.PricingFlatRate,
				BillingMode:  scenario.BillingPostpaid,
				RateConfig:   scenario.RateConfig{FlatFeeUSD: 99.0},
			},
			assert: func(t *testing.T, req ratePlanCreateRequest) {
				if req.BaseFee != 99.0 {
					t.Errorf("BaseFee = %v, want 99.0", req.BaseFee)
				}
				if len(req.MetricConfigs) != 1 {
					t.Fatalf("want 1 metric, got %d", len(req.MetricConfigs))
				}
				if req.MetricConfigs[0].Rate != 0 {
					t.Errorf("FLAT_RATE metric rate should be 0, got %v", req.MetricConfigs[0].Rate)
				}
				if req.MetricConfigs[0].Model != scenario.PricingFlatRate {
					t.Errorf("metric pricing model = %s, want FLAT_RATE", req.MetricConfigs[0].Model)
				}
				if req.PricingModel != string(scenario.PricingFlatRate) {
					t.Errorf("top-level pricingModel = %s, want FLAT_RATE", req.PricingModel)
				}
				if req.MetricConfigs[0].BillingTiming != "ARREARS" {
					t.Errorf("billingTiming = %q, want ARREARS (NOT IN_ARREARS)", req.MetricConfigs[0].BillingTiming)
				}
			},
		},
		{
			name: "PER_UNIT propagates rate",
			a: scenario.TenantArchetype{
				Name:         "perunit",
				PricingModel: scenario.PricingPerUnit,
				BillingMode:  scenario.BillingPostpaid,
				RateConfig:   scenario.RateConfig{PerUnitRateUSD: 0.001},
			},
			assert: func(t *testing.T, req ratePlanCreateRequest) {
				if req.BaseFee != 0 {
					t.Errorf("PER_UNIT BaseFee should be 0, got %v", req.BaseFee)
				}
				if req.MetricConfigs[0].Rate != 0.001 {
					t.Errorf("rate = %v, want 0.001", req.MetricConfigs[0].Rate)
				}
			},
		},
		{
			name: "PERCENTAGE includes minFee",
			a: scenario.TenantArchetype{
				Name:         "pct",
				PricingModel: scenario.PricingPercentage,
				BillingMode:  scenario.BillingPostpaid,
				RateConfig:   scenario.RateConfig{PercentageRate: 0.025, MinFeeUSD: 5.0},
			},
			assert: func(t *testing.T, req ratePlanCreateRequest) {
				m := req.MetricConfigs[0]
				if m.Rate != 0.025 {
					t.Errorf("rate = %v, want 0.025", m.Rate)
				}
				if m.MinFee != 5.0 {
					t.Errorf("minFee = %v, want 5.0", m.MinFee)
				}
			},
		},
		{
			name: "INCLUDED_QUOTA with block size",
			a: scenario.TenantArchetype{
				Name:         "quota",
				PricingModel: scenario.PricingIncludedQuota,
				BillingMode:  scenario.BillingPostpaid,
				RateConfig: scenario.RateConfig{
					IncludedFreeUnits: 10000,
					BlockSizeUnits:    100,
					PerUnitRateUSD:    0.001,
				},
			},
			assert: func(t *testing.T, req ratePlanCreateRequest) {
				m := req.MetricConfigs[0]
				if m.IncludedFree != 10000 {
					t.Errorf("includedFree = %d, want 10000", m.IncludedFree)
				}
				if m.BlockSize != 100 {
					t.Errorf("blockSize = %d, want 100", m.BlockSize)
				}
				if m.Rate != 0.001 {
					t.Errorf("rate = %v, want 0.001", m.Rate)
				}
				// INCLUDED_QUOTA MUST set ovBehavior=CHARGE (the legacy
				// "ALLOW" was an invalid enum value; v3 contract is CHARGE).
				if m.OvBehavior != "CHARGE" {
					t.Errorf("ovBehavior = %q, want CHARGE (NOT ALLOW)", m.OvBehavior)
				}
			},
		},
		{
			name: "GRADUATED ladders tiers with prevEnd → tierStart",
			a: scenario.TenantArchetype{
				Name:         "grad",
				PricingModel: scenario.PricingGraduated,
				BillingMode:  scenario.BillingPostpaid,
				RateConfig: scenario.RateConfig{
					GraduatedTiers: []scenario.TierBand{
						{UpToUnits: 1000, UnitPriceUSD: 0.010},
						{UpToUnits: 10000, UnitPriceUSD: 0.007},
						{UpToUnits: 0, UnitPriceUSD: 0.005}, // 0 = infinity
					},
				},
			},
			assert: func(t *testing.T, req ratePlanCreateRequest) {
				tiers := req.MetricConfigs[0].Tiers
				if len(tiers) != 3 {
					t.Fatalf("got %d tiers, want 3", len(tiers))
				}
				// Tier 0: [0, 1000], price 0.010
				if tiers[0].TierStart != 0 || tiers[0].TierEnd != 1000 || tiers[0].UnitPrice != 0.010 {
					t.Errorf("tier 0: %+v", tiers[0])
				}
				// Tier 1: [1000, 10000], price 0.007
				if tiers[1].TierStart != 1000 || tiers[1].TierEnd != 10000 || tiers[1].UnitPrice != 0.007 {
					t.Errorf("tier 1: %+v", tiers[1])
				}
				// Tier 2: [10000, -1 (infinity)], price 0.005
				if tiers[2].TierStart != 10000 || tiers[2].TierEnd != -1 || tiers[2].UnitPrice != 0.005 {
					t.Errorf("tier 2: %+v", tiers[2])
				}
			},
		},
		{
			name: "VOLUME_TIERED uses volume_tiers",
			a: scenario.TenantArchetype{
				Name:         "vol",
				PricingModel: scenario.PricingVolumeTiered,
				BillingMode:  scenario.BillingPostpaid,
				RateConfig: scenario.RateConfig{
					VolumeTiers: []scenario.TierBand{
						{UpToUnits: 10000, UnitPriceUSD: 0.005},
						{UpToUnits: 0, UnitPriceUSD: 0.002},
					},
				},
			},
			assert: func(t *testing.T, req ratePlanCreateRequest) {
				tiers := req.MetricConfigs[0].Tiers
				if len(tiers) != 2 {
					t.Fatalf("got %d tiers, want 2", len(tiers))
				}
				if tiers[0].UnitPrice != 0.005 || tiers[1].UnitPrice != 0.002 {
					t.Errorf("tiers: %+v", tiers)
				}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := buildRatePlanRequest(tc.a, productIDs, metricIDs, "loadgen-rateplan-"+tc.a.Name+"-001")
			if len(req.ProductIDs) != 1 || req.ProductIDs[0] != "prod-1" {
				t.Errorf("ProductIDs = %v", req.ProductIDs)
			}
			if req.ExternalID != "loadgen-rateplan-"+tc.a.Name+"-001" {
				t.Errorf("ExternalID = %s", req.ExternalID)
			}
			tc.assert(t, req)
		})
	}
}

func TestRateConfigSummary_Rendering(t *testing.T) {
	a := scenario.TenantArchetype{
		PricingModel: scenario.PricingIncludedQuota,
		RateConfig: scenario.RateConfig{
			IncludedFreeUnits: 5000,
			PerUnitRateUSD:    0.002,
			BlockSizeUnits:    100,
		},
	}
	got := rateConfigSummary(a)
	if got["pricing_model"] != "INCLUDED_QUOTA" {
		t.Errorf("pricing_model = %v", got["pricing_model"])
	}
	if got["included_free_units"].(int64) != 5000 {
		t.Errorf("included_free_units = %v", got["included_free_units"])
	}
	if got["block_size_units"].(int64) != 100 {
		t.Errorf("block_size_units = %v", got["block_size_units"])
	}
}
