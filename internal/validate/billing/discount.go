package billing

// DiscountCheck is a small named alias the orchestrator uses to surface
// the DiscountStage rule independently of the full pipeline. It restates
// applyDiscount's contract for callers that want to validate a single
// (subtotal, discount) pair.
//
// DiscountStage rule: discount cannot exceed subtotal. The platform clamps
// at subtotal — never returns a negative invoice line. We mirror that.
type DiscountCheck struct {
	Subtotal    float64
	Discount    *Discount
	AppliedAmt  float64
	NetSubtotal float64
}

// Run computes the applied discount amount and the resulting net subtotal.
// Discount is nil-safe: a nil discount is treated as zero.
func (d DiscountCheck) Run() DiscountCheck {
	d.AppliedAmt = applyDiscount(d.Subtotal, d.Discount)
	d.NetSubtotal = d.Subtotal - d.AppliedAmt
	return d
}

// IsValid asserts the DiscountStage invariants:
//
//	0 ≤ AppliedAmt ≤ Subtotal
//	NetSubtotal ≥ 0
//
// Used by the property-based test to catch a negative-discount or
// over-discount regression.
func (d DiscountCheck) IsValid() bool {
	return d.AppliedAmt >= 0 &&
		d.AppliedAmt <= d.Subtotal+1e-9 &&
		d.NetSubtotal >= -1e-9
}
