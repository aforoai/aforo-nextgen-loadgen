package payments

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestNewStripeClient_OfflineDefault(t *testing.T) {
	t.Setenv("STRIPE_TEST_SECRET_KEY", "")
	c, err := NewStripeClient(Config{})
	if err != nil {
		t.Fatalf("offline default should construct without error: %v", err)
	}
	if c.Mode() != ModeOffline {
		t.Fatalf("expected offline mode")
	}
}

func TestNewStripeClient_RejectsLiveKey(t *testing.T) {
	_, err := NewStripeClient(Config{APIKey: "sk_live_thisisreal", ForceMode: ModeLive})
	if err == nil {
		t.Fatal("must reject sk_live_ key")
	}
}

func TestNewStripeClient_RejectsBogusKey(t *testing.T) {
	_, err := NewStripeClient(Config{APIKey: "bogus", ForceMode: ModeLive})
	if err == nil {
		t.Fatal("must reject non-sk_test_ key")
	}
}

func TestStripeClient_OfflineCharges(t *testing.T) {
	c, _ := NewStripeClient(Config{ForceMode: ModeOffline})
	ctx := context.Background()
	tests := []struct {
		name string
		card string
		want PaymentOutcome
	}{
		{"success card", TestCardSuccess, OutcomeSucceeded},
		{"decline generic", TestCardDeclineGeneric, OutcomeDeclined},
		{"insufficient", TestCardDeclineInsufFunds, OutcomeInsufficientFunds},
		{"requires action", TestCardRequires3DS, OutcomeRequiresAction},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ch, err := c.CreatePaymentIntent(ctx, 100, "USD", tt.card, "idem-"+tt.name, "cust_test")
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if ch.Outcome != tt.want {
				t.Errorf("outcome = %v; want %v", ch.Outcome, tt.want)
			}
			if ch.PaymentIntentID == "" {
				t.Error("missing payment_intent_id")
			}
			if ch.IdempotencyKey == "" {
				t.Error("missing idempotency_key")
			}
		})
	}
}

func TestStripeClient_RequiresIdempotency(t *testing.T) {
	c, _ := NewStripeClient(Config{ForceMode: ModeOffline})
	_, err := c.CreatePaymentIntent(context.Background(), 100, "USD", TestCardSuccess, "", "cust")
	if err == nil {
		t.Fatal("missing idempotency key must error — acceptance criterion")
	}
}

func TestStripeClient_RequiresPositiveAmount(t *testing.T) {
	c, _ := NewStripeClient(Config{ForceMode: ModeOffline})
	for _, amt := range []float64{0, -1} {
		_, err := c.CreatePaymentIntent(context.Background(), amt, "USD", TestCardSuccess, "idem", "cust")
		if err == nil {
			t.Errorf("amount %v should error", amt)
		}
	}
}

func TestOutcomePicker_Distribution(t *testing.T) {
	p := scenario.Payments{
		SuccessPct:           0.7,
		DeclinePct:           0.2,
		InsufficientFundsPct: 0.1,
	}
	picker := NewOutcomePicker(p, 42)
	dist := Distribution{}
	for i := 0; i < 10000; i++ {
		dist.Tally(picker.Pick(""))
	}
	successPct := dist.SuccessPct()
	declinePct := dist.DeclinePct()
	if math.Abs(successPct-0.7) > 0.02 {
		t.Errorf("success rate %v; want ~0.7", successPct)
	}
	// decline+insuf together
	if math.Abs(declinePct-0.3) > 0.02 {
		t.Errorf("decline+insuf %v; want ~0.3", declinePct)
	}
}

func TestOutcomePicker_Pin(t *testing.T) {
	picker := NewOutcomePicker(scenario.Payments{SuccessPct: 1.0}, 1)
	picker.Pin("inv-1", OutcomeDeclined)
	if got := picker.Pick("inv-1"); got != OutcomeDeclined {
		t.Errorf("pinned outcome ignored: %v", got)
	}
	if got := picker.Pick("inv-2"); got != OutcomeSucceeded {
		t.Errorf("non-pinned should follow weights: %v", got)
	}
}

func TestOutcomePicker_DefaultsToSuccess(t *testing.T) {
	// All pcts zero → 100% success.
	picker := NewOutcomePicker(scenario.Payments{}, 1)
	for i := 0; i < 100; i++ {
		if got := picker.Pick(""); got != OutcomeSucceeded {
			t.Fatalf("default mix returned %v on iteration %d", got, i)
		}
	}
}

func TestCardFor(t *testing.T) {
	tests := []struct {
		out  PaymentOutcome
		card string
	}{
		{OutcomeSucceeded, TestCardSuccess},
		{OutcomeDeclined, TestCardDeclineGeneric},
		{OutcomeInsufficientFunds, TestCardDeclineInsufFunds},
		{OutcomeRequiresAction, TestCardRequires3DS},
	}
	for _, tt := range tests {
		got, err := CardFor(tt.out)
		if err != nil || got != tt.card {
			t.Errorf("%v → %s, err %v; want %s", tt.out, got, err, tt.card)
		}
	}
	if _, err := CardFor(OutcomeError); err == nil {
		t.Error("CardFor(error) should fail")
	}
}

func TestPaymentLog_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "payments.jsonl")
	pl, err := NewPaymentLog(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := []PaymentRecord{
		{InvoiceID: "i1", TenantID: "t1", AmountUSD: 100, Currency: "USD", Outcome: "succeeded"},
		{InvoiceID: "i2", TenantID: "t1", AmountUSD: 50, Currency: "EUR", Outcome: "declined", FailureCode: "card_declined"},
	}
	for _, r := range want {
		if err := pl.Append(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	if err := pl.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("file: %v", err)
	}
	got, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %d records; want %d", len(got), len(want))
	}
	for i, r := range got {
		if r.InvoiceID != want[i].InvoiceID || r.Outcome != want[i].Outcome {
			t.Errorf("record %d: %+v != %+v", i, r, want[i])
		}
	}
}

func TestLoad_NoFile(t *testing.T) {
	got, err := Load(t.TempDir())
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty: %d records", len(got))
	}
}

func TestClassifyStripeStatus(t *testing.T) {
	tests := []struct {
		status string
		err    *stripeError
		want   PaymentOutcome
	}{
		{"succeeded", nil, OutcomeSucceeded},
		{"requires_action", nil, OutcomeRequiresAction},
		{"requires_payment_method", &stripeError{Code: "insufficient_funds"}, OutcomeInsufficientFunds},
		{"requires_payment_method", &stripeError{Code: "card_declined"}, OutcomeDeclined},
		{"requires_payment_method", nil, OutcomeDeclined},
		{"canceled", nil, OutcomeError},
		{"failed", nil, OutcomeError},
		{"unknown", nil, OutcomeError},
	}
	for _, tt := range tests {
		got := classifyStripeStatus(tt.status, tt.err)
		if got != tt.want {
			t.Errorf("status=%s err=%+v: got %v; want %v", tt.status, tt.err, got, tt.want)
		}
	}
}
