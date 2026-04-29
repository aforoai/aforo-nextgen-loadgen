package driver

import (
	"sync"
	"testing"
	"time"
)

// TestCircuitBreakerStaysClosedBelowMinSamples — small samples shouldn't
// trip the breaker even at 100% error rate.
func TestCircuitBreakerStaysClosedBelowMinSamples(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		MinSamples:         100,
		ErrorRateThreshold: 0.5,
		Window:             time.Minute,
		Now:                func() time.Time { return time.Unix(1000, 0) },
	})
	for i := 0; i < 10; i++ {
		cb.Record(false)
	}
	if cb.State() != StateClosed {
		t.Errorf("breaker should stay closed below min samples; got %s", cb.State())
	}
}

// TestCircuitBreakerOpensAtThreshold — 50% error rate over min samples
// trips the breaker.
func TestCircuitBreakerOpensAtThreshold(t *testing.T) {
	now := time.Unix(1000, 0)
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		MinSamples:         20,
		ErrorRateThreshold: 0.5,
		Window:             time.Minute,
		OpenDuration:       30 * time.Second,
		Now:                func() time.Time { return now },
	})
	// 12 failures + 8 successes = 60% error rate over 20 samples.
	for i := 0; i < 12; i++ {
		cb.Record(false)
	}
	for i := 0; i < 8; i++ {
		cb.Record(true)
	}
	if cb.State() != StateOpen {
		t.Errorf("breaker should be OPEN at 60%% error; got %s", cb.State())
	}
	// Allow returns false while open.
	if cb.Allow() {
		t.Errorf("Allow should be false when open")
	}
}

// TestCircuitBreakerTransitionsToHalfOpen — open → half-open after
// OpenDuration elapses.
func TestCircuitBreakerTransitionsToHalfOpen(t *testing.T) {
	now := time.Unix(1000, 0)
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		MinSamples:         5,
		ErrorRateThreshold: 0.5,
		Window:             time.Minute,
		OpenDuration:       10 * time.Second,
		HalfOpenProbes:     3,
		Now:                func() time.Time { return now },
	})
	for i := 0; i < 5; i++ {
		cb.Record(false)
	}
	if cb.State() != StateOpen {
		t.Fatalf("breaker should be open; got %s", cb.State())
	}
	// Advance clock past OpenDuration.
	now = now.Add(11 * time.Second)
	if !cb.Allow() {
		t.Errorf("Allow should be true after OpenDuration → half-open")
	}
	if cb.State() != StateHalfOpen {
		t.Errorf("breaker should be HALF_OPEN; got %s", cb.State())
	}

	// Three successes close the breaker.
	cb.Record(true)
	cb.Record(true)
	cb.Record(true)
	if cb.State() != StateClosed {
		t.Errorf("breaker should close after %d probes; got %s", 3, cb.State())
	}
}

// TestCircuitBreakerHalfOpenFailureReopens — any failure in half-open
// reopens the breaker immediately.
func TestCircuitBreakerHalfOpenFailureReopens(t *testing.T) {
	now := time.Unix(1000, 0)
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		MinSamples:         5,
		ErrorRateThreshold: 0.5,
		Window:             time.Minute,
		OpenDuration:       10 * time.Second,
		HalfOpenProbes:     5,
		Now:                func() time.Time { return now },
	})
	for i := 0; i < 5; i++ {
		cb.Record(false)
	}
	now = now.Add(11 * time.Second)
	cb.Allow()
	if cb.State() != StateHalfOpen {
		t.Fatalf("expected half-open; got %s", cb.State())
	}
	cb.Record(true)  // 1 probe ok
	cb.Record(false) // failure → reopen
	if cb.State() != StateOpen {
		t.Errorf("breaker should reopen on half-open failure; got %s", cb.State())
	}
}

// TestCircuitBreakerConcurrent — Records from many goroutines must not
// race; the final state is consistent with the recorded mix.
func TestCircuitBreakerConcurrent(t *testing.T) {
	cb := NewCircuitBreaker(CircuitBreakerConfig{
		MinSamples:         50,
		ErrorRateThreshold: 0.5,
		Window:             time.Minute,
		Now:                time.Now,
	})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				cb.Record(j%2 == 0)
			}
		}(i)
	}
	wg.Wait()
	// 50% errors over many samples — should be at threshold.
	// Either closed (right at threshold) or open (over). Both are OK; we
	// just want no panic / data race.
	st := cb.State()
	if st != StateClosed && st != StateOpen {
		t.Errorf("unexpected state after concurrent records: %s", st)
	}
}
