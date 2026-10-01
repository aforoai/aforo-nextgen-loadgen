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
	metrics := []ManifestMetric{{ID: "metric-1", Name: "API Calls"}}

	tests := []struct {
		name   string
		a      scenario.TenantArchetype
		assert func(t *testing.T, req ratePlanCreateRequest)
	}{
		{
			// Pricing bills a FLAT_RATE metric's rate once per period and rejects a
			// FLAT_RATE rate of 0, so the fee rides on the metric, not on baseFee.
			name: "FLAT_RATE puts the fee on the metric, baseFee=0",
			a: scenario.TenantArchetype{
				Name:         "flat",
				PricingModel: scenario.PricingFlatRate,
				BillingMode:  scenario.BillingPostpaid,
				RateConfig:   scenario.RateConfig{FlatFeeUSD: 99.0},
			},
			assert: func(t *testing.T, req ratePlanCreateRequest) {
				if req.BaseFee != 0 {
					t.Errorf("BaseFee = %v, want 0 (fee is on the metric)", req.BaseFee)
				}
				if len(req.MetricConfigs) != 1 {
					t.Fatalf("want 1 metric, got %d", len(req.MetricConfigs))
				}
				if req.MetricConfigs[0].Rate == nil || *req.MetricConfigs[0].Rate != 99.0 {
					t.Errorf("FLAT_RATE metric rate = %v, want 99.0", req.MetricConfigs[0].Rate)
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
				if req.MetricConfigs[0].Rate == nil || *req.MetricConfigs[0].Rate != 0.001 {
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
				// Scenario fraction 0.025 → pricing percent 2.5 (pricing bills base × rate / 100).
				if m.Rate == nil || *m.Rate != 2.5 {
					t.Errorf("rate = %v, want 2.5 (percent)", m.Rate)
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
				if m.Rate == nil || *m.Rate != 0.001 {
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
				// Pricing's 1-based inclusive encoding (PricingCalculator
				// Option B): [1, 1000], [1001, 10000], [10001, null].
				if tiers[0].TierStart != 1 || tiers[0].TierEnd == nil || *tiers[0].TierEnd != 1000 || tiers[0].UnitPrice != 0.010 {
					t.Errorf("tier 0: %+v", tiers[0])
				}
				if tiers[1].TierStart != 1001 || tiers[1].TierEnd == nil || *tiers[1].TierEnd != 10000 || tiers[1].UnitPrice != 0.007 {
					t.Errorf("tier 1: %+v", tiers[1])
				}
				// Open-ended top tier is JSON null — never -1, which billing
				// reads as a real (inverted) bound and skips.
				if tiers[2].TierStart != 10001 || tiers[2].TierEnd != nil || tiers[2].UnitPrice != 0.005 {
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
			// buildRatePlanRequest signature dropped its externalID arg
			// (rename pass — see CONVENTIONS.md "Wire-format alignment").
			// The deterministic backend identity for cross-day idempotency
			// is now Name; the Idempotency-Key header is set by
			// provisionRatePlanWithName via the caller's seedKey, not by this builder.
			req := buildRatePlanRequest(tc.a, productIDs, metrics)
			if len(req.ProductIDs) != 1 || req.ProductIDs[0] != "prod-1" {
				t.Errorf("ProductIDs = %v", req.ProductIDs)
			}
			wantName := "Loadgen Rate Plan " + tc.a.Name
			if req.Name != wantName {
				t.Errorf("Name = %q, want %q", req.Name, wantName)
			}
			// V57: non-flat plans must carry metricName alongside metricId so the
			// rate plan persists the canonical name (billing name-fallback).
			if tc.a.PricingModel != scenario.PricingFlatRate {
				if len(req.MetricConfigs) == 0 {
					t.Fatalf("expected at least one metric config for %s", tc.a.PricingModel)
				}
				if req.MetricConfigs[0].MetricID != "metric-1" {
					t.Errorf("MetricID = %q, want metric-1", req.MetricConfigs[0].MetricID)
				}
				if req.MetricConfigs[0].MetricName != "API Calls" {
					t.Errorf("MetricName = %q, want \"API Calls\"", req.MetricConfigs[0].MetricName)
				}
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

// A FLAT_RATE plan with several metrics must bill the fee once: the first metric
// carries it as FLAT_RATE, the others go out PER_UNIT at $0 (pricing rejects a
// FLAT_RATE metric with rate 0). Total per period = 49.
func TestBuildRatePlanRequest_FlatRateMultiMetricBillsFeeOnce(t *testing.T) {
	a := scenario.TenantArchetype{
		Name: "flat", PricingModel: scenario.PricingFlatRate, BillingMode: scenario.BillingPostpaid,
		RateConfig: scenario.RateConfig{FlatFeeUSD: 49},
	}
	req := buildRatePlanRequest(a, []string{"p"}, []ManifestMetric{{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "C"}})
	if req.BaseFee != 0 {
		t.Fatalf("BaseFee = %v, want 0", req.BaseFee)
	}
	total := 0.0
	flat := 0
	for _, mc := range req.MetricConfigs {
		if mc.Rate == nil {
			t.Fatalf("metric %s has no rate", mc.MetricID)
		}
		switch mc.Model {
		case scenario.PricingFlatRate:
			flat++
			if *mc.Rate <= 0 {
				t.Errorf("FLAT_RATE metric %s rate %v — pricing rejects <= 0", mc.MetricID, *mc.Rate)
			}
			total += *mc.Rate
		case scenario.PricingPerUnit:
			if *mc.Rate != 0 {
				t.Errorf("filler metric %s rate %v, want 0", mc.MetricID, *mc.Rate)
			}
		default:
			t.Errorf("unexpected model %s", mc.Model)
		}
	}
	if flat != 1 || total != 49 {
		t.Fatalf("flat metrics=%d total=%v, want 1 and 49", flat, total)
	}
}

// With no metrics at all the fee has nowhere else to go: it stays the plan baseFee.
func TestBuildRatePlanRequest_FlatRateNoMetricsUsesBaseFee(t *testing.T) {
	a := scenario.TenantArchetype{
		Name: "flat", PricingModel: scenario.PricingFlatRate, BillingMode: scenario.BillingPostpaid,
		RateConfig: scenario.RateConfig{FlatFeeUSD: 49},
	}
	req := buildRatePlanRequest(a, []string{"p"}, nil)
	if req.BaseFee != 49 || len(req.MetricConfigs) != 0 {
		t.Fatalf("BaseFee=%v metrics=%d, want 49 and 0", req.BaseFee, len(req.MetricConfigs))
	}
}

func TestPercentOf(t *testing.T) {
	for in, want := range map[float64]float64{0.025: 2.5, 0.029: 2.9, 0.03: 3, 1: 100, 0.0001: 0.01} {
		if got := percentOf(in); got != want {
			t.Errorf("percentOf(%v) = %v, want %v", in, got, want)
		}
	}
}
