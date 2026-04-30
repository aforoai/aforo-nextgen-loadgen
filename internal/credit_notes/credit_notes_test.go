package credit_notes

import (
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestPickKind_Distribution(t *testing.T) {
	mix := scenario.CreditNotes{
		Enabled:    true,
		RefundPct:  0.10,
		PartialPct: 0.20,
	}
	d := &Driver{
		mix: mix,
	}
	// Use stable rng so the distribution is reproducible.
	d.rng = rand.New(rand.NewSource(7))
	const N = 10000
	var refund, partial, none int
	for i := 0; i < N; i++ {
		switch d.pickKind() {
		case kindFull:
			refund++
		case kindPartial:
			partial++
		default:
			none++
		}
	}
	pct := func(n int) float64 { return float64(n) / float64(N) }
	if pct(refund) < 0.08 || pct(refund) > 0.12 {
		t.Errorf("refund pct %v; want ~0.10", pct(refund))
	}
	if pct(partial) < 0.18 || pct(partial) > 0.22 {
		t.Errorf("partial pct %v; want ~0.20", pct(partial))
	}
	if pct(none) < 0.68 || pct(none) > 0.72 {
		t.Errorf("none pct %v; want ~0.70", pct(none))
	}
}

func TestRoundCents(t *testing.T) {
	tests := []struct {
		in   float64
		want float64
	}{
		{100.005, 100.01},
		{100.004, 100.0},
		{99.999, 100.0},
		{0, 0},
	}
	for _, tt := range tests {
		got := roundCents(tt.in)
		if got != tt.want {
			t.Errorf("roundCents(%v) = %v; want %v", tt.in, got, tt.want)
		}
	}
}

func TestLog_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	l, err := NewLog(filepath.Join(dir, "credit_notes.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	want := []Record{
		{InvoiceID: "i1", TenantID: "t1", CreditNoteID: "cn-1", AmountUSD: 100, Kind: "FULL", Status: "DRAFT"},
		{InvoiceID: "i1", TenantID: "t1", CreditNoteID: "cn-1", AmountUSD: 100, Kind: "FULL", Status: "ISSUED"},
		{InvoiceID: "i1", TenantID: "t1", CreditNoteID: "cn-1", AmountUSD: 100, Kind: "FULL", Status: "APPLIED"},
	}
	for _, r := range want {
		if err := l.Append(r); err != nil {
			t.Fatal(err)
		}
	}
	l.Close()
	got, err := Load(dir)
	if err != nil || len(got) != 3 {
		t.Fatalf("load: %v / count %d", err, len(got))
	}
}

func TestReconstruct(t *testing.T) {
	now := time.Now()
	records := []Record{
		{InvoiceID: "i1", CreditNoteID: "cn-1", AmountUSD: 100, Kind: "FULL", Status: "DRAFT", Timestamp: now},
		{InvoiceID: "i1", CreditNoteID: "cn-1", AmountUSD: 100, Kind: "FULL", Status: "ISSUED", Timestamp: now.Add(time.Second)},
		{InvoiceID: "i1", CreditNoteID: "cn-1", AmountUSD: 100, Kind: "FULL", Status: "APPLIED", Timestamp: now.Add(2 * time.Second)},

		{InvoiceID: "i2", CreditNoteID: "cn-2", AmountUSD: 50, Kind: "PARTIAL", Status: "DRAFT", Timestamp: now},
		{InvoiceID: "i2", CreditNoteID: "cn-2", AmountUSD: 50, Kind: "PARTIAL", Status: "ISSUED", Timestamp: now.Add(time.Second)},

		{InvoiceID: "i3", Status: "error", Note: "draft failed", Timestamp: now},
	}
	progs := Reconstruct(records)
	if len(progs) != 3 {
		t.Fatalf("progs %d; want 3", len(progs))
	}
	byID := map[string]LifecycleProgression{}
	for _, p := range progs {
		byID[p.InvoiceID] = p
	}
	if !byID["i1"].HasDraft || !byID["i1"].HasIssued || !byID["i1"].HasApplied {
		t.Errorf("i1 progression incomplete: %+v", byID["i1"])
	}
	if byID["i1"].HasError {
		t.Errorf("i1 should have no error")
	}
	if !byID["i2"].HasIssued || byID["i2"].HasApplied {
		t.Errorf("i2 expected ISSUED but not APPLIED: %+v", byID["i2"])
	}
	if !byID["i3"].HasError || len(byID["i3"].Errors) != 1 {
		t.Errorf("i3 should record error: %+v", byID["i3"])
	}
}

