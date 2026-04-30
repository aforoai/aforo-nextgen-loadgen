package billing

// flatRateCharge is the platform's FLAT_RATE: a fixed period fee that does
// not depend on usage at all. Returns FlatFeeUSD verbatim.
func flatRateCharge(r RateConfig) float64 {
	return r.FlatFeeUSD
}
