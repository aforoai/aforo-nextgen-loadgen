package billing

// perUnitCharge implements PER_UNIT:
//
//	billable_units = max(events − included_free, 0)
//	charge         = billable_units × rate
//
// IncludedFreeUnits at zero degenerates to events × rate. Negative results
// are impossible (the max clamp), so this never returns a negative.
func perUnitCharge(events int64, r RateConfig) float64 {
	billable := events - r.IncludedFreeUnits
	if billable < 0 {
		billable = 0
	}
	return float64(billable) * r.PerUnitRateUSD
}
