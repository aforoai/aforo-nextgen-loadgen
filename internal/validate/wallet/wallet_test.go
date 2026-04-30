package wallet

import "testing"

func TestBalanceCheck_PassesWithExactArithmetic(t *testing.T) {
	b := BalanceCheck{
		PreBalance:        500,
		WalletDebits:      120,
		HoldsReleasedBack: 5,
		PostBalance:       385,
	}
	if !b.IsValid(0.01) {
		t.Fatalf("expected valid; computed=%.4f post=%.4f", b.Computed(), b.PostBalance)
	}
}

func TestBalanceCheck_FailsOnDrift(t *testing.T) {
	b := BalanceCheck{
		PreBalance:        500,
		WalletDebits:      120,
		HoldsReleasedBack: 0,
		PostBalance:       350, // expected 380 → 30 short
	}
	if b.IsValid(1.0) {
		t.Fatal("expected drift to be caught")
	}
}

func TestHoldLifecycle_TerminalStates(t *testing.T) {
	h := HoldLifecycle{State: HoldPending}
	if h.IsTerminal() {
		t.Fatal("PENDING is not terminal")
	}
	h.State = HoldSettled
	if !h.IsTerminal() {
		t.Fatal("SETTLED should be terminal")
	}
}

func TestHoldLifecycle_Settled_OverDebitFails(t *testing.T) {
	h := HoldLifecycle{
		State:      HoldSettled,
		HoldUSD:    10,
		SettledUSD: 12, // exceeds hold
	}
	if h.IsValid(0.01) {
		t.Fatal("settled over hold must fail")
	}
}

func TestHoldLifecycle_Released_FullReturnRequired(t *testing.T) {
	h := HoldLifecycle{
		State:       HoldReleased,
		HoldUSD:     10,
		ReleasedUSD: 8, // partial — invariant violated
	}
	if h.IsValid(0.01) {
		t.Fatal("partial-release must fail")
	}
	h.ReleasedUSD = 10
	if !h.IsValid(0.01) {
		t.Fatal("full-release must pass")
	}
}
