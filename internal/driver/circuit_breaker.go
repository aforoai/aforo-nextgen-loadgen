package driver

import (
	"sync"
	"sync/atomic"
	"time"
)

// CircuitState reports whether the breaker is closed (passing through),
// open (refusing requests), or half-open (probing).
type CircuitState int32

const (
	StateClosed CircuitState = iota
	StateHalfOpen
	StateOpen
)

func (s CircuitState) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateHalfOpen:
		return "half_open"
	case StateOpen:
		return "open"
	}
	return "unknown"
}

// CircuitBreakerConfig governs trip/reset behavior.
type CircuitBreakerConfig struct {
	// ErrorRateThreshold trips the breaker when (errors / total) within
	// the rolling Window exceeds this fraction. Default 0.5 (50%).
	ErrorRateThreshold float64
	// MinSamples is the minimum sample count before the rate check applies.
	// Below this, the breaker stays closed regardless of error %.
	MinSamples int
	// Window is the rolling lookback for the rate computation. Default 60s.
	Window time.Duration
	// OpenDuration is how long the breaker stays open before half-open
	// probes resume. Default 30s.
	OpenDuration time.Duration
	// HalfOpenProbes is the number of consecutive successes required to
	// close from half-open. Default 5.
	HalfOpenProbes int
	// Now is for tests. Defaults to time.Now.
	Now func() time.Time
}

// CircuitBreaker is a rolling-window breaker. Every Submit reports a
// success or failure; the breaker computes a rolling error rate and trips
// when the rate exceeds the threshold.
type CircuitBreaker struct {
	cfg         CircuitBreakerConfig
	state       atomic.Int32 // CircuitState
	openedAt    atomic.Int64 // unix nano
	halfSuccess atomic.Int32

	mu      sync.Mutex
	buckets []counterBucket // ring buffer; one bucket per second
	now     func() time.Time
}

// counterBucket is one second's success/failure count in the rolling window.
type counterBucket struct {
	tsec int64 // unix seconds bucket id; -1 for empty
	succ int
	fail int
}

// NewCircuitBreaker constructs a breaker with sensible defaults.
func NewCircuitBreaker(cfg CircuitBreakerConfig) *CircuitBreaker {
	if cfg.ErrorRateThreshold <= 0 {
		cfg.ErrorRateThreshold = 0.5
	}
	if cfg.Window <= 0 {
		cfg.Window = 60 * time.Second
	}
	if cfg.OpenDuration <= 0 {
		cfg.OpenDuration = 30 * time.Second
	}
	if cfg.HalfOpenProbes <= 0 {
		cfg.HalfOpenProbes = 5
	}
	if cfg.MinSamples <= 0 {
		cfg.MinSamples = 20
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	bucketCount := int(cfg.Window / time.Second)
	if bucketCount < 1 {
		bucketCount = 1
	}
	cb := &CircuitBreaker{
		cfg:     cfg,
		buckets: make([]counterBucket, bucketCount),
		now:     cfg.Now,
	}
	for i := range cb.buckets {
		cb.buckets[i].tsec = -1
	}
	cb.state.Store(int32(StateClosed))
	return cb
}

// State returns the current state. Cheap (atomic load).
func (cb *CircuitBreaker) State() CircuitState {
	return CircuitState(cb.state.Load())
}

// Allow reports whether a Submit should be attempted. Honors the
// open→half-open timer.
func (cb *CircuitBreaker) Allow() bool {
	switch cb.State() {
	case StateClosed, StateHalfOpen:
		return true
	case StateOpen:
		openedAt := time.Unix(0, cb.openedAt.Load())
		if cb.now().Sub(openedAt) >= cb.cfg.OpenDuration {
			// Transition to half-open. Use CAS so concurrent Allow calls
			// don't double-flip.
			if cb.state.CompareAndSwap(int32(StateOpen), int32(StateHalfOpen)) {
				cb.halfSuccess.Store(0)
			}
			return cb.State() == StateHalfOpen
		}
		return false
	}
	return false
}

// Record reports the outcome of one Submit. Drives state transitions.
//
// success=true increments the success counter. success=false increments
// the failure counter and may trip the breaker if the rolling error rate
// exceeds the threshold.
func (cb *CircuitBreaker) Record(success bool) {
	now := cb.now()
	cb.mu.Lock()
	cb.touchBucket(now, success)
	succ, fail := cb.tally(now)
	cb.mu.Unlock()

	state := cb.State()
	if state == StateHalfOpen {
		if success {
			n := cb.halfSuccess.Add(1)
			if int(n) >= cb.cfg.HalfOpenProbes {
				// Close the breaker; reset history.
				if cb.state.CompareAndSwap(int32(StateHalfOpen), int32(StateClosed)) {
					cb.mu.Lock()
					for i := range cb.buckets {
						cb.buckets[i] = counterBucket{tsec: -1}
					}
					cb.mu.Unlock()
				}
			}
		} else {
			// Any failure during half-open reopens.
			cb.openedAt.Store(now.UnixNano())
			cb.state.Store(int32(StateOpen))
		}
		return
	}

	if state != StateClosed {
		return
	}

	total := succ + fail
	if total < cb.cfg.MinSamples {
		return
	}
	rate := float64(fail) / float64(total)
	if rate >= cb.cfg.ErrorRateThreshold {
		cb.openedAt.Store(now.UnixNano())
		cb.state.Store(int32(StateOpen))
	}
}

// touchBucket increments the bucket for the current second; clears stale
// buckets in the ring before incrementing.
func (cb *CircuitBreaker) touchBucket(now time.Time, success bool) {
	tsec := now.Unix()
	idx := int(tsec) % len(cb.buckets)
	if cb.buckets[idx].tsec != tsec {
		cb.buckets[idx] = counterBucket{tsec: tsec}
	}
	if success {
		cb.buckets[idx].succ++
	} else {
		cb.buckets[idx].fail++
	}
}

// tally returns total success/failure across all buckets within Window.
func (cb *CircuitBreaker) tally(now time.Time) (succ, fail int) {
	threshold := now.Add(-cb.cfg.Window).Unix()
	for _, b := range cb.buckets {
		if b.tsec < 0 || b.tsec <= threshold {
			continue
		}
		succ += b.succ
		fail += b.fail
	}
	return succ, fail
}

// ForceOpen flips to open immediately. Used by tests + the runner during
// emergency drains. Honors the open-duration timer same as a natural trip.
func (cb *CircuitBreaker) ForceOpen() {
	cb.openedAt.Store(cb.now().UnixNano())
	cb.state.Store(int32(StateOpen))
}
