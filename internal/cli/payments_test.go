package cli

import (
	"math"
	"testing"
)

func TestAssignProviders_RespectsMixDistribution(t *testing.T) {
	mix := map[string]float64{
		"quickbooks": 0.6,
		"xero":       0.4,
	}
	tenants := make([]string, 0, 100)
	for i := 0; i < 100; i++ {
		tenants = append(tenants, fmtID(i))
	}
	got := assignProviders(tenants, mix)
	counts := map[string]int{}
	for _, p := range got {
		counts[p]++
	}
	// Should be roughly 60/40, ±5 absolute.
	if math.Abs(float64(counts["quickbooks"]-60)) > 5 {
		t.Errorf("quickbooks count %d; want ~60", counts["quickbooks"])
	}
	if math.Abs(float64(counts["xero"]-40)) > 5 {
		t.Errorf("xero count %d; want ~40", counts["xero"])
	}
}

func TestAssignProviders_Deterministic(t *testing.T) {
	mix := map[string]float64{
		"quickbooks": 0.5,
		"xero":       0.3,
		"netsuite":   0.2,
	}
	tenants := []string{"t-1", "t-2", "t-3", "t-4", "t-5"}
	a := assignProviders(tenants, mix)
	b := assignProviders(tenants, mix)
	for _, ten := range tenants {
		if a[ten] != b[ten] {
			t.Errorf("not deterministic for %s: a=%s b=%s", ten, a[ten], b[ten])
		}
	}
	// Shuffled input must produce the SAME output — assignProviders sorts
	// tenants internally so callers don't have to.
	shuffled := []string{"t-3", "t-5", "t-1", "t-4", "t-2"}
	c := assignProviders(shuffled, mix)
	for _, ten := range tenants {
		if a[ten] != c[ten] {
			t.Errorf("internal sort not stable: %s a=%s c=%s", ten, a[ten], c[ten])
		}
	}
}

func TestAssignProviders_EmptyInputs(t *testing.T) {
	if got := assignProviders(nil, map[string]float64{"quickbooks": 1.0}); len(got) != 0 {
		t.Errorf("nil tenants should yield empty: %v", got)
	}
	if got := assignProviders([]string{"t-1"}, nil); len(got) != 0 {
		t.Errorf("nil mix should yield empty: %v", got)
	}
}

func TestUniqueTenants(t *testing.T) {
	in := []invoiceLite{
		{TenantID: "t-3"}, {TenantID: "t-1"}, {TenantID: "t-1"},
		{TenantID: "t-2"}, {TenantID: "t-2"}, {TenantID: "t-3"},
	}
	got := uniqueTenants(in)
	want := []string{"t-1", "t-2", "t-3"}
	if len(got) != len(want) {
		t.Fatalf("len=%d; want %d", len(got), len(want))
	}
	for i, v := range want {
		if got[i] != v {
			t.Errorf("idx %d: got %s; want %s", i, got[i], v)
		}
	}
}

func TestBuildSyncItems_RespectsTenantProvider(t *testing.T) {
	invoices := []invoiceLite{
		{InvoiceID: "i1", TenantID: "t-1"},
		{InvoiceID: "i2", TenantID: "t-1"}, // same tenant — same provider
		{InvoiceID: "i3", TenantID: "t-2"},
	}
	mix := map[string]float64{"quickbooks": 1.0}
	items := buildSyncItems(invoices, mix)
	if len(items) != 3 {
		t.Fatalf("items=%d; want 3", len(items))
	}
	for _, it := range items {
		if it.Provider != "quickbooks" {
			t.Errorf("invoice %s: provider %s; want quickbooks", it.InvoiceID, it.Provider)
		}
	}
}

func TestBuildSyncItems_EmptyMix(t *testing.T) {
	got := buildSyncItems([]invoiceLite{{InvoiceID: "i1"}}, nil)
	if len(got) != 0 {
		t.Errorf("empty mix should yield no items: %d", len(got))
	}
}

func fmtID(i int) string {
	const hex = "0123456789abcdef"
	out := []byte("t-")
	out = append(out, hex[(i>>4)&0xf], hex[i&0xf])
	return string(out)
}
