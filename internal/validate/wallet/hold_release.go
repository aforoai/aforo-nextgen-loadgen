package wallet

// HoldLifecycle is the per-hold lifecycle observed during a run. The
// validator collects one of these per (subscription × hold) and asserts
// invariants:
//
//	1. Created < Released or Created < Settled (terminal state, no orphans)
//	2. Settled holds have Settled ≥ amountActuallyDebited
//	3. Released holds returned the full hold amount to balance
//
// The platform's HoldExpiryScheduler runs every minute and releases
// pending-but-unsettled holds that have aged past hold_ttl_seconds. A
// run that ends inside the TTL window MUST be validated AFTER the
// scheduler has had a chance to run — the validator should not race the
// scheduler.
type HoldLifecycle struct {
	HoldID            string
	SubscriptionID    string
	CustomerID        string
	HoldUSD           float64
	SettledUSD        float64
	ReleasedUSD       float64
	State             HoldState
	CreatedUnixMillis int64
	TerminalUnixMs    int64
}

// HoldState mirrors the platform's wallet_holds.state enum.
type HoldState string

const (
	HoldPending  HoldState = "PENDING"
	HoldSettled  HoldState = "SETTLED"
	HoldReleased HoldState = "RELEASED"
	HoldExpired  HoldState = "EXPIRED"
)

// IsTerminal returns true when the hold reached a terminal state.
func (h HoldLifecycle) IsTerminal() bool {
	switch h.State {
	case HoldSettled, HoldReleased, HoldExpired:
		return true
	}
	return false
}

// IsValid asserts the lifecycle invariants on a settled run:
//
//   - Pending holds at run-end are a regression IF the validation window
//     includes the scheduler-tick deadline.
//   - Settled holds: SettledUSD ≤ HoldUSD (settlement never exceeds the
//     authorized hold).
//   - Released/Expired holds: ReleasedUSD must equal HoldUSD (full return).
//
// Tolerance is in dollars.
func (h HoldLifecycle) IsValid(tolerance float64) bool {
	if !h.IsTerminal() {
		return false
	}
	if h.State == HoldSettled {
		return h.SettledUSD <= h.HoldUSD+tolerance
	}
	delta := h.ReleasedUSD - h.HoldUSD
	if delta < 0 {
		delta = -delta
	}
	return delta <= tolerance
}
