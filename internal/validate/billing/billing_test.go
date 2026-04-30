package billing

import (
	"math"
	"testing"
)

// approxEqual returns true if |a−b| ≤ tol. Used everywhere because the
// production service uses BigDecimal HALF_EVEN at scale=2 and we use
// double — sub-cent noise is allowed.
func approxEqual(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestPerUnit_Golden(t *testing.T) {
	cases := []struct {
		name   string
		events int64
		rate   RateConfig
		want   float64
	}{
		{"basic", 100, RateConfig{PerUnitRateUSD: 0.001}, 0.10},
		{"with-included-free", 1500, RateConfig{PerUnitRateUSD: 0.001, IncludedFreeUnits: 1000}, 0.50},
		{"included-eats-everything", 800, RateConfig{PerUnitRateUSD: 0.001, IncludedFreeUnits: 1000}, 0.0},
		{"zero-events", 0, RateConfig{PerUnitRateUSD: 0.005}, 0.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := perUnitCharge(tc.events, tc.rate)
			if !approxEqual(got, tc.want, 1e-9) {
				t.Fatalf("got %.6f want %.6f", got, tc.want)
			}
		})
	}
}

func TestFlatRate_Golden(t *testing.T) {
	got := flatRateCharge(RateConfig{FlatFeeUSD: 99.00})
	if got != 99.00 {
		t.Fatalf("flat rate must return FlatFeeUSD verbatim, got %.4f", got)
	}
	if flatRateCharge(RateConfig{}) != 0 {
		t.Fatal("zero config must yield zero charge")
	}
}

func TestPercentage_Golden(t *testing.T) {
	cases := []struct {
		name string
		base float64
		rate RateConfig
		want float64
	}{
		{"above-min", 1000, RateConfig{PercentageRate: 0.025, MinFeeUSD: 5}, 25.0},
		{"below-min-clamp", 100, RateConfig{PercentageRate: 0.025, MinFeeUSD: 10}, 10.0},
		{"zero-base", 0, RateConfig{PercentageRate: 0.025, MinFeeUSD: 5}, 5.0},
		{"negative-base-clamps-to-min", -100, RateConfig{PercentageRate: 0.025, MinFeeUSD: 5}, 5.0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := percentageCharge(tc.base, tc.rate)
			if !approxEqual(got, tc.want, 1e-9) {
				t.Fatalf("got %.6f want %.6f", got, tc.want)
			}
		})
	}
}

func TestIncludedQuota_Golden(t *testing.T) {
	cases := []struct {
		name   string
		events int64
		rate   RateConfig
		want   float64
	}{
		{"under-quota", 800, RateConfig{IncludedFreeUnits: 1000, PerUnitRateUSD: 0.001}, 0},
		{"slight-overage", 1500, RateConfig{IncludedFreeUnits: 1000, PerUnitRateUSD: 0.001}, 0.50},
		{"block-pricing-partial-block-ceils", 1100, RateConfig{IncludedFreeUnits: 1000, PerUnitRateUSD: 0.001, BlockSizeUnits: 100}, 0.10},
		{"block-pricing-mid-block-ceils-up", 1150, RateConfig{IncludedFreeUnits: 1000, PerUnitRateUSD: 0.001, BlockSizeUnits: 100}, 0.20},
		{"block-pricing-exact-block", 1200, RateConfig{IncludedFreeUnits: 1000, PerUnitRateUSD: 0.001, BlockSizeUnits: 100}, 0.20},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := includedQuotaCharge(tc.events, tc.rate)
			if !approxEqual(got, tc.want, 1e-9) {
				t.Fatalf("got %.6f want %.6f", got, tc.want)
			}
		})
	}
}

func TestGraduated_Golden(t *testing.T) {
	tiers := []Tier{
		{UpToUnits: 1000, UnitPrice: 0.001},
		{UpToUnits: 10_000, UnitPrice: 0.0008},
		{UpToUnits: math.MaxInt64, UnitPrice: 0.0005},
	}
	cases := []struct {
		name   string
		events int64
		want   float64
	}{
		{"first-tier-only", 500, 0.50},
		{"spans-two-tiers", 5_000, 1.00 + 4_000*0.0008}, // 1.00 + 3.20 = 4.20
		{"spans-all-three", 15_000, 1.00 + 9_000*0.0008 + 5_000*0.0005},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, br := graduatedCharge(tc.events, tiers)
			if !approxEqual(got, tc.want, 1e-9) {
				t.Fatalf("got %.6f want %.6f", got, tc.want)
			}
			// breakdown sum must equal total
			sum := 0.0
			for _, b := range br {
				sum += b.Charge
			}
			if !approxEqual(sum, got, 1e-9) {
				t.Fatalf("breakdown sum %.6f != total %.6f", sum, got)
			}
		})
	}
}

