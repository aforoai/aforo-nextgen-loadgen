package runner

import (
	"fmt"
	"testing"
	"time"
)

// TestPerTenantStore_FairnessReportComputesStddevPct — populates per-
// tenant histograms with controlled latency distributions and asserts
// FairnessReport's stddev_pct correctly captures the spread.
func TestPerTenantStore_FairnessReportComputesStddevPct(t *testing.T) {
	store := newPerTenantStore()

	// 5 tenants, each with 100 samples clustered around different means.
	// Stddev across tenant p99s should be small but positive.
	tenants := []struct {
		id   string
		base time.Duration
	}{
		{"tenant-a", 5 * time.Millisecond},
		{"tenant-b", 6 * time.Millisecond},
		{"tenant-c", 5500 * time.Microsecond},
		{"tenant-d", 5750 * time.Microsecond},
		{"tenant-e", 6250 * time.Microsecond},
	}
	for _, tt := range tenants {
		for i := 0; i < 100; i++ {
			// Add slight jitter so HDR has multiple bins.
			lat := tt.base + time.Duration(i)*time.Microsecond
			store.Record(tt.id, "rest_direct", "API", lat)
		}
	}
	report := store.FairnessReport()
	if report.TenantsObserved != 5 {
		t.Errorf("tenants observed: got %d want 5", report.TenantsObserved)
	}
	if report.MeanP99Ms <= 0 {
		t.Errorf("mean p99: got %.3f", report.MeanP99Ms)
	}
	if report.StddevPct < 0.0 || report.StddevPct > 0.5 {
		t.Errorf("stddev pct out of expected range: got %.4f", report.StddevPct)
	}
	if report.MaxP99Ms < report.MinP99Ms {
		t.Errorf("max < min: %v < %v", report.MaxP99Ms, report.MinP99Ms)
	}
}

// TestPerTenantStore_OutlierDetected — one tenant 10x slower must surface
// in WorstOffenders. Confirms the noisy-neighbor detection ranking.
func TestPerTenantStore_OutlierDetected(t *testing.T) {
	store := newPerTenantStore()
	for i := 0; i < 4; i++ {
		id := fmt.Sprintf("good-%d", i)
		for j := 0; j < 100; j++ {
			store.Record(id, "rest_direct", "API", 5*time.Millisecond)
		}
	}
	for j := 0; j < 100; j++ {
		store.Record("noisy-neighbor", "rest_direct", "API", 50*time.Millisecond)
	}
	report := store.FairnessReport()
	if len(report.WorstOffenders) == 0 {
		t.Fatal("expected non-empty WorstOffenders")
	}
	if report.WorstOffenders[0].TenantID != "noisy-neighbor" {
		t.Errorf("worst offender: got %q want noisy-neighbor (offenders: %+v)",
			report.WorstOffenders[0].TenantID, report.WorstOffenders)
	}
	// stddev_pct should be >0.5 (50% deviation from mean) when a 10x outlier
	// dominates a 5-sample population.
	if report.StddevPct < 0.5 {
		t.Errorf("expected high stddev_pct with outlier, got %.4f", report.StddevPct)
	}
}

// TestPerTenantStore_MemoryFootprint matches the spec: 50 tenants × 4
// paths × 4 product types ≈ 120 MiB. The footprint reporter must round-
// trip to within reason.
func TestPerTenantStore_MemoryFootprint(t *testing.T) {
	store := newPerTenantStore()
	for ti := 0; ti < 50; ti++ {
		tid := fmt.Sprintf("tenant-%02d", ti)
		for _, p := range []string{"rest_direct", "sdk_node", "gateway_kong", "webhook_receiver"} {
			for _, pt := range []string{"API", "AI_AGENT", "MCP_SERVER", "AGENTIC_API"} {
				store.Record(tid, p, pt, time.Millisecond)
			}
		}
	}
	mb := float64(store.MemoryFootprint()) / (1 << 20)
	// Spec says ~400MB ceiling. Our actual ~120MiB at the documented
	// breakdown — anything between 50 and 400 indicates the histograms
	// were allocated as expected.
	if mb < 50 || mb > 400 {
		t.Errorf("memory footprint out of expected range: %.1fMB", mb)
	}
}
