package billing

import (
	"math"
	"sort"
)

// Band is one STAIRCASE step. From/To are inclusive, 1-based (pricing's
// Option B encoding: {1,100} is units 1..100). To == 0 means open-ended.
// Price is the flat price billed when usage lands in the band.
type Band struct {
	From  int64
	To    int64
	Price float64
}

// staircaseCharge mirrors PricingCalculator.staircase (aforo-common):
//
//	billable = max(units − included_free, 0)
//	billable == 0 or no bands → $0
//	charge = price of the band with From ≤ billable ≤ To (To == 0: From ≤ billable)
//	billable above every bound → the last band's price
//
// Bands are matched in ascending From order.
func staircaseCharge(units float64, includedFree int64, raw []Band) (float64, []TierCharge) {
	billable := units - float64(includedFree)
	if billable < 0 {
		billable = 0
	}
	if billable == 0 || len(raw) == 0 {
		return 0, nil
	}
	bands := make([]Band, len(raw))
	copy(bands, raw)
	sort.SliceStable(bands, func(i, j int) bool { return bands[i].From < bands[j].From })

	matched := -1
	for i, b := range bands {
		from := float64(b.From)
		if b.To == 0 {
			if billable >= from {
				matched = i
				break
			}
			continue
		}
		if billable >= from && billable <= float64(b.To) {
			matched = i
			break
		}
	}
	if matched < 0 {
		matched = len(bands) - 1
	}
	b := bands[matched]
	price := roundCents(b.Price)
	to := b.To
	if to == 0 {
		to = math.MaxInt64
	}
	return price, []TierCharge{{
		From:    b.From,
		To:      to,
		Units:   int64(billable),
		FlatFee: price,
		Charge:  price,
	}}
}
