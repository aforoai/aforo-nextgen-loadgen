package billing

import (
	"math"
	"testing"
)

// Hand-computed expectations for OUTCOME_BASED (billing's OutcomeWeighting +
// PricingCalculator.perUnit) and STAIRCASE (PricingCalculator.staircase).
// Every expected number has its arithmetic written next to it.

const eps = 1e-9

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

func TestWeightOutcomes_SumUsesQuantity(t *testing.T) {
	rows := []OutcomeUsage{
		{Status: "SUCCESS", Quantity: 900, EventCount: 90},
		{Status: "PARTIAL", Quantity: 40, EventCount: 4},
		{Status: "ERROR", Quantity: 60, EventCount: 6},
	}
	// Lower-case weight keys: matched case-insensitively.
	w := map[string]float64{"success": 1, "partial": 0.5, "error": 0}
	got, ok := WeightOutcomes(rows, w, "SUM")
	// 900×1 + 40×0.5 + 60×0 = 900 + 20 + 0 = 920 ; unweighted 900+40+60 = 1000
	if !ok || !near(got.Weighted, 920) || !near(got.Unweighted, 1000) {
		t.Errorf("SUM: got %+v ok=%v; want weighted 920 of 1000", got, ok)
	}
	// Empty aggregation = legacy SUM.
	got, ok = WeightOutcomes(rows, w, "")
	if !ok || !near(got.Weighted, 920) {
		t.Errorf("blank aggregation should weight quantity: %+v", got)
	}
}

func TestWeightOutcomes_CountUsesEventCount(t *testing.T) {
	rows := []OutcomeUsage{
		{Status: "SUCCESS", Quantity: 500, EventCount: 10},
		{Status: "PARTIAL", Quantity: 300, EventCount: 4},
	}
	w := map[string]float64{"SUCCESS": 1, "PARTIAL": 0.5}
	got, ok := WeightOutcomes(rows, w, "count")
	// COUNT weights event count: 10×1 + 4×0.5 = 10 + 2 = 12 (quantity 500/300 ignored)
	if !ok || !near(got.Weighted, 12) || !near(got.Unweighted, 14) {
		t.Errorf("COUNT: got %+v; want weighted 12 of 14", got)
	}
	got, _ = WeightOutcomes(rows, w, "SUM")
	// SUM on the same rows: 500×1 + 300×0.5 = 650
	if !near(got.Weighted, 650) {
		t.Errorf("SUM: got %v; want 650", got.Weighted)
	}
}

func TestWeightOutcomes_MissingStatusAndUnweightedStatusCountAtOne(t *testing.T) {
	rows := []OutcomeUsage{
		{Status: "", Quantity: 7},         // no status → 1.0
		{Status: "null", Quantity: 3},     // JSON null read as "null" → no status → 1.0
		{Status: "TIMEOUT", Quantity: 10}, // status with no weight → 1.0
		{Status: "PARTIAL", Quantity: 4},  // weight 0.5
	}
	w := map[string]float64{"SUCCESS": 1, "PARTIAL": 0.5}
	got, ok := WeightOutcomes(rows, w, "SUM")
	// 7×1 + 3×1 + 10×1 + 4×0.5 = 7 + 3 + 10 + 2 = 22
	if !ok || !near(got.Weighted, 22) {
		t.Errorf("got %v; want 22", got.Weighted)
	}
}

func TestWeightOutcomes_NotApplied(t *testing.T) {
	rows := []OutcomeUsage{{Status: "SUCCESS", Quantity: 5, EventCount: 5}}
	w := map[string]float64{"SUCCESS": 0.5}
	for _, agg := range []string{"MAX", "AVG", "UNIQUE", "COUNT_DISTINCT"} {
		if _, ok := WeightOutcomes(rows, w, agg); ok {
			t.Errorf("%s must not be weighted", agg)
		}
	}
	if _, ok := WeightOutcomes(nil, w, "SUM"); ok {
		t.Error("no rows must not be weighted")
	}
	if _, ok := WeightOutcomes(rows, nil, "SUM"); ok {
		t.Error("no weights must not be weighted")
	}
}

