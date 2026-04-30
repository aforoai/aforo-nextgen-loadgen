package driver

import (
	"testing"
	"time"
)

// TestFairnessGate_GuaranteesMinimumShare drives a saturating workload
// where a single tenant tries to consume the entire window. The gate
// must defer once the tenant exceeds its cap, ensuring tail tenants
// can claim their guaranteed share.
//
// We measure success by the deferred count: the gate's job is to BLOCK
// requests, not to record them at fractional share. The Stats() snapshot
// reflects only allowed events; the proper signal is "did the gate
// reject saturating requests?"
func TestFairnessGate_GuaranteesMinimumShare(t *testing.T) {
	gate := NewFairnessGate(FairnessConfig{
		Window:           60 * time.Second,
		MinShareFraction: 0.5,
		Now:              func() time.Time { return time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC) },
	})

	const totalTenants = 5
	deferred := 0
	allowed := 0
	for i := 0; i < 200; i++ {
		// Always pick tenant-1 — the gate must eventually block this.
		if gate.Allow("tenant-1", totalTenants) {
			allowed++
		} else {
			deferred++
		}
	}
	if deferred == 0 {
		t.Fatalf("gate never deferred — Pareto monopoly should have triggered the cap (allowed=%d)", allowed)
	}
	// We expect roughly the first ~32 to pass (warm-up), then most subsequent
	// ones to be deferred. With 5 tenants and MinShareFraction=0.5, the cap
	// is uniformShare * 2 / 0.5 * 1 = 0.8 — a single tenant beyond ~80%
	// share should be blocked.
	if allowed > 80 {
		t.Errorf("gate allowed too many monopoly attempts: allowed=%d (expected ~32-50)", allowed)
	}
}

// TestFairnessGate_AllowsAllUnderPopulationDistribution ensures the gate
// doesn't false-positive when traffic is evenly spread.
func TestFairnessGate_AllowsAllUnderPopulationDistribution(t *testing.T) {
	gate := NewFairnessGate(FairnessConfig{
		Window:           60 * time.Second,
		MinShareFraction: 0.5,
		Now:              func() time.Time { return time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC) },
	})
	tenants := []string{"a", "b", "c", "d", "e"}
	deferred := 0
	for i := 0; i < 1000; i++ {
		t := tenants[i%len(tenants)]
		if !gate.Allow(t, len(tenants)) {
			deferred++
		}
	}
	if deferred > 5 {
		t.Errorf("uniform traffic should not trigger the cap; deferred=%d", deferred)
	}
}

// TestFairnessGate_WindowDecaysOverTime verifies the half-life decay so
// long runs don't accumulate stale per-tenant counts forever.
func TestFairnessGate_WindowDecaysOverTime(t *testing.T) {
	now := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	gate := NewFairnessGate(FairnessConfig{
		Window:           5 * time.Second,
		MinShareFraction: 0.5,
		Now:              func() time.Time { return now },
	})
	for i := 0; i < 64; i++ {
		gate.Allow("tenant-1", 5)
	}
	pre := gate.Stats()
	// Advance time past the window.
	now = now.Add(10 * time.Second)
	// One more call to trigger the rollover; total counts should halve.
	gate.Allow("tenant-1", 5)
	post := gate.Stats()

	// The window roll halves stored counts; sample size in the snapshot
	// should be at most pre.SampleSize after one roll, plus the one we
	// added afterwards.
	if post.SampleSize > pre.SampleSize/2+1 {
		t.Errorf("window rollover did not decay: pre=%d post=%d", pre.SampleSize, post.SampleSize)
	}
}

// TestFairnessGate_DisabledForSingleTenant ensures we don't gate when
// only one tenant is in the population — there's nothing to be unfair to.
func TestFairnessGate_DisabledForSingleTenant(t *testing.T) {
	gate := NewFairnessGate(FairnessConfig{
		Window:           60 * time.Second,
		MinShareFraction: 0.5,
	})
	for i := 0; i < 1000; i++ {
		if !gate.Allow("only-tenant", 1) {
			t.Fatalf("gate deferred a single-tenant request at i=%d", i)
		}
	}
}
