package generator

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// TestSine24h_TimezonePhasedPeaks asserts the cumulative count function
// produces three local maxima per 24h corresponding to Asia / EU / US
// peaks at 06:00, 12:00, 18:00 UTC.
//
// We can't directly observe "peaks" on the cumulative function (which is
// monotone), but the rate (= d/dt cumulative) MUST be locally maximal at
// each peak hour and locally minimal between peaks.
func TestSine24h_TimezonePhasedPeaks(t *testing.T) {
	pacer := newSinePacerForTest(t, 1000)
	// Sample the rate at each hour boundary and verify we observe three
	// local maxima within a 24h period.
	const samplesPerHour = 1
	rates := make([]float64, 24*samplesPerHour)
	for i := range rates {
		seconds := float64(i) * 3600.0 / float64(samplesPerHour)
		rates[i] = pacer.instantaneousRate(seconds)
	}
	// Find local maxima — points where rate is greater than both neighbors.
	var peaks []int
	for i := 1; i < len(rates)-1; i++ {
		if rates[i] > rates[i-1] && rates[i] > rates[i+1] {
			peaks = append(peaks, i)
		}
	}
	if len(peaks) != 3 {
		t.Fatalf("expected 3 daily peaks, got %d at hours %v", len(peaks), peaks)
	}
	// Default config: 3 peaks/day, first peak at 06:00 UTC → peaks at
	// 6h (Asia), 14h (EU late afternoon), 22h (US late afternoon).
	expectedHours := []int{6, 14, 22}
	for i, peak := range peaks {
		if abs(peak-expectedHours[i]) > 1 {
			t.Errorf("peak %d at hour %d, expected near %d", i, peak, expectedHours[i])
		}
	}
}

// TestSine24h_CumulativeIsMonotonic guards against a math regression in
// the multi-phase integral. The cumulative count must never decrease.
func TestSine24h_CumulativeIsMonotonic(t *testing.T) {
	pacer := newSinePacerForTest(t, 500)
	prev := 0.0
	for s := 0.0; s < 86400; s += 60 {
		cur := pacer.cumulativeSineCount(s)
		if cur < prev-1e-6 {
			t.Fatalf("cumulative decreased at t=%.0fs: %.3f → %.3f", s, prev, cur)
		}
		prev = cur
	}
}

// TestSine24h_DailyMeanMatchesTPS — over a full 24h period, the mean rate
// equals TargetTPS. The sum-of-sines components have zero mean over the
// period, so the only contribution is the linear term.
func TestSine24h_DailyMeanMatchesTPS(t *testing.T) {
	tps := 1000
	pacer := newSinePacerForTest(t, tps)
	cum24h := pacer.cumulativeSineCount(86400)
	expected := float64(tps) * 86400
	rel := math.Abs(cum24h-expected) / expected
	if rel > 0.01 {
		t.Errorf("24h cumulative count: got %.0f want ≈%.0f (rel err %.4f)", cum24h, expected, rel)
	}
}

func newSinePacerForTest(t *testing.T, tps int) *sinePacer {
	t.Helper()
	now := time.Date(2026, 4, 30, 0, 0, 0, 0, time.UTC)
	cfg := PacerConfig{
		TargetTPS:     tps,
		Pattern:       scenario.TimeSine24h,
		Start:         now,
		SineAmplitude: 0.4,
		Now:           func() time.Time { return now },
		Sleep: func(_ context.Context, _ time.Duration) error {
			return nil
		},
	}
	return NewPacer(cfg).(*sinePacer)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
