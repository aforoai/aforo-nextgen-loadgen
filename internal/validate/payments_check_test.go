package validate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/creditnotes"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/erp"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/payments"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/wallet"
)

// writePaymentsJSONL drops a synthetic payments.jsonl into dir.
func writePaymentsJSONL(t *testing.T, dir string, recs []payments.PaymentRecord) {
	t.Helper()
	pl, err := payments.NewPaymentLog(filepath.Join(dir, "payments.jsonl"))
	if err != nil {
		t.Fatalf("payments log: %v", err)
	}
	for _, r := range recs {
		if err := pl.Append(r); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	pl.Close()
}

func TestRunPaymentProcessing_NoFile_Skips(t *testing.T) {
	dir := t.TempDir()
	scen := minimalScenario()
	scen.Payments.Enabled = false
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runPaymentProcessing(context.Background())
	if got.Status != StatusSkip {
		t.Fatalf("expected SKIP without payments.jsonl + payments disabled, got %v: %s", got.Status, got.Reason)
	}
}

func TestRunPaymentProcessing_HappyPath(t *testing.T) {
	dir := t.TempDir()
	writePaymentsJSONL(t, dir, []payments.PaymentRecord{
		{InvoiceID: "i1", AmountUSD: 100, Currency: "USD", Outcome: "succeeded", StripeIntentID: "pi_1"},
		{InvoiceID: "i2", AmountUSD: 100, Currency: "USD", Outcome: "succeeded", StripeIntentID: "pi_2"},
		{InvoiceID: "i3", AmountUSD: 100, Currency: "USD", Outcome: "declined", FailureCode: "card_declined"},
	})
	scen := minimalScenario()
	scen.Payments = scenario.Payments{
		Enabled: true, StripeMode: scenario.StripeTest, SuccessPct: 0.66, DeclinePct: 0.34,
	}
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runPaymentProcessing(context.Background())
	if got.Status != StatusPass {
		t.Fatalf("expected PASS; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunPaymentProcessing_MissingIntent_Fails(t *testing.T) {
	dir := t.TempDir()
	writePaymentsJSONL(t, dir, []payments.PaymentRecord{
		{InvoiceID: "i1", AmountUSD: 100, Outcome: "succeeded"}, // no intent id
	})
	scen := minimalScenario()
	scen.Payments = scenario.Payments{Enabled: true, SuccessPct: 1.0}
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runPaymentProcessing(context.Background())
	if got.Status != StatusFail {
		t.Fatalf("expected FAIL on missing intent id; got %v", got.Status)
	}
}

func TestRunPaymentProcessing_MissingFailureCode_Fails(t *testing.T) {
	dir := t.TempDir()
	writePaymentsJSONL(t, dir, []payments.PaymentRecord{
		{InvoiceID: "i1", AmountUSD: 100, Outcome: "declined"}, // no failure_code
	})
	scen := minimalScenario()
	scen.Payments = scenario.Payments{Enabled: true, DeclinePct: 1.0}
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runPaymentProcessing(context.Background())
	if got.Status != StatusFail {
		t.Fatalf("expected FAIL on missing failure_code; got %v", got.Status)
	}
}

func TestRunMultiCurrency_ScenarioMisconfig_Fails(t *testing.T) {
	dir := t.TempDir()
	writePaymentsJSONL(t, dir, []payments.PaymentRecord{
		{InvoiceID: "i1", AmountUSD: 100, Currency: "USD", Outcome: "succeeded"},
	})
	scen := minimalScenario()
	scen.FX.AppliedAt = "event_ingest_time" // the wrong path
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runMultiCurrency(context.Background())
	if got.Status != StatusFail {
		t.Fatalf("expected FAIL on event_ingest_time; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunMultiCurrency_NoForeignInvoices_Skips(t *testing.T) {
	dir := t.TempDir()
	writePaymentsJSONL(t, dir, []payments.PaymentRecord{
		{InvoiceID: "i1", AmountUSD: 100, Currency: "USD", Outcome: "succeeded"},
	})
	scen := minimalScenario()
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runMultiCurrency(context.Background())
	if got.Status != StatusSkip {
		t.Fatalf("expected SKIP when no foreign-currency invoices; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunERPSync_AllSyncedAndVerified_Passes(t *testing.T) {
	dir := t.TempDir()
	sl, err := erp.NewSyncLog(filepath.Join(dir, "erp_sync.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		_ = sl.Append(erp.SyncRecord{
			Timestamp: time.Now(), InvoiceID: "i", TenantID: "t",
			Provider: "quickbooks", ExternalID: "qbo_x", Status: "synced",
			LatencySeconds: 5, Verified: true,
		})
	}
	sl.Close()
	scen := minimalScenario()
	scen.ERP.Enabled = true
	scen.ERP.SyncSLASeconds = 60
	scen.ERP.VerifyExternalIDs = true
	scen.ERP.ProvidersPerTenantMix = map[string]float64{"quickbooks": 1.0}
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runERPSync(context.Background())
	if got.Status != StatusPass {
		t.Fatalf("expected PASS; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunERPSync_BelowThreshold_Fails(t *testing.T) {
	dir := t.TempDir()
	sl, err := erp.NewSyncLog(filepath.Join(dir, "erp_sync.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	// 8/10 synced — below the 99% threshold.
	for i := 0; i < 8; i++ {
		_ = sl.Append(erp.SyncRecord{
			Timestamp: time.Now(), InvoiceID: "i", TenantID: "t",
			Provider: "xero", ExternalID: "x", Status: "synced", Verified: true,
		})
	}
	for i := 0; i < 2; i++ {
		_ = sl.Append(erp.SyncRecord{
			Timestamp: time.Now(), InvoiceID: "i", TenantID: "t",
			Provider: "xero", Status: "missing",
		})
	}
	sl.Close()
	scen := minimalScenario()
	scen.ERP.Enabled = true
	scen.ERP.SyncSLASeconds = 60
	scen.ERP.ProvidersPerTenantMix = map[string]float64{"xero": 1.0}
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runERPSync(context.Background())
	if got.Status != StatusFail {
		t.Fatalf("expected FAIL when sync rate < 99%%; got %v", got.Status)
	}
}

func TestRunCreditNotes_FullProgression_Passes(t *testing.T) {
	dir := t.TempDir()
	cnLog, err := creditnotes.NewLog(filepath.Join(dir, "credit_notes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"DRAFT", "ISSUED", "APPLIED"} {
		_ = cnLog.Append(creditnotes.Record{
			InvoiceID: "i1", CreditNoteID: "cn-1", Status: st, AmountUSD: 50, Kind: "FULL",
		})
	}
	cnLog.Close()
	scen := minimalScenario()
	scen.CreditNotes.Enabled = true
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runCreditNotes(context.Background())
	if got.Status != StatusPass {
		t.Fatalf("expected PASS on complete progression; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunCreditNotes_DraftWithoutIssued_Fails(t *testing.T) {
	dir := t.TempDir()
	cnLog, _ := creditnotes.NewLog(filepath.Join(dir, "credit_notes.jsonl"))
	_ = cnLog.Append(creditnotes.Record{
		InvoiceID: "i1", CreditNoteID: "cn-1", Status: "DRAFT", AmountUSD: 50, Kind: "FULL",
	})
	cnLog.Close()
	scen := minimalScenario()
	scen.CreditNotes.Enabled = true
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runCreditNotes(context.Background())
	if got.Status != StatusFail {
		t.Fatalf("expected FAIL on DRAFT-only; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunWalletLifecycle_Disabled_Skips(t *testing.T) {
	dir := t.TempDir()
	scen := minimalScenario()
	scen.Wallet.HoldExpiryAudit = false
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runWalletLifecycle(context.Background())
	if got.Status != StatusSkip {
		t.Fatalf("expected SKIP when audit not enabled; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunWalletLifecycle_HoldsConverged_Passes(t *testing.T) {
	dir := t.TempDir()
	wl, err := wallet.NewAuditLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	_ = wl.AppendSnapshot(wallet.Snapshot{
		TenantID: "t", CustomerID: "c", WalletID: "w", BalanceUSD: 500, HeldUSD: 0, Phase: "pre",
	})
	_ = wl.AppendSnapshot(wallet.Snapshot{
		TenantID: "t", CustomerID: "c", WalletID: "w", BalanceUSD: 400, HeldUSD: 0, Phase: "post-expiry",
		HoldsActive: 0,
	})
	wl.Close()
	scen := minimalScenario()
	scen.Wallet.HoldExpiryAudit = true
	scen.Wallet.HoldTTLSeconds = 60
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runWalletLifecycle(context.Background())
	if got.Status != StatusPass {
		t.Fatalf("expected PASS on full lifecycle; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunWalletLifecycle_OutstandingHolds_Fails(t *testing.T) {
	dir := t.TempDir()
	wl, _ := wallet.NewAuditLog(dir)
	_ = wl.AppendSnapshot(wallet.Snapshot{
		TenantID: "t", CustomerID: "c", WalletID: "w", BalanceUSD: 500, HeldUSD: 100, Phase: "pre",
	})
	_ = wl.AppendSnapshot(wallet.Snapshot{
		TenantID: "t", CustomerID: "c", WalletID: "w", BalanceUSD: 400, HeldUSD: 50, Phase: "post-expiry",
		HoldsActive: 1,
	})
	wl.Close()
	scen := minimalScenario()
	scen.Wallet.HoldExpiryAudit = true
	scen.Wallet.HoldTTLSeconds = 60
	in := &Inputs{
		RunOutputDir: dir, Run: minimalRunResult(),
		Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runWalletLifecycle(context.Background())
	if got.Status != StatusFail {
		t.Fatalf("expected FAIL when holds outstanding past TTL; got %v: %s", got.Status, got.Reason)
	}
}

func TestRunSingleERPInvariant_MultiERPOn_Skips(t *testing.T) {
	scen := minimalScenario()
	scen.ERP.Enabled = true
	scen.ERP.MultiERPEnabled = true
	scen.ERP.ProvidersPerTenantMix = map[string]float64{"quickbooks": 0.5, "xero": 0.5}
	in := &Inputs{
		Run: minimalRunResult(), Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runSingleERPInvariant(context.Background())
	if got.Status != StatusSkip {
		t.Fatalf("expected SKIP when multi_erp_enabled=true; got %v", got.Status)
	}
}

func TestRunSingleERPInvariant_Disabled_Skips(t *testing.T) {
	scen := minimalScenario()
	scen.ERP.Enabled = false
	in := &Inputs{
		Run: minimalRunResult(), Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runSingleERPInvariant(context.Background())
	if got.Status != StatusSkip {
		t.Fatalf("expected SKIP when erp disabled; got %v", got.Status)
	}
}

func TestRunSingleERPInvariant_TooFewProviders_Skips(t *testing.T) {
	scen := minimalScenario()
	scen.ERP.Enabled = true
	scen.ERP.ProvidersPerTenantMix = map[string]float64{"quickbooks": 1.0}
	in := &Inputs{
		Run: minimalRunResult(), Manifest: minimalManifest(), Scenario: scen,
	}
	v, _ := New(in)
	got := v.runSingleERPInvariant(context.Background())
	if got.Status != StatusSkip {
		t.Fatalf("expected SKIP with <2 providers; got %v", got.Status)
	}
}
