package billing

import "math"

// percentageCharge implements PERCENTAGE:
//
//	raw_charge = base × rate
//	charge     = max(raw_charge, min_fee)
//
// Used for marketplaces / payment processing where Aforo bills a percent
// of the customer's processed volume.
//
// rate is a fraction (0.025 = 2.5%), not a percent string. Negative bases
// or rates clamp to zero — the platform does the same.
func percentageCharge(base float64, r RateConfig) float64 {
	if base < 0 || r.PercentageRate < 0 {
		return math.Max(0, r.MinFeeUSD)
	}
	raw := base * r.PercentageRate
	if raw < r.MinFeeUSD {
		return r.MinFeeUSD
	}
	return raw
}
