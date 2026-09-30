package seed

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// pricingGraduated / pricingVolume are line-for-line ports of aforo-common
// PricingCalculator.graduated / volumeTiered over the tiers loadgen sends:
// tiers are 1-based inclusive [tierStart, tierEnd]; tierEnd nil = open;
// a tierEnd below tierStart is skipped (graduated) / never matches (volume).
func pricingGraduated(tiers []rateTierRequest, units int64) float64 {
	remaining := units
	total := 0.0
	for _, t := range tiers { // already ascending by tierStart
		if remaining <= 0 {
			break
		}
		if t.TierEnd != nil && *t.TierEnd < t.TierStart {
			continue
		}
		size := remaining
		if t.TierEnd != nil {
			size = *t.TierEnd - t.TierStart + 1
		}
		used := remaining
		if size < used {
			used = size
		}
		total += float64(used) * t.UnitPrice
		remaining -= used
	}
	return math.Round(total*100) / 100
}

func pricingVolume(tiers []rateTierRequest, units int64) float64 {
	var matched *rateTierRequest
	for i := range tiers {
		t := &tiers[i]
		if t.TierEnd == nil {
			if units >= t.TierStart {
				matched = t
			}
		} else if units >= t.TierStart && units <= *t.TierEnd {
			matched = t
		}
	}
	if matched == nil {
		matched = &tiers[len(tiers)-1]
	}
	return math.Round(float64(units)*matched.UnitPrice*100) / 100
}

var threeBands = []scenario.TierBand{
	{UpToUnits: 1000, UnitPriceUSD: 0.010},
	{UpToUnits: 10000, UnitPriceUSD: 0.007},
	{UpToUnits: 0, UnitPriceUSD: 0.005},
}

// TestTiersFromBands_GraduatedBillsLikeScenario: 15,000 units over
// [≤1000 @ .010, ≤10000 @ .007, rest @ .005] =
// 1000×.010 + 9000×.007 + 5000×.005 = 10 + 63 + 25 = $98.00.
// The old 0-based / tierEnd=-1 encoding billed 1001×.010 + 9001×.007 =
// $73.02 and left the top 4,998 units free.
func TestTiersFromBands_GraduatedBillsLikeScenario(t *testing.T) {
	tiers := tiersFromBands(threeBands)
	cases := map[int64]float64{
		1:     0.01,
		1000:  10.00,
		1001:  10.01, // first unit of band 2: 10 + .007 → 10.007 → 10.01
		10000: 73.00, // 10 + 9000×.007
		10002: 73.01, // + 2×.005
		15000: 98.00,
	}
	for units, want := range cases {
		if got := pricingGraduated(tiers, units); math.Abs(got-want) > 1e-9 {
			t.Errorf("graduated(%d) = %.4f, want %.2f (tiers %+v)", units, got, want, tiers)
		}
	}
}

// TestTiersFromBands_VolumeBoundaries: volume 1000 is band 1 (1000×.010 =
// $10), 1001 is band 2 (1001×.007 = $7.007 → $7.01), 10001 is the open band
// (10001×.005 = $50.005 → rounding-dependent, so 20000 is used: $100).
func TestTiersFromBands_VolumeBoundaries(t *testing.T) {
	tiers := tiersFromBands(threeBands)
	cases := map[int64]float64{1000: 10.00, 1001: 7.01, 10000: 70.00, 20000: 100.00}
	for units, want := range cases {
		if got := pricingVolume(tiers, units); math.Abs(got-want) > 1e-9 {
			t.Errorf("volume(%d) = %.4f, want %.2f", units, got, want)
		}
	}
}

// TestTiersFromBands_WireShape locks the JSON pricing receives.
func TestTiersFromBands_WireShape(t *testing.T) {
	raw, err := json.Marshal(tiersFromBands(threeBands))
	if err != nil {
		t.Fatal(err)
	}
	want := `[{"tierStart":1,"tierEnd":1000,"unitPrice":0.01,"sortOrder":0},` +
		`{"tierStart":1001,"tierEnd":10000,"unitPrice":0.007,"sortOrder":1},` +
		`{"tierStart":10001,"tierEnd":null,"unitPrice":0.005,"sortOrder":2}]`
	if string(raw) != want {
		t.Errorf("wire =\n%s\nwant\n%s", raw, want)
	}
	if strings.Contains(string(raw), "-1") {
		t.Error("open-ended tier must be null, never -1")
	}
}
