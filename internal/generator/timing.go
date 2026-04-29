package generator

import (
	"context"
	"math"
	"math/rand"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// Pacer schedules tick events at a target TPS. Implementations differ in how
// they shape rate over a 24h window (constant, sine, bursty), but every
// Pacer must be drift-free over multi-day runs and must respect ctx.Done().
//
// Wait blocks until the next tick is due, then returns the canonical event
// timestamp for that tick. A drift-free pacer computes the next deadline as
//
//	start + count / target_tps
//
// rather than now + 1/target_tps; the latter accumulates scheduler jitter
// across hundreds of millions of ticks.
type Pacer interface {
	Wait(ctx context.Context) (time.Time, error)
	// Multiplier returns the current TPS multiplier (1.0 = full speed,
	// 0.5 = half-speed under backpressure). Pacers consult this every tick.
	Multiplier() float64
	SetMultiplier(m float64)
	// Stop releases any underlying timer/goroutine. Idempotent.
	Stop()
}

// PacerConfig is the construction-time bag of knobs.
type PacerConfig struct {
	TargetTPS  int
	Pattern    scenario.TimePattern
	Start      time.Time
	BurstySeed int64 // optional — used by bursty pacer for jitter

	// Bursty knobs (applied only when Pattern==TimeBursty). Reasonable defaults.
	BurstyPeakMultiplier float64 // peak TPS multiplier during a burst (default 4.0)
	BurstyDuration       time.Duration
	BurstyEvery          time.Duration

	// Sine knobs (applied only when Pattern==TimeSine24h).
	SineAmplitude float64 // 0..1 — fraction of TargetTPS swing (default 0.4)

	// Now is for tests. nil → time.Now.
	Now func() time.Time
	// Sleep is for tests. nil → blocks via time.NewTimer + ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// NewPacer constructs the appropriate Pacer for the scenario time pattern.
func NewPacer(cfg PacerConfig) Pacer {
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Sleep == nil {
		cfg.Sleep = ctxSleep
	}
	if cfg.Start.IsZero() {
		cfg.Start = cfg.Now()
	}
	if cfg.BurstyPeakMultiplier <= 0 {
		cfg.BurstyPeakMultiplier = 4.0
	}
	if cfg.BurstyDuration <= 0 {
		cfg.BurstyDuration = 30 * time.Second
	}
	if cfg.BurstyEvery <= 0 {
		cfg.BurstyEvery = 10 * time.Minute
	}
	if cfg.SineAmplitude <= 0 || cfg.SineAmplitude > 1 {
		cfg.SineAmplitude = 0.4
	}

	base := basePacer{
		cfg:        cfg,
		multiplier: 1.0,
	}
	switch cfg.Pattern {
	case scenario.TimeSine24h:
		return &sinePacer{basePacer: base}
	case scenario.TimeBursty:
		return &burstyPacer{
			basePacer: base,
			rng:       rand.New(rand.NewSource(cfg.BurstySeed)),
		}
	default:
		return &constantPacer{basePacer: base}
	}
}

// basePacer holds the drift-free count and shared multiplier.
type basePacer struct {
	cfg        PacerConfig
	count      int64
	multiplier float64
}

func (b *basePacer) Multiplier() float64 { return b.multiplier }
func (b *basePacer) SetMultiplier(m float64) {
	if m <= 0 {
		m = 0.0001 // avoid divide-by-zero in deadline math; effectively pause
	}
	b.multiplier = m
}
func (b *basePacer) Stop() {}

// constantPacer — every tick is 1/(TargetTPS*multiplier) apart.
//
// Implementation: incremental scheduling. Each tick computes its emit
// interval from the CURRENT multiplier and adds it to the next-emit time.
// This is:
//   - drift-free at constant multiplier (sum of intervals = count × interval)
//   - cleanly responsive to multiplier changes (next interval reflects the
//     new multiplier immediately, with no retroactive "catch up" hammer
//     that a count/rate scheme would produce after a downshift)
type constantPacer struct {
	basePacer
	next    time.Time
	started bool
}

func (p *constantPacer) Wait(ctx context.Context) (time.Time, error) {
	if p.cfg.TargetTPS <= 0 {
		return time.Time{}, nil // pacer disabled
	}
	rate := float64(p.cfg.TargetTPS) * p.multiplier
	if rate <= 0 {
		rate = 0.0001
	}
	interval := time.Duration(float64(time.Second) / rate)
	if !p.started {
		p.next = p.cfg.Start
		p.started = true
	}
	if err := p.cfg.Sleep(ctx, p.next.Sub(p.cfg.Now())); err != nil {
		return time.Time{}, err
	}
	deadline := p.next
	p.next = p.next.Add(interval)
	p.count++
	return deadline, nil
}

// sinePacer — TPS shapes by sin(2π * elapsed/24h). Peak at hour 6 (sin=1),
// trough at hour 18 (sin=-1). Stays drift-free by integrating the rate
// function: count(t) = ∫ TargetTPS * (1 + amp*sin(2π t / 24h)) dt
//
// We invert this by Newton-Raphson on the integral so each tick advances
// the integrated count by 1.0.
type sinePacer struct{ basePacer }

func (p *sinePacer) Wait(ctx context.Context) (time.Time, error) {
	if p.cfg.TargetTPS <= 0 {
		return time.Time{}, nil
	}
	target := float64(p.count + 1)
	// Solve cumulativeSineCount(t) = target for t (seconds since start).
	t := p.solveForCumulative(target)
	deadline := p.cfg.Start.Add(time.Duration(t * float64(time.Second)))
	if err := p.cfg.Sleep(ctx, deadline.Sub(p.cfg.Now())); err != nil {
		return time.Time{}, err
	}
	p.count++
	return deadline, nil
}

// cumulativeSineCount returns ∫₀ᵗ TPS·m·(1+amp·sin(2π s / 86400)) ds
// = TPS·m·t - TPS·m·amp·86400/(2π)·(cos(2π t/86400) - 1).
func (p *sinePacer) cumulativeSineCount(seconds float64) float64 {
	tps := float64(p.cfg.TargetTPS) * p.multiplier
	if tps <= 0 {
		return 0
	}
	period := 86400.0
	omega := 2.0 * math.Pi / period
	amp := p.cfg.SineAmplitude
	return tps*seconds + tps*amp*(1.0-math.Cos(omega*seconds))/omega
}

func (p *sinePacer) solveForCumulative(target float64) float64 {
	// Decent initial guess: linear inverse at mean rate (ignore amplitude).
	tps := float64(p.cfg.TargetTPS) * p.multiplier
	if tps <= 0 {
		// Degenerate: return 1s in the future.
		return 1.0
	}
	t := target / tps
	// Newton-Raphson — d/dt cumulativeSineCount = tps*(1+amp*sin(2πt/86400))
	for iter := 0; iter < 16; iter++ {
		f := p.cumulativeSineCount(t) - target
		dfdt := tps * (1.0 + p.cfg.SineAmplitude*math.Sin(2*math.Pi*t/86400.0))
		if dfdt <= 0 {
			break
		}
		dt := f / dfdt
		t -= dt
		if math.Abs(dt) < 1e-9 {
			break
		}
		if t < 0 {
			t = 0
		}
	}
	return t
}

// burstyPacer — overall mean TPS == TargetTPS, but emits bursts at
// `BurstyPeakMultiplier × TargetTPS` for `BurstyDuration` every `BurstyEvery`.
// Off-peak rate is computed so the long-run average matches TargetTPS.
type burstyPacer struct {
	basePacer
	rng *rand.Rand
}

func (p *burstyPacer) Wait(ctx context.Context) (time.Time, error) {
	if p.cfg.TargetTPS <= 0 {
		return time.Time{}, nil
	}
	target := float64(p.count + 1)
	t := p.solveForCumulative(target)
	deadline := p.cfg.Start.Add(time.Duration(t * float64(time.Second)))
	if err := p.cfg.Sleep(ctx, deadline.Sub(p.cfg.Now())); err != nil {
		return time.Time{}, err
	}
	p.count++
	return deadline, nil
}

// burst window: [k*BurstyEvery, k*BurstyEvery + BurstyDuration). Inside the
// window the rate is peak; outside the rate is computed so the mean over
// every BurstyEvery cycle equals TargetTPS:
//
//	peak*duration + offpeak*(every-duration) = TargetTPS * every
//	offpeak = (TargetTPS*every - peak*duration) / (every - duration)
//
// If offpeak < 0 (peak too aggressive), we clamp to 0 — caller's tuning bug.
func (p *burstyPacer) cumulativeBurstyCount(seconds float64) float64 {
	tps := float64(p.cfg.TargetTPS) * p.multiplier
	if tps <= 0 {
		return 0
	}
	every := p.cfg.BurstyEvery.Seconds()
	dur := p.cfg.BurstyDuration.Seconds()
	if dur >= every {
		return tps * p.cfg.BurstyPeakMultiplier * seconds
	}
	peak := tps * p.cfg.BurstyPeakMultiplier
	offpeak := (tps*every - peak*dur) / (every - dur)
	if offpeak < 0 {
		offpeak = 0
	}
	cycles := math.Floor(seconds / every)
	rem := seconds - cycles*every
	cyclePart := cycles * (peak*dur + offpeak*(every-dur))
	var remPart float64
	if rem <= dur {
		remPart = peak * rem
	} else {
		remPart = peak*dur + offpeak*(rem-dur)
	}
	return cyclePart + remPart
}

func (p *burstyPacer) solveForCumulative(target float64) float64 {
	tps := float64(p.cfg.TargetTPS) * p.multiplier
	if tps <= 0 {
		return 1.0
	}
	t := target / tps
	for iter := 0; iter < 24; iter++ {
		f := p.cumulativeBurstyCount(t) - target
		// Numerical derivative — ratesAt is piecewise so closed-form gets
		// fiddly across cycle boundaries; tiny epsilon is safe.
		eps := 1e-3
		dfdt := (p.cumulativeBurstyCount(t+eps) - p.cumulativeBurstyCount(t)) / eps
		if dfdt <= 0 {
			break
		}
		dt := f / dfdt
		t -= dt
		if math.Abs(dt) < 1e-9 {
			break
		}
		if t < 0 {
			t = 0
		}
	}
	return t
}

// ctxSleep is the default sleep — supports cancellation and zero/negative
// durations (returns nil immediately).
func ctxSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		// Yield so a tight loop still respects ctx.
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