func TestCalculate_OutcomeBased(t *testing.T) {
	rate := RateConfig{
		PerUnitRateUSD:    0.02,
		IncludedFreeUnits: 100,
		BlockSizeUnits:    10,
		MinFeeUSD:         5,
		OutcomeWeights:    map[string]float64{"SUCCESS": 1, "PARTIAL": 0.5, "HITL_REQUIRED": 0},
	}
	cases := []struct {
		name     string
		events   int64
		outcomes []OutcomeUsage
		want     float64
	}{
		{
			// weighted = 1234×1 + 100×0.5 + 50×0 + 16×1(no status) = 1234 + 50 + 0 + 16 = 1300
			// billable = 1300 − 100 = 1200 → blocks = ceil(1200/10) = 120 → 1200 × 0.02 = 24.00
			// 24.00 ≥ min fee 5 → 24.00
			name: "exact blocks",
			outcomes: []OutcomeUsage{
				{Status: "SUCCESS", Quantity: 1234}, {Status: "PARTIAL", Quantity: 100},
				{Status: "HITL_REQUIRED", Quantity: 50}, {Status: "", Quantity: 16},
			},
			want: 24.00,
		},
		{
			// weighted = 1205 → billable 1105 → ceil(110.5) = 111 blocks → 1110 × 0.02 = 22.20
			name:     "partial block rounds up",
			outcomes: []OutcomeUsage{{Status: "SUCCESS", Quantity: 1205}},
			want:     22.20,
		},
		{
			// weighted = 100 + 100×0.5 = 150 → billable 50 → 5 blocks → 50 × 0.02 = 1.00 → min fee 5.00
			name:     "min fee floor",
			outcomes: []OutcomeUsage{{Status: "SUCCESS", Quantity: 100}, {Status: "PARTIAL", Quantity: 100}},
			want:     5.00,
		},
		{
			// weighted = 80 → billable 0 → cost 0 → min fee still floors to 5.00 (platform behaviour)
			name:     "all free still pays min fee",
			outcomes: []OutcomeUsage{{Status: "SUCCESS", Quantity: 80}},
			want:     5.00,
		},
		{
			// No outcome rows → billing bills the unweighted total:
			// 1000 − 100 = 900 → 90 blocks → 900 × 0.02 = 18.00
			name:   "no outcome rows bills unweighted",
			events: 1000,
			want:   18.00,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Calculate(CalcInputs{
				Events: tc.events, Outcomes: tc.outcomes, Aggregation: "SUM",
				Model: OutcomeBased, Rate: rate, Mode: Postpaid,
			}, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !near(res.Subtotal, tc.want) {
				t.Errorf("subtotal = %v; want %v", res.Subtotal, tc.want)
			}
		})
	}
}

func TestCalculate_OutcomeBased_FractionalAndMaxAggregation(t *testing.T) {
	rate := RateConfig{PerUnitRateUSD: 0.1, OutcomeWeights: map[string]float64{"PARTIAL": 0.5}}
	res, _ := Calculate(CalcInputs{
		Model: OutcomeBased, Rate: rate, Mode: Postpaid, Aggregation: "SUM",
		Outcomes: []OutcomeUsage{{Status: "SUCCESS", Quantity: 101}, {Status: "PARTIAL", Quantity: 3}},
	}, 0)
	// 101×1 (no SUCCESS weight → 1.0) + 3×0.5 = 102.5 → 102.5 × 0.1 = 10.25
	if !near(res.Subtotal, 10.25) {
		t.Errorf("fractional: %v; want 10.25", res.Subtotal)
	}
	// MAX can't be split by status → unweighted Events: 104 × 0.1 = 10.40
	res, _ = Calculate(CalcInputs{
		Events: 104, Model: OutcomeBased, Rate: rate, Mode: Postpaid, Aggregation: "MAX",
		Outcomes: []OutcomeUsage{{Status: "SUCCESS", Quantity: 101}, {Status: "PARTIAL", Quantity: 3}},
	}, 0)
	if !near(res.Subtotal, 10.40) {
		t.Errorf("MAX: %v; want 10.40", res.Subtotal)
	}
}

