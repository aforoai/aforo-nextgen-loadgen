package billing

// includedQuotaCharge implements INCLUDED_QUOTA — a quota of free events
// followed by per-unit overage, optionally rounded up to a block.
//
//	overage_units = max(events − included_free, 0)
//	if block_size > 0:
//	    overage_blocks = ceil(overage_units / block_size)
//	    charge         = overage_blocks × block_size × rate
//	else:
//	    charge         = overage_units × rate
//
// Block pricing is the "$X per 1,000 calls beyond your quota" pattern —
// the customer is billed for the whole block even if they used part of it.
func includedQuotaCharge(events int64, r RateConfig) float64 {
	overage := events - r.IncludedFreeUnits
	if overage <= 0 {
		return 0
	}
	if r.BlockSizeUnits > 0 {
		blocks := overage / r.BlockSizeUnits
		if overage%r.BlockSizeUnits != 0 {
			blocks++
		}
		return float64(blocks) * float64(r.BlockSizeUnits) * r.PerUnitRateUSD
	}
	return float64(overage) * r.PerUnitRateUSD
}
