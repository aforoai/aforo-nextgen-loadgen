// Package wallet validates wallet-side billing-pipeline outcomes — the
// hold-release lifecycle and the pre/post balance arithmetic for PREPAID
// + HYBRID flows.
//
// All functions here are pure. The Validator orchestrator pulls live state
// from the BackendClient and feeds it in.
package wallet

// BalanceCheck verifies the wallet ledger arithmetic for a settled run:
//
//	post_balance = pre_balance − wallet_debits + holds_released_back
//
// where holds_released_back covers expired holds that returned to balance
// (e.g. an over-estimated escrow). The platform's HoldExpiryScheduler
// releases unused holds on a 1-minute cadence — the validator's window
// must extend past run.stopped_at by that interval.
type BalanceCheck struct {
	PreBalance        float64
	PostBalance       float64
	WalletDebits      float64
	HoldsReleasedBack float64
}

// Computed returns the expected post-balance from the inputs.
func (b BalanceCheck) Computed() float64 {
	return b.PreBalance - b.WalletDebits + b.HoldsReleasedBack
}

// IsValid asserts |Computed - PostBalance| ≤ tolerance. Tolerance is in
// dollars; 0.01 = one cent — the rounding noise of double arithmetic.
func (b BalanceCheck) IsValid(tolerance float64) bool {
	delta := b.Computed() - b.PostBalance
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}