func TestExpectedOutcomes_WeightedEstimate(t *testing.T) {
	w := map[string]float64{"SUCCESS": 1, "PARTIAL": 0.5, "HITL_REQUIRED": 0}
	rows := ExpectedOutcomes(1000, map[string]float64{"SUCCESS": 0.9, "PARTIAL": 0.05, "HITL_REQUIRED": 0.05})
	got, ok := WeightOutcomes(rows, w, "COUNT")
	// 1000×0.9×1 + 1000×0.05×0.5 + 1000×0.05×0 = 900 + 25 + 0 = 925
	if !ok || !near(got.Weighted, 925) || !near(got.Unweighted, 1000) {
		t.Errorf("got %+v; want 925 of 1000", got)
	}
	// A mix that covers only 80% leaves 20% on the no-status row (weight 1):
	// 800×0.5 + 200×1 = 400 + 200 = 600
	rows = ExpectedOutcomes(1000, map[string]float64{"SUCCESS": 0.8})
	got, _ = WeightOutcomes(rows, map[string]float64{"SUCCESS": 0.5}, "COUNT")
	if !near(got.Weighted, 600) {
		t.Errorf("partial coverage: %v; want 600", got.Weighted)
	}
	if ExpectedOutcomes(0, map[string]float64{"SUCCESS": 1}) != nil {
		t.Error("zero events → no rows")
	}
}

func TestCalculate_Staircase(t *testing.T) {
	bands := []Band{
		{From: 1, To: 100, Price: 10},
		{From: 101, To: 500, Price: 30},
		{From: 501, To: 0, Price: 50}, // open-ended
	}
	cases := []struct {
		events int64
		free   int64
		want   float64
	}{
		{0, 0, 0},       // nothing billable → $0
		{1, 0, 10},      // 1 ∈ [1,100]
		{100, 0, 10},    // upper bound inclusive
		{101, 0, 30},    // 101 ∈ [101,500]
		{500, 0, 30},    // upper bound inclusive
		{501, 0, 50},    // open-ended band
		{100000, 0, 50}, // still open-ended
		{150, 50, 10},   // billable 150−50 = 100 → [1,100]
		{151, 50, 30},   // billable 101 → [101,500]
		{40, 50, 0},     // billable max(40−50,0) = 0 → $0
	}
	for _, tc := range cases {
		res, err := Calculate(CalcInputs{
			Events: tc.events, Model: Staircase, Mode: Postpaid,
			Rate: RateConfig{StaircaseBands: bands, IncludedFreeUnits: tc.free},
		}, 0)
		if err != nil {
			t.Fatal(err)
		}
		if !near(res.Subtotal, tc.want) {
			t.Errorf("events=%d free=%d: subtotal %v; want %v", tc.events, tc.free, res.Subtotal, tc.want)
		}
	}
}

func TestCalculate_Staircase_AboveBoundedTopAndUnordered(t *testing.T) {
	// Bands given out of order; top band bounded at 200.
	bands := []Band{{From: 101, To: 200, Price: 20}, {From: 1, To: 100, Price: 10}}
	res, _ := Calculate(CalcInputs{Events: 999, Model: Staircase, Mode: Postpaid,
		Rate: RateConfig{StaircaseBands: bands}}, 0)
	// 999 is above every bound → last band (sorted by From) = [101,200] → $20
	if !near(res.Subtotal, 20) {
		t.Errorf("above top: %v; want 20", res.Subtotal)
	}
	res, _ = Calculate(CalcInputs{Events: 50, Model: Staircase, Mode: Postpaid,
		Rate: RateConfig{StaircaseBands: bands}}, 0)
	// 50 ∈ [1,100] → $10 regardless of input order
	if !near(res.Subtotal, 10) {
		t.Errorf("unordered: %v; want 10", res.Subtotal)
	}
	res, _ = Calculate(CalcInputs{Events: 50, Model: Staircase, Mode: Postpaid}, 0)
	// No bands → $0 (platform returns zero rather than guessing a rate)
	if res.Subtotal > eps {
		t.Errorf("no bands: %v; want 0", res.Subtotal)
	}
}

