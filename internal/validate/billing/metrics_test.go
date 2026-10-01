package billing

import (
	"math"
	"testing"
)

func TestCalculateMetrics_SumsPerMetricCharges(t *testing.T) {
	in := MetricsInputs{
		Mode:     Postpaid,
		Discount: &Discount{Type: "PERCENTAGE", Value: 0.10},
		TaxPct:   0.05,
		Metrics: []MetricInput{
			// 500 − 100 free = 400 × $0.01 = $4.00
			{Name: "calls", Units: 500, Model: PerUnit,
				Rate: RateConfig{PerUnitRateUSD: 0.01, IncludedFreeUnits: 100}},
			// 100 × $0.001 = $0.10 → min fee $3.00
			{Name: "webhooks", Units: 100, Model: PerUnit,
				Rate: RateConfig{PerUnitRateUSD: 0.001, MinFeeUSD: 3}},
			// 1000 × $0.002 + 1500 × $0.001 = $3.50
			{Name: "tokens", Units: 2500, Model: Graduated, Rate: RateConfig{GraduatedTiers: []Tier{
				{UpToUnits: 1000, UnitPrice: 0.002}, {UpToUnits: math.MaxInt64, UnitPrice: 0.001}}}},
			// 20 − 5 free = 15 → band [11, ∞) = $50.00
			{Name: "sessions", Units: 20, Model: Staircase, Rate: RateConfig{IncludedFreeUnits: 5,
				StaircaseBands: []Band{{From: 1, To: 10, Price: 20}, {From: 11, Price: 50}}}},
			// 750 − 500 free = 250 → 3 blocks × 100 × $0.05 = $15.00
			{Name: "transfer", Units: 750, Model: IncludedQuota,
				Rate: RateConfig{PerUnitRateUSD: 0.05, IncludedFreeUnits: 500, BlockSizeUnits: 100}},
			// unused: no line, so no min fee and no first band
			{Name: "unused-min-fee", Model: PerUnit, Rate: RateConfig{PerUnitRateUSD: 1, MinFeeUSD: 9}},
			{Name: "unused-staircase", Model: Staircase, Rate: RateConfig{StaircaseBands: []Band{{From: 1, Price: 7}}}},
			{Name: "unused-percentage", Model: Percentage, Rate: RateConfig{PercentageRate: 0.02, MinFeeUSD: 4}},
		},
	}
	res, lines, err := CalculateMetrics(in, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []float64{4.00, 3.00, 3.50, 50.00, 15.00, 0, 0, 0}
	for i, w := range want {
		if math.Abs(lines[i].Charge-w) > 1e-9 {
			t.Errorf("%s charge = %v; want %v", lines[i].Name, lines[i].Charge, w)
		}
	}
	// subtotal 75.50; −10% = 67.95; +5% tax 3.40 (3.3975 half-even) = 71.35
	if math.Abs(res.Subtotal-75.50) > 1e-9 || math.Abs(res.DiscountAmount-7.55) > 1e-9 ||
		math.Abs(res.TaxAmount-3.40) > 1e-9 || math.Abs(res.Total-71.35) > 1e-9 {
		t.Errorf("result = %+v; want subtotal 75.50, discount 7.55, tax 3.40, total 71.35", res)
	}
	if res.InvoiceUSD != res.Total || res.WalletDebit != 0 {
		t.Errorf("POSTPAID route = wallet %v invoice %v", res.WalletDebit, res.InvoiceUSD)
	}
}

// FLAT_RATE bills every period, used or not; OUTCOME_BASED weights its own
// per-status rows; an unknown model is an error.
func TestCalculateMetrics_FlatOutcomeAndErrors(t *testing.T) {
	res, lines, err := CalculateMetrics(MetricsInputs{Mode: Prepaid, Metrics: []MetricInput{
		{Name: "platform", Model: FlatRate, Rate: RateConfig{FlatFeeUSD: 99}},
		{Name: "runs", Units: 100, Model: OutcomeBased, Aggregation: "COUNT",
			Outcomes: []OutcomeUsage{{Status: "SUCCESS", EventCount: 80}, {Status: "PARTIAL", EventCount: 10}, {Status: "ERROR", EventCount: 10}},
			Rate:     RateConfig{PerUnitRateUSD: 0.10, OutcomeWeights: map[string]float64{"success": 1, "partial": 0.5, "error": 0}}},
	}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// $99 + (80 + 5 + 0) × $0.10 = $107.50, all from the wallet.
	if lines[0].Charge != 99 || math.Abs(lines[1].Charge-8.50) > 1e-9 || math.Abs(res.Total-107.50) > 1e-9 || res.WalletDebit != res.Total {
		t.Errorf("lines %+v result %+v", lines, res)
	}
	if _, _, err := CalculateMetrics(MetricsInputs{Metrics: []MetricInput{{Name: "x", Units: 1, Model: "BOGUS"}}}, 0); err == nil {
		t.Error("unknown model with usage must error")
	}
	if _, _, err := CalculateMetrics(MetricsInputs{Metrics: []MetricInput{{Name: "x", Model: "BOGUS"}}}, 0); err == nil {
		t.Error("unknown model without usage must error")
	}
}

// A one-metric plan rated per metric equals the single-metric Calculate.
func TestCalculateMetrics_SingleMetricParity(t *testing.T) {
	rates := map[PricingModel]RateConfig{
		PerUnit:       {PerUnitRateUSD: 0.013, IncludedFreeUnits: 7, BlockSizeUnits: 10, MinFeeUSD: 1},
		IncludedQuota: {PerUnitRateUSD: 0.02, IncludedFreeUnits: 100, BlockSizeUnits: 25},
		Graduated:     {GraduatedTiers: []Tier{{UpToUnits: 100, UnitPrice: 0.05}, {UpToUnits: math.MaxInt64, UnitPrice: 0.01, FlatFee: 2}}},
		VolumeTiered:  {VolumeTiers: []Tier{{UpToUnits: 100, UnitPrice: 0.05}, {UpToUnits: math.MaxInt64, UnitPrice: 0.01}}},
		Staircase:     {StaircaseBands: []Band{{From: 1, To: 100, Price: 10}, {From: 101, Price: 30}}},
		FlatRate:      {FlatFeeUSD: 49},
		Percentage:    {PercentageRate: 0.025, MinFeeUSD: 0.5},
	}
	for model, rc := range rates {
		for _, events := range []int64{1, 99, 100, 101, 1234} {
			single, err := Calculate(CalcInputs{Events: events, ChargeBaseUSD: float64(events) * 3, Model: model, Rate: rc, Mode: Postpaid}, 0)
			if err != nil {
				t.Fatal(err)
			}
			multi, _, err := CalculateMetrics(MetricsInputs{Mode: Postpaid, Metrics: []MetricInput{
				{Name: "m", Units: float64(events), ChargeBaseUSD: float64(events) * 3, Model: model, Rate: rc}}}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if single.Total != multi.Total {
				t.Errorf("%s @ %d events: Calculate %v, CalculateMetrics %v", model, events, single.Total, multi.Total)
			}
		}
	}
}
