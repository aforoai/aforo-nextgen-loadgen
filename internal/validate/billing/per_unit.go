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
		cost = ceilBlocks(billable, r.BlockSizeUnits) * block * r.PerUnitRateUSD
	} else {
		cost = billable * r.PerUnitRateUSD
	}
	cost = roundCents(cost)
	if r.MinFeeUSD > 0 && cost < roundCents(r.MinFeeUSD) {
		cost = roundCents(r.MinFeeUSD)
	}
	return cost
}

// ceilBlocks is ceil(billable / blockSize) with billable taken at billing's
// 6-dp scale (OutcomeWeighting's weighted units are 6-dp decimals and
// PricingCalculator divides the exact decimal with RoundingMode.CEILING).
// The division is done in integer micro-units: a float quotient rounded to
// 6 dp — the previous approach — loses the fractional unit whenever
// billable/blockSize has more than 6 significant decimals, e.g.
// 2000.000001 / 1000 = 2.000000001 → 2.000000 → 2 blocks, where billing
// bills 3. Falls back to a float ceil only when billable is too large for
// int64 micro-units (> ~9.2e12 units), where 6-dp fractions are below
// float64 resolution anyway.
func ceilBlocks(billable float64, blockSize int64) float64 {
	const maxMicros = float64(math.MaxInt64) / 2
	micros := math.RoundToEven(billable * 1e6)
	blockMicros := float64(blockSize) * 1e6
	if micros >= maxMicros || blockMicros >= maxMicros {
		return math.Ceil(round6(billable) / float64(blockSize))
	}
	bm, blk := int64(micros), int64(blockMicros)
	q := bm / blk
	if bm%blk != 0 {
		q++
	}
	return float64(q)
}

// round6 rounds half-even to 6 decimal places — billing's OutcomeWeighting
// scale.
func round6(x float64) float64 {
	return math.RoundToEven(x*1e6) / 1e6
}
