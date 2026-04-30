package billing

import "math"

// RouteCheck mirrors the platform's RouteStage. Constructed from
// (Total, Mode, WalletAvailable). Run() fills WalletDebit + InvoiceUSD.
//
// Invariant (HYBRID):
//
//	WalletDebit + InvoiceUSD = Total
//	WalletDebit ≤ WalletAvailable
//	InvoiceUSD  ≥ 0
//
// PREPAID with insufficient wallet funds is the platform's SUSPEND surface
// — the validator does NOT model that here. RouteStage's job is the split;
// suspend logic is upstream of the calculator.
type RouteCheck struct {
	Total           float64
	Mode            BillingMode
	WalletAvailable float64

	WalletDebit float64
	InvoiceUSD  float64
}

// Run computes the split.
func (r RouteCheck) Run() RouteCheck {
	r.WalletDebit, r.InvoiceUSD = route(r.Total, r.Mode, r.WalletAvailable)
	return r
}

// IsValid asserts the routing invariants per mode.
func (r RouteCheck) IsValid() bool {
	const eps = 1e-6
	if r.WalletDebit < -eps || r.InvoiceUSD < -eps {
		return false
	}
	sum := r.WalletDebit + r.InvoiceUSD
	if math.Abs(sum-r.Total) > eps {
		return false
	}
	switch r.Mode {
	case Postpaid:
		return r.WalletDebit < eps
	case Prepaid:
		return r.InvoiceUSD < eps
	case Hybrid:
		// wallet portion must not exceed available funds (within 1¢)
		return r.WalletDebit <= r.WalletAvailable+0.01
	default:
		return false
	}
}
