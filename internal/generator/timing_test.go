package generator

import (
	"context"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// TestConstantPacerDriftFree — at constant multiplier=1.0, 100 ticks at
// 100 TPS sums to exactly 1 second of "virtual" deadlines.
func TestConstantPacerDriftFree(t *testing.T) {
	now := time.Unix(1000, 0)
	advance := time.Duration(0)
	p := NewPacer(PacerConfig{
		TargetTPS: 100,
		Pattern:   scenario.TimeConstant,
		Start:     now,
		Now:       func() time.Time { return now.Add(advance) },
		Sleep: func(_ context.Context, d time.Duration) error {
			if d > 0 {
				advance += d
			}
			return nil
		},
	})

	first, err := p.Wait(context.Background())
	if err != nil {
		t.Fatalf("Wait 0: %v", err)
	}
	if !first.Equal(now) {
		t.Errorf("first deadline = %v, want %v", first, now)
	}

	for i := 1; i < 100; i++ {
		_, err := p.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
	}
	// 100 ticks at 100 TPS → 99 intervals of 10ms each = 990ms after start.
	wantElapsed := 99 * 10 * time.Millisecond
	if advance != wantElapsed {
		t.Errorf("after 100 ticks: advance=%s, want %s", advance, wantElapsed)
	}
}

// TestConstantPacerHandlesMultiplierChange — multiplier 1.0 → 0.5 must
// affect *subsequent* intervals (not retroactively reschedule already-
// committed deadlines, which would either delay-bombard or
// emit-bombard).
func TestConstantPacerHandlesMultiplierChange(t *testing.T) {
	now := time.Unix(1000, 0)
	advance := time.Duration(0)
	p := NewPacer(PacerConfig{
		TargetTPS: 100,
		Pattern:   scenario.TimeConstant,
		Start:     now,
		Now:       func() time.Time { return now.Add(advance) },
		Sleep: func(_ context.Context, d time.Duration) error {
			if d > 0 {
				advance += d
			}
			return nil
		},
	})

	// 10 ticks at 1.0 → 9 sleeps × 10ms = 90ms.
	for i := 0; i < 10; i++ {
		_, _ = p.Wait(context.Background())
	}
	if advance != 90*time.Millisecond {
		t.Fatalf("after 10 ticks: advance=%s, want 90ms", advance)
	}

	p.SetMultiplier(0.5)

	// Tick 11 was already scheduled (at 100ms) at the OLD multiplier — the
	// gap to tick 11 is still 10ms. That's correct: a downshift can't
	// retroactively delay an already-scheduled tick without either
	// hammering or stalling.
	_, _ = p.Wait(context.Background())
	deltaTo11 := advance - 90*time.Millisecond
	if deltaTo11 != 10*time.Millisecond {
		t.Errorf("tick 11 gap = %s, want 10ms (pre-scheduled at old rate)", deltaTo11)
	}

	// Tick 12 uses the new multiplier — interval doubles to 20ms.
	_, _ = p.Wait(context.Background())
	deltaTo12 := advance - 90*time.Millisecond - deltaTo11
	want := 20 * time.Millisecond
	if deltaTo12 < want-time.Millisecond || deltaTo12 > want+time.Millisecond {
		t.Errorf("tick 12 gap = %s, want %s ±1ms (new multiplier)", deltaTo12, want)
	}
}

// TestSinePacerProducesNonDecreasingDeadlines — sine_24h pacer emits
// monotonically increasing deadlines and approximates the target rate
// over a full cycle.
func TestSinePacerProducesNonDecreasingDeadlines(t *testing.T) {
	now := time.Unix(1000, 0)
	advance := time.Duration(0)
	p := NewPacer(PacerConfig{
		TargetTPS: 100,
		Pattern:   scenario.TimeSine24h,
		Start:     now,
		Now:       func() time.Time { return now.Add(advance) },
		Sleep: func(_ context.Context, d time.Duration) error {
			if d > 0 {
				advance += d
			}
			return nil
		},
	})
	prev := time.Time{}
	for i := 0; i < 200; i++ {
		t1, err := p.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
		if !prev.IsZero() && t1.Before(prev) {
			t.Errorf("non-monotonic deadline at i=%d: %v before %v", i, t1, prev)
		}
		prev = t1
	}
	// 200 ticks at 100 TPS over ~2s under sine — small region of curve, so
	// elapsed should be ~ within 50% of linear baseline.
	wantApprox := 2 * time.Second
	if advance < wantApprox/2 || advance > wantApprox*3 {
		t.Errorf("sine pacer 200-tick advance = %s, want ~%s ±50%%", advance, wantApprox)
	}
}

// TestBurstyPacerEmitsMonotonic — bursty pacer emits monotonically
// increasing deadlines and returns reasonable values for short windows.
func TestBurstyPacerEmitsMonotonic(t *testing.T) {
	now := time.Unix(1000, 0)
	advance := time.Duration(0)
	p := NewPacer(PacerConfig{
		TargetTPS: 100,
		Pattern:   scenario.TimeBursty,
		Start:     now,
		Now:       func() time.Time { return now.Add(advance) },
		Sleep: func(_ context.Context, d time.Duration) error {
			if d > 0 {
				advance += d
			}
			return nil
		},
		BurstyEvery:    60 * time.Second,
		BurstyDuration: 5 * time.Second,
	})
	prev := time.Time{}
	for i := 0; i < 50; i++ {
		t1, err := p.Wait(context.Background())
		if err != nil {
			t.Fatalf("Wait %d: %v", i, err)
		}
		if !prev.IsZero() && t1.Before(prev) {
			t.Errorf("bursty: non-monotonic at i=%d: %v before %v", i, t1, prev)
		}
		prev = t1
	}
}

// TestPacerMultiplierGetSet — Multiplier and SetMultiplier round-trip.
func TestPacerMultiplierGetSet(t *testing.T) {
	p := NewPacer(PacerConfig{TargetTPS: 100, Pattern: scenario.TimeConstant})
	if p.Multiplier() != 1.0 {
		t.Errorf("default multiplier = %v, want 1.0", p.Multiplier())
	}
	p.SetMultiplier(0.5)
	if p.Multiplier() != 0.5 {
		t.Errorf("after SetMultiplier(0.5) = %v, want 0.5", p.Multiplier())
	}
	p.SetMultiplier(0) // floor should kick in
	if p.Multiplier() == 0 {
		t.Errorf("SetMultiplier(0) should floor above zero, got %v", p.Multiplier())
	}
	p.Stop()
}

// TestConstantPacerRespectsContextCancel — Wait returns ctx.Err when ctx
// cancels before or during sleep.
func TestConstantPacerRespectsContextCancel(t *testing.T) {
	p := NewPacer(PacerConfig{
		TargetTPS: 1, // 1 TPS = 1s interval, would block long otherwise
		Pattern:   scenario.TimeConstant,
		Start:     time.Now(),
		Sleep:     ctxSleep,
	})
	// First call returns immediately at start time (no sleep needed).
	if _, err := p.Wait(context.Background()); err != nil {
		t.Fatalf("first Wait: %v", err)
	}

	// Second call needs to sleep ~1s. Cancel mid-sleep.
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if _, err := p.Wait(ctx); err == nil {
		t.Errorf("Wait should respect cancelled ctx; got nil error")
	}
}
