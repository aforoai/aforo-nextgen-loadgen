package billing

import "math"

// perUnitCharge implements PER_UNIT on a whole-event count. See linearCharge.
func perUnitCharge(events int64, r RateConfig) float64 {
	return linearCharge(float64(events), r)
}

// linearCharge is PricingCalculator.perUnit (aforo-common), shared by
// PER_UNIT and OUTCOME_BASED (whose units arrive pre-weighted):
//
//	billable = max(units − included_free, 0)
//	if block_size > 0 and billable > 0:
//	    cost = ceil(billable / block_size) × block_size × rate
//	else:
//	    cost = billable × rate
//	cost = round_cents(cost); if min_fee > 0: cost = max(cost, min_fee)
//
// The min fee floors the cost even when nothing is billable, exactly like
// the platform. Negative units (correction entries) are billed as
// units × rate with no allowance, block or floor — the platform's refund
// branch; loadgen never generates them but the model stays faithful.
func linearCharge(units float64, r RateConfig) float64 {
	if units < 0 {
		return roundCents(units * r.PerUnitRateUSD)
	}
	billable := units - float64(r.IncludedFreeUnits)
	if billable < 0 {
		billable = 0
	}
	var cost float64
	if r.BlockSizeUnits > 0 && billable > 0 {
		block := float64(r.BlockSizeUnits)
		// Round to 6 dp before ceil so float noise (e.g. 2000.0000000001)
		// can't bill an extra block. Weighted units are 6-dp values already.
		blocks := math.Ceil(round6(billable / block))
		cost = blocks * block * r.PerUnitRateUSD
	} else {
		cost = billable * r.PerUnitRateUSD
	}
	cost = roundCents(cost)
	if r.MinFeeUSD > 0 && cost < roundCents(r.MinFeeUSD) {
		cost = roundCents(r.MinFeeUSD)
	}
	return cost
}

// round6 rounds half-even to 6 decimal places — billing's OutcomeWeighting
// scale.
func round6(x float64) float64 {
	return math.RoundToEven(x*1e6) / 1e6
}
