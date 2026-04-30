package billing

// TaxCheck is a small named alias for validating the TaxStage in isolation.
// The platform's TaxStage today is a placeholder (taxAmount = 0) when
// engine=mock and a real Avalara/Vertex call otherwise. The validator's
// model uses the supplied TaxPct verbatim — driven by scenario.tax.
type TaxCheck struct {
	Taxable   float64
	TaxPct    float64
	TaxAmount float64
	Total     float64
}

// Run computes TaxAmount = Taxable × TaxPct, rounds to cents, and adds back.
// Negative pct or taxable values clamp to zero (defensive — should never
// happen in a valid scenario).
func (t TaxCheck) Run() TaxCheck {
	if t.Taxable < 0 {
		t.Taxable = 0
	}
	if t.TaxPct < 0 {
		t.TaxPct = 0
	}
	t.TaxAmount = roundCents(t.Taxable * t.TaxPct)
	t.Total = t.Taxable + t.TaxAmount
	return t
}

// IsValid asserts the TaxStage invariants:
//
//	TaxAmount ≥ 0
//	Total = Taxable + TaxAmount (within 1¢)
//
// 1¢ tolerance covers HALF_EVEN rounding near tier transitions.
func (t TaxCheck) IsValid() bool {
	if t.TaxAmount < 0 {
		return false
	}
	delta := t.Total - (t.Taxable + t.TaxAmount)
	if delta < 0 {
		delta = -delta
	}
	return delta < 0.01
}
