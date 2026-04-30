package payments

import (
	"fmt"
	"math/rand"
	"sync"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// OutcomePicker decides what outcome (success / decline / insufficient
// funds) each invoice should attempt, weighted by the scenario's payments
// mix. Determinism: the picker is seeded by scenario.seed so a re-run of
// the same scenario picks the same outcomes for the same inputs.
//
// Concurrency: safe for many goroutines.
type OutcomePicker struct {
	mu       sync.Mutex
	rng      *rand.Rand
	successW float64
	declineW float64
	insufW   float64
	totalW   float64
	pinByID  map[string]PaymentOutcome // optional caller-supplied overrides
}

// NewOutcomePicker constructs the picker from a scenario.Payments block.
// Sums the three weights; if all three are zero, the runner default is
// 100% success (no scenario authored a mix).
func NewOutcomePicker(p scenario.Payments, seed int64) *OutcomePicker {
	successW := p.SuccessPct
	declineW := p.DeclinePct
	insufW := p.InsufficientFundsPct
	total := successW + declineW + insufW
	if total == 0 {
		successW = 1.0
		total = 1.0
	}
	return &OutcomePicker{
		rng:      rand.New(rand.NewSource(seed)),
		successW: successW,
		declineW: declineW,
		insufW:   insufW,
		totalW:   total,
		pinByID:  map[string]PaymentOutcome{},
	}
}

// Pin forces a specific outcome for an invoice id — used by deterministic
// tests that need exact decline-then-recover sequences. Calling Pin
// overrides any subsequent Pick(invoiceID) result.
func (op *OutcomePicker) Pin(invoiceID string, outcome PaymentOutcome) {
	op.mu.Lock()
	op.pinByID[invoiceID] = outcome
	op.mu.Unlock()
}

// Pick returns the outcome the driver should attempt against invoiceID.
// Pinned overrides win; otherwise the weighted RNG decides.
func (op *OutcomePicker) Pick(invoiceID string) PaymentOutcome {
	op.mu.Lock()
	defer op.mu.Unlock()
	if pinned, ok := op.pinByID[invoiceID]; ok {
		return pinned
	}
	r := op.rng.Float64() * op.totalW
	if r < op.successW {
		return OutcomeSucceeded
	}
	if r < op.successW+op.declineW {
		return OutcomeDeclined
	}
	return OutcomeInsufficientFunds
}

// CardFor returns the Stripe test card number that produces the given outcome
// from Stripe's test environment. Used by the driver when constructing the
// payment intent.
func CardFor(outcome PaymentOutcome) (string, error) {
	switch outcome {
	case OutcomeSucceeded:
		return TestCardSuccess, nil
	case OutcomeDeclined:
		return TestCardDeclineGeneric, nil
	case OutcomeInsufficientFunds:
		return TestCardDeclineInsufFunds, nil
	case OutcomeRequiresAction:
		return TestCardRequires3DS, nil
	}
	return "", fmt.Errorf("payments: no test card maps to outcome %q", outcome)
}

// Distribution is the realized mix from a sequence of Pick calls — used by
// tests + Check 12 to assert that the actual distribution sits within
// tolerance of the requested mix.
type Distribution struct {
	Success int
	Decline int
	Insuf   int
	Other   int
	Total   int
}

// SuccessPct returns the realized success rate.
func (d Distribution) SuccessPct() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.Success) / float64(d.Total)
}

// DeclinePct returns the realized decline rate (incl. insufficient).
func (d Distribution) DeclinePct() float64 {
	if d.Total == 0 {
		return 0
	}
	return float64(d.Decline+d.Insuf) / float64(d.Total)
}

// Tally counts an outcome into the distribution.
func (d *Distribution) Tally(o PaymentOutcome) {
	d.Total++
	switch o {
	case OutcomeSucceeded:
		d.Success++
	case OutcomeDeclined:
		d.Decline++
	case OutcomeInsufficientFunds:
		d.Insuf++
	default:
		d.Other++
	}
}
