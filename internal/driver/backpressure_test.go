package driver

import (
	"testing"
	"time"
)

// TestBackpressureActivatesAtThreshold — error rate >5% over Window
// triggers throttling (multiplier drops to 0.5).
func TestBackpressureActivatesAtThreshold(t *testing.T) {
	now := time.Unix(1000, 0)
	bp := NewBackpressure(BackpressureConfig{
		MinSamples:         10,
		ErrorRateThreshold: 0.05,
		Window:             30 * time.Second,
		SlowMultiplier:     0.5,
		RecoverDelay:       30 * time.Second,
		Now:                func() time.Time { return now },
	})
	if bp.Multiplier() != 1.0 {
		t.Fatalf("initial multiplier = %f, want 1.0", bp.Multiplier())
	}
	// 2 failures + 8 successes = 20% error rate over 10 samples.
	for i := 0; i < 8; i++ {
		bp.Record(true)
	}
	for i := 0; i < 2; i++ {
		bp.Record(false)
	}
	if !bp.Active() {
		t.Errorf("backpressure should be active at 20%% error")
	}
	if got := bp.Multiplier(); got != 0.5 {
		t.Errorf("multiplier = %f, want 0.5 while throttled", got)
	}
}

// TestBackpressureRecoverWaitsForDelay — multiplier returns to 1.0 only
// after RecoverDelay of below-threshold operation.
func TestBackpressureRecoverWaitsForDelay(t *testing.T) {
	now := time.Unix(1000, 0)
	bp := NewBackpressure(BackpressureConfig{
		MinSamples:         10,
		ErrorRateThreshold: 0.05,
		Window:             time.Minute,
		SlowMultiplier:     0.5,
		RecoverDelay:       30 * time.Second,
		Now:                func() time.Time { return now },
	})
	// Engage backpressure.
	for i := 0; i < 8; i++ {
		bp.Record(true)
	}
	for i := 0; i < 2; i++ {
		bp.Record(false)
	}
	if !bp.Active() {
		t.Fatalf("backpressure should be active")
	}

	// Now run successes — but error rate stays elevated until old samples
	// fall out of the window. Add many successes to drop the rate.
	for i := 0; i < 1000; i++ {
		bp.Record(true)
	}
	// Still active? Recovery requires RecoverDelay below threshold.
	now = now.Add(10 * time.Second)
	bp.Record(true)
	if !bp.Active() {
		t.Errorf("backpressure should still be active before RecoverDelay")
	}

	// Advance past RecoverDelay AND record once more so the controller
	// actually evaluates state.
	now = now.Add(31 * time.Second)
	bp.Record(true)
	if bp.Active() {
		t.Errorf("backpressure should release after RecoverDelay; multiplier=%f", bp.Multiplier())
	}
	if bp.Multiplier() != 1.0 {
		t.Errorf("multiplier = %f, want 1.0 after recovery", bp.Multiplier())
	}
}

// TestBackpressureNoFlap — staying below threshold without prior trigger
// should never engage.
func TestBackpressureNoFlap(t *testing.T) {
	bp := NewBackpressure(BackpressureConfig{
		MinSamples: 10,
		Now:        time.Now,
	})
	for i := 0; i < 100; i++ {
		bp.Record(true)
	}
	if bp.Active() {
		t.Errorf("backpressure should not engage on all-success traffic")
	}
}

// TestBackpressureMultiplierAtomic — multiplier reads/writes don't race.
func TestBackpressureMultiplierAtomic(t *testing.T) {
	bp := NewBackpressure(BackpressureConfig{Now: time.Now})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			bp.SetMultiplier(0.5)
			bp.SetMultiplier(1.0)
		}
		close(done)
	}()
	for i := 0; i < 1000; i++ {
		_ = bp.Multiplier()
	}
	<-done
}