func TestVolumeTiered_Golden(t *testing.T) {
	tiers := []Tier{
		{UpToUnits: 1000, UnitPrice: 0.001},
		{UpToUnits: 10_000, UnitPrice: 0.0008},
		{UpToUnits: math.MaxInt64, UnitPrice: 0.0005},
	}
	cases := []struct {
		name   string
		events int64
		want   float64
	}{
		{"lands-in-first-tier", 500, 0.50},                  // 500 × 0.001
		{"lands-in-second-tier", 5_000, 5_000 * 0.0008},     // entire volume at 0.0008
		{"lands-in-third-tier", 15_000, 15_000 * 0.0005},    // entire volume at 0.0005
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := volumeTieredCharge(tc.events, tiers)
			if !approxEqual(got, tc.want, 1e-9) {
				t.Fatalf("got %.6f want %.6f", got, tc.want)
			}
		})
	}
}

func TestDiscount_BoundedBySubtotal(t *testing.T) {
	if got := applyDiscount(100, &Discount{Type: "PERCENTAGE", Value: 0.10}); got != 10 {
		t.Fatalf("10%% of 100 = 10, got %.2f", got)
	}
	if got := applyDiscount(100, &Discount{Type: "FIXED_AMOUNT", Value: 250}); got != 100 {
		t.Fatalf("fixed > subtotal must clamp at subtotal, got %.2f", got)
	}
	if got := applyDiscount(100, &Discount{Type: "FIXED_AMOUNT", Value: -10}); got != 0 {
		t.Fatalf("negative discount must clamp at 0, got %.2f", got)
	}
	if got := applyDiscount(100, nil); got != 0 {
		t.Fatalf("nil discount must be 0, got %.2f", got)
	}
}

func TestRoute_Postpaid(t *testing.T) {
	w, i := route(99.00, Postpaid, 500)
	if w != 0 || i != 99.00 {
		t.Fatalf("postpaid: wallet=0 invoice=99 expected, got w=%.2f i=%.2f", w, i)
	}
}

func TestRoute_Prepaid(t *testing.T) {
	w, i := route(99.00, Prepaid, 500)
	if w != 99.00 || i != 0 {
		t.Fatalf("prepaid: wallet=99 invoice=0 expected, got w=%.2f i=%.2f", w, i)
	}
}

func TestRoute_Hybrid_Split(t *testing.T) {
	// wallet covers part, invoice covers rest
	w, i := route(100, Hybrid, 30)
	if w != 30 || i != 70 {
		t.Fatalf("hybrid split: w=30 i=70 expected, got w=%.2f i=%.2f", w, i)
	}
}

func TestRoute_Hybrid_FullyCoveredByWallet(t *testing.T) {
	w, i := route(100, Hybrid, 200)
	if w != 100 || i != 0 {
		t.Fatalf("hybrid fully covered: w=100 i=0 expected, got w=%.2f i=%.2f", w, i)
	}
}

func TestCalculate_FullPipeline_PostpaidPerUnit(t *testing.T) {
	r, err := Calculate(CalcInputs{
		Events: 10_000,
		Model:  PerUnit,
		Mode:   Postpaid,
		Rate:   RateConfig{PerUnitRateUSD: 0.001},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !approxEqual(r.Total, 10.00, 1e-9) {
		t.Fatalf("total = %.4f want 10.00", r.Total)
	}
	if r.WalletDebit != 0 || r.InvoiceUSD != 10.00 {
		t.Fatalf("postpaid routing wrong: w=%.2f i=%.2f", r.WalletDebit, r.InvoiceUSD)
	}
}

func TestCalculate_FullPipeline_HybridGraduated_WithDiscount(t *testing.T) {
	r, err := Calculate(CalcInputs{
		Events: 5_000,
		Model:  Graduated,
		Mode:   Hybrid,
		Rate: RateConfig{
			GraduatedTiers: []Tier{
				{UpToUnits: 1000, UnitPrice: 0.001},
				{UpToUnits: 10_000, UnitPrice: 0.0008},
			},
		},
		Discount: &Discount{Type: "PERCENTAGE", Value: 0.10},
	}, 2.00)
	if err != nil {
		t.Fatal(err)
	}
	// Subtotal = 1.00 + 3.20 = 4.20; discount = 0.42; total = 3.78
	wantSubtotal := 4.20
	if !approxEqual(r.Subtotal, wantSubtotal, 0.01) {
		t.Fatalf("subtotal = %.4f want %.4f", r.Subtotal, wantSubtotal)
	}
	wantTotal := 3.78
	if !approxEqual(r.Total, wantTotal, 0.01) {
		t.Fatalf("total = %.4f want %.4f", r.Total, wantTotal)
	}
	// Hybrid split must add up to total within a cent
	if !approxEqual(r.WalletDebit+r.InvoiceUSD, r.Total, 0.01) {
		t.Fatalf("hybrid split sum %.4f != total %.4f", r.WalletDebit+r.InvoiceUSD, r.Total)
	}
}
