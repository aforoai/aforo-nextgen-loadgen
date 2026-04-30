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

	// SinePeaksPerDay sets the number of equal-amplitude peaks across a 24h
	// window. Default 3 (US / EU / Asia regional load cycles). Set to 1 for
	// the legacy single-peak shape.
	//
	// Three peaks per day approximates real-world SaaS load: the US peak
	// sits roughly 8h after the EU peak which sits 8h after the Asia peak
	// (the regions take turns being awake). The pacer plants them at
	// FirstPeakOffset, FirstPeakOffset + 8h, FirstPeakOffset + 16h.
	SinePeaksPerDay int

	// FirstPeakOffset is the seconds-since-midnight at which the first peak
	// of the day lands. Default 6h UTC (Asia business hours). With three
	// peaks per day, the others fall at 14h (EU late afternoon) and 22h
	// (US late afternoon to evening).
	FirstPeakOffset time.Duration

	// Now is for tests. nil → time.Now.
	Now func() time.Time
	// Sleep is for tests. nil → blocks via time.NewTimer + ctx.
	Sleep func(ctx context.Context, d time.Duration) error
}

// DefaultSinePeaksPerDay reflects the regional split (US/EU/Asia).
const DefaultSinePeaksPerDay = 3

// DefaultFirstPeakOffset is 06:00 UTC — middle of the Asia business day.
var DefaultFirstPeakOffset = 6 * time.Hour

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
	if cfg.SinePeaksPerDay <= 0 {
		cfg.SinePeaksPerDay = DefaultSinePeaksPerDay
	}
	if cfg.FirstPeakOffset <= 0 {
		cfg.FirstPeakOffset = DefaultFirstPeakOffset
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

// cumulativeSineCount returns the integrated event count over [0, seconds]
// for the N-peaks-per-day shape:
//
//	rate(t) = TPS · m · (1 + amp · cos(2π · N · (t - φ₀) / 86400))
//
// where N = SinePeaksPerDay and φ₀ = FirstPeakOffset is the seconds-since-
// midnight at which the FIRST peak lands. Subsequent peaks repeat every
// 86400/N seconds. The cosine peaks at +1 when its argument is 0, so the
// rate's maximum is at t = φ₀ + k·(86400/N) for integer k ≥ 0.
//
// Three peaks per day → period 8h → peaks at φ₀, φ₀+8h, φ₀+16h. With the
// default φ₀ = 6h, that's 06:00, 14:00, 22:00 UTC — Asia / EU / US local
// business hours respectively (the regions take turns being awake).
//
// Cumulative (integrated):
//
//	C(t) = TPS·m·t + TPS·m·amp/ω · (sin(ω(t-φ₀)) - sin(-ω·φ₀))
//
// where ω = 2π·N/86400. Monotone-non-decreasing for amp ≤ 1: derivative =
// TPS·m·(1 + amp·cos(ω(t-φ₀))) ∈ [TPS·m·(1-amp), TPS·m·(1+amp)] ≥ 0.
func (p *sinePacer) cumulativeSineCount(seconds float64) float64 {
	tps := float64(p.cfg.TargetTPS) * p.multiplier
	if tps <= 0 {
		return 0
	}
	const period = 86400.0
	n := float64(p.cfg.SinePeaksPerDay)
	if n <= 0 {
		n = 1
	}
	amp := p.cfg.SineAmplitude
	phi0 := p.cfg.FirstPeakOffset.Seconds()
	omega := 2.0 * math.Pi * n / period
	cum := tps*seconds + tps*amp/omega*(math.Sin(omega*(seconds-phi0))-math.Sin(-omega*phi0))
	return cum
}

// instantaneousRate is d/dt cumulativeSineCount.
func (p *sinePacer) instantaneousRate(seconds float64) float64 {
	tps := float64(p.cfg.TargetTPS) * p.multiplier
	if tps <= 0 {
		return 0
	}
	const period = 86400.0
	n := float64(p.cfg.SinePeaksPerDay)
	if n <= 0 {
		n = 1
	}
	amp := p.cfg.SineAmplitude
	phi0 := p.cfg.FirstPeakOffset.Seconds()
	omega := 2.0 * math.Pi * n / period
	return tps * (1 + amp*math.Cos(omega*(seconds-phi0)))
}

func (p *sinePacer) solveForCumulative(target float64) float64 {
	// Decent initial guess: linear inverse at mean rate (the sum-of-sines
	// has zero mean over a full period, so TPS is a good linear estimate).
	tps := float64(p.cfg.TargetTPS) * p.multiplier
	if tps <= 0 {
		return 1.0
	}
	t := target / tps
	for iter := 0; iter < 24; iter++ {
		f := p.cumulativeSineCount(t) - target
		dfdt := p.instantaneousRate(t)
		if dfdt <= 0 {
			// Degenerate (extreme amp + multiplier collapse). Fall back to
			// linear advance so the pacer keeps producing.
			t += 1.0 / tps
			continue
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