func TestCalculate_PerUnit_BlockAndMinFee(t *testing.T) {
	// PER_UNIT shares PricingCalculator.perUnit with OUTCOME_BASED, so block
	// size and min fee apply to it too.
	res, _ := Calculate(CalcInputs{Events: 1001, Model: PerUnit, Mode: Postpaid,
		Rate: RateConfig{PerUnitRateUSD: 0.01, BlockSizeUnits: 100}}, 0)
	// ceil(1001/100) = 11 blocks → 1100 × 0.01 = 11.00
	if !near(res.Subtotal, 11) {
		t.Errorf("block: %v; want 11", res.Subtotal)
	}
	res, _ = Calculate(CalcInputs{Events: 1001, Model: PerUnit, Mode: Postpaid,
		Rate: RateConfig{PerUnitRateUSD: 0.01, BlockSizeUnits: 100, MinFeeUSD: 20}}, 0)
	// 11.00 < min fee 20 → 20.00
	if !near(res.Subtotal, 20) {
		t.Errorf("min fee: %v; want 20", res.Subtotal)
	}
}

// TestLinearCharge_BlockCeilingAtSixDecimals: billing ceils the exact
// 6-dp weighted units. 2000.000001 units / block 1000 = 2.000000001 → 3
// blocks → 3000 × $0.01 = $30. Rounding the float QUOTIENT to 6 dp (the
// old approach) gave 2 blocks = $20.
func TestLinearCharge_BlockCeilingAtSixDecimals(t *testing.T) {
	r := RateConfig{PerUnitRateUSD: 0.01, BlockSizeUnits: 1000}
	cases := []struct {
		units float64
		want  float64
	}{
		{2000.000001, 30.00},
		{2000, 20.00},
		{2000.0000000001, 20.00}, // float noise below billing's 6-dp scale
		{1, 10.00},
		{999.5, 10.00},
	}
	for _, tc := range cases {
		if got := linearCharge(tc.units, r); got != tc.want {
			t.Errorf("linearCharge(%v) = %v, want %v", tc.units, got, tc.want)
		}
	}
}

// TestCalculate_NoUsageNoMinFee: billing writes no line for a metric with no
// usage in the period (BillRunServiceImpl only synthesises FLAT_RATE), so a
// PER_UNIT / OUTCOME_BASED min fee is NOT charged at zero events. Usage that
// weighs to 0 units is still usage — the min fee applies.
func TestCalculate_NoUsageNoMinFee(t *testing.T) {
	rate := RateConfig{PerUnitRateUSD: 0.01, MinFeeUSD: 5, OutcomeWeights: map[string]float64{"ERROR": 0}}
	for _, m := range []PricingModel{PerUnit, OutcomeBased} {
		res, _ := Calculate(CalcInputs{Events: 0, Model: m, Mode: Postpaid, Rate: rate}, 0)
		if res.Subtotal != 0 {
			t.Errorf("%s with no usage = %v; want 0 (no line, no min fee)", m, res.Subtotal)
		}
	}
	// 10 ERROR events weigh 0 → 0 units billed → min fee $5.00.
	res, _ := Calculate(CalcInputs{Model: OutcomeBased, Mode: Postpaid, Rate: rate,
		Outcomes: []OutcomeUsage{{Status: "ERROR", Quantity: 10, EventCount: 10}}, Aggregation: "COUNT"}, 0)
	if res.Subtotal != 5 {
		t.Errorf("all-zero-weight usage = %v; want 5.00 (min fee)", res.Subtotal)
	}
}
