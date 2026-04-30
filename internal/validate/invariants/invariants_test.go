package invariants

import (
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate/billing"
)

func TestRun_DeterministicWithSameSeed(t *testing.T) {
	r1 := Run(FuzzConfig{Seed: 42, Trials: 100})
	r2 := Run(FuzzConfig{Seed: 42, Trials: 100})
	if r1.Trials != r2.Trials {
		t.Fatalf("trial counts diverged: %d vs %d", r1.Trials, r2.Trials)
	}
	if len(r1.Violations) != len(r2.Violations) {
		t.Fatalf("violation counts diverged: %d vs %d", len(r1.Violations), len(r2.Violations))
	}
	for i := range r1.Violations {
		if r1.Violations[i].Invariant != r2.Violations[i].Invariant {
			t.Fatalf("violation %d invariant diverged: %s vs %s",
				i, r1.Violations[i].Invariant, r2.Violations[i].Invariant)
		}
	}
}

func TestRun_HappyPath_NoViolations(t *testing.T) {
	// Seed=1 with 200 trials. The math we wrote satisfies every invariant
	// on every randomly generated sample — if a future change breaks one,
	// this test catches it.
	r := Run(FuzzConfig{Seed: 1, Trials: 200})
	if len(r.Violations) != 0 {
		for _, v := range r.Violations[:min(5, len(r.Violations))] {
			t.Logf("violation: %s — %s", v.Invariant, v.Message)
		}
		t.Fatalf("expected 0 violations, got %d", len(r.Violations))
	}
}

func TestRun_DeliberateInvariantViolation_Caught(t *testing.T) {
	// Plant a stale-key false positive — the fuzzer's Check 7.g surfaces
	// this independent of generation. This is "test the test" — proves
	// the property check catches a real regression.
	r := Run(FuzzConfig{Seed: 1, Trials: 50, StaleKeyPostRevokeIngestions: 7})
	if len(r.Violations) == 0 {
		t.Fatal("expected at least one violation — planted stale-key false positive ignored")
	}
	found := false
	for _, v := range r.Violations {
		if v.Invariant == InvStaleKeyZeroPost {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("planted stale-key violation not surfaced; got: %+v", r.Violations)
	}
}

func TestCheckInvariants_HybridSplitMustSumToTotal(t *testing.T) {
	// Synthesize a sample that hand-violates the HYBRID invariant by
	// constructing a CalcResult that doesn't add up.
	s := Sample{Index: 99, Model: billing.PerUnit, Mode: billing.Hybrid}
	bad := billing.CalcResult{
		Subtotal:    100,
		Total:       100,
		WalletDebit: 30,
		InvoiceUSD:  60, // 30+60=90, not 100 — invariant violated
	}
	v := checkInvariants(s, bad)
	found := false
	for _, viol := range v {
		if viol.Invariant == InvHybridSplit {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("HYBRID split violation not caught; got: %+v", v)
	}
}

func TestCheckInvariants_DiscountBound(t *testing.T) {
	s := Sample{Index: 1, Model: billing.PerUnit, Mode: billing.Postpaid}
	bad := billing.CalcResult{
		Subtotal:       50,
		DiscountAmount: 75, // exceeds subtotal — violation
		Total:          0,
		InvoiceUSD:     0,
	}
	v := checkInvariants(s, bad)
	found := false
	for _, viol := range v {
		if viol.Invariant == InvDiscountBound {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("discount-bound violation not caught")
	}
}

func TestCheckInvariants_NonNegativeInvoice(t *testing.T) {
	s := Sample{Index: 1, Model: billing.PerUnit, Mode: billing.Postpaid}
	bad := billing.CalcResult{
		Subtotal:   10,
		InvoiceUSD: -5,
	}
	v := checkInvariants(s, bad)
	found := false
	for _, viol := range v {
		if viol.Invariant == InvNonNegativeInvoice {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("non-negative invoice violation not caught")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
