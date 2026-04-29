package driver

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// BackpressureConfig governs how the controller throttles the generator
// based on driver health.
type BackpressureConfig struct {
	// ErrorRateThreshold activates throttling when the rolling error rate
	// exceeds this fraction. Default 0.05 (5%).
	ErrorRateThreshold float64
	// Window is the rolling lookback. Default 30s.
	Window time.Duration
	// SlowMultiplier is the TPS multiplier applied while throttled.
	// Default 0.5 (50%). Lower = harsher throttle.
	SlowMultiplier float64
	// RecoverDelay is how long the error rate must stay below threshold
	// before backpressure releases. Default 30s.
	RecoverDelay time.Duration
	// MinSamples is the minimum sample count before the rate check applies.
	MinSamples int
	// Now is for tests. Defaults to time.Now.
	Now func() time.Time
}

// BackpressureController exposes a TPS multiplier the runner reads on
// every tick. Records driver outcomes and updates the multiplier when the
// rolling error window crosses thresholds.
type BackpressureController struct {
	cfg        BackpressureConfig
	multiplier atomic.Uint64 // float64 bits — 0x3FF0000000000000 == 1.0
	active     atomic.Bool
	belowSince atomic.Int64 // unix nano when error rate first dipped below threshold

	mu      sync.Mutex
	buckets []counterBucket
	now     func() time.Time
}

// NewBackpressure returns a controller with sensible defaults.
func NewBackpressure(cfg BackpressureConfig) *BackpressureController {
	if cfg.ErrorRateThreshold <= 0 {
		cfg.ErrorRateThreshold = 0.05
	}
	if cfg.Window <= 0 {
		cfg.Window = 30 * time.Second
	}
	if cfg.SlowMultiplier <= 0 || cfg.SlowMultiplier >= 1 {
		cfg.SlowMultiplier = 0.5
	}
	if cfg.RecoverDelay <= 0 {
		cfg.RecoverDelay = 30 * time.Second
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
	bp := &BackpressureController{
		cfg:     cfg,
		buckets: make([]counterBucket, bucketCount),
		now:     cfg.Now,
	}
	for i := range bp.buckets {
		bp.buckets[i].tsec = -1
	}
	bp.SetMultiplier(1.0)
	return bp
}

// Multiplier returns the current TPS multiplier (1.0 normal, < 1.0 throttled).
func (bp *BackpressureController) Multiplier() float64 {
	bits := bp.multiplier.Load()
	return float64FromBits(bits)
}

// SetMultiplier overrides the multiplier. Called by tests + the controller
// itself when state transitions.
func (bp *BackpressureController) SetMultiplier(m float64) {
	bp.multiplier.Store(bitsFromFloat64(m))
}

// Active reports whether backpressure is currently throttling.
func (bp *BackpressureController) Active() bool { return bp.active.Load() }

// Record an outcome. Triggers state transitions when the rolling rate
// crosses thresholds and the recovery delay has elapsed.
func (bp *BackpressureController) Record(success bool) {
	now := bp.now()
	bp.mu.Lock()
	bp.touchBucket(now, success)
	succ, fail := bp.tally(now)
	bp.mu.Unlock()

	total := succ + fail
	if total < bp.cfg.MinSamples {
		return
	}
	rate := float64(fail) / float64(total)
	if rate >= bp.cfg.ErrorRateThreshold {
		// Stay/become active; reset belowSince.
		bp.belowSince.Store(0)
		if !bp.active.Load() {
			bp.active.Store(true)
			bp.SetMultiplier(bp.cfg.SlowMultiplier)
		}
		return
	}

	// Below threshold — start recovery timer.
	if bp.belowSince.Load() == 0 {
		bp.belowSince.Store(now.UnixNano())
	}
	if bp.active.Load() {
		first := bp.belowSince.Load()
		if first > 0 && now.Sub(time.Unix(0, first)) >= bp.cfg.RecoverDelay {
			bp.active.Store(false)
			bp.SetMultiplier(1.0)
		}
	}
}

func (bp *BackpressureController) touchBucket(now time.Time, success bool) {
	tsec := now.Unix()
	idx := int(tsec) % len(bp.buckets)
	if bp.buckets[idx].tsec != tsec {
		bp.buckets[idx] = counterBucket{tsec: tsec}
	}
	if success {
		bp.buckets[idx].succ++
	} else {
		bp.buckets[idx].fail++
	}
}

func (bp *BackpressureController) tally(now time.Time) (succ, fail int) {
	threshold := now.Add(-bp.cfg.Window).Unix()
	for _, b := range bp.buckets {
		if b.tsec < 0 || b.tsec <= threshold {
			continue
		}
		succ += b.succ
		fail += b.fail
	}
	return succ, fail
}

// float64FromBits / bitsFromFloat64 are atomic-safe conversions used to
// store the multiplier in an atomic.Uint64. Standard math helpers — kept
// inline so it's clear we're not stashing arbitrary state.
func float64FromBits(bits uint64) float64 { return math.Float64frombits(bits) }
func bitsFromFloat64(f float64) uint64    { return math.Float64bits(f) }
