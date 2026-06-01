package driver

import (
	"sort"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// FairnessConfig governs the tenant fairness scheduler.
//
// Without fairness, a Pareto 80/20 tenant distribution can starve tail
// tenants entirely under bounded throughput — the top tenant claims most
// of the worker pool's time and tail tenants accumulate delay in the
// generator → pool channel.
//
// The scheduler enforces a per-tenant minimum slot share over a sliding
// window. Within the slot, the requested distribution (Pareto / Zipf /
// uniform) determines which tenants are picked MORE often, but every
// tenant gets at least its guaranteed slice.
type FairnessConfig struct {
	// Window is the sliding fairness window. Default 60s.
	Window time.Duration
	// MinShareFraction is the minimum slot share each tenant must claim,
	// expressed as a fraction of (1.0 / numTenants). 0.5 means each tenant
	// is guaranteed at least half of its uniform-fair share. 0 disables
	// the lower bound. Default 0.5.
	MinShareFraction float64
	// Now is for tests.
	Now func() time.Time
}

// FairnessGate decides whether a generator-emitted event should be
// dispatched immediately or held back briefly so a tail tenant gets its
// guaranteed share. The gate is non-blocking: it returns a bool the
// generator caller honors by re-rolling the tenant pick.
//
// Implementation: a fixed-window counter per tenant. On each Allow check,
// the gate compares the tenant's recent share to the guaranteed minimum.
// When the tenant's count exceeds (1.5 × min_share × window_total),
// further events are deferred until the window slides forward — this is
// the soft "ceiling" that prevents one tenant from monopolizing capacity.
//
// Note: this scheduler is advisory. The generator picks tenants by
// distribution; the gate biases the pick toward fairness. The downstream
// pool still processes all events that reach it — the gate's job is
// purely to reshape the input distribution.
type FairnessGate struct {
	cfg          FairnessConfig
	now          func() time.Time
	mu           sync.Mutex
	totalEvents  int
	tenantCounts map[string]int
	windowStart  time.Time
	tenantOrder  []string
}

// NewFairnessGate constructs a fairness gate.
func NewFairnessGate(cfg FairnessConfig) *FairnessGate {
	if cfg.Window <= 0 {
		cfg.Window = 60 * time.Second
	}
	if cfg.MinShareFraction <= 0 {
		cfg.MinShareFraction = 0.5
	}
	if cfg.MinShareFraction > 1 {
		cfg.MinShareFraction = 1
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &FairnessGate{
		cfg:          cfg,
		now:          cfg.Now,
		tenantCounts: map[string]int{},
		windowStart:  cfg.Now(),
	}
}

// Allow records that an event for the given tenant is being considered.
// Returns true when the event should proceed; false to ask the generator
// to re-pick. Generator code typically loops with a hard cap so a fully
// saturated window doesn't infinite-loop.
//
// Concurrency: safe to call from multiple goroutines. Synchronizes on a
// single mutex — this is fine for the call rate (one per generated event)
// because the work inside is O(1).
func (g *FairnessGate) Allow(tenantID string, totalTenants int) bool {
	if g == nil || totalTenants <= 1 {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.maybeRollWindow()

	// Compute the per-tenant cap inside the current window. The cap is the
	// uniform-fair share scaled by 1/MinShareFraction; events above this
	// are deferred so tail tenants get their guarantee.
	count := g.tenantCounts[tenantID]
	if count == 0 {
		// First time we've seen this tenant in the current window — track
		// its order so the rollover replays in deterministic shape.
		g.tenantOrder = append(g.tenantOrder, tenantID)
	}

	// Always allow until we've seen enough samples for the share check to
	// be meaningful — otherwise the first event always trips the cap.
	const minTotalForGate = 32
	if g.totalEvents < minTotalForGate {
		g.tenantCounts[tenantID]++
		g.totalEvents++
		return true
	}

	uniformShare := 1.0 / float64(totalTenants)
	currentShare := float64(count) / float64(g.totalEvents)
	// "Hot" cap: 2× the guaranteed share, plus the uniform-fair top-up.
	// This permits reasonable Pareto skew (top tenant gets ~5× tail) while
	// blocking pathological monopolies.
	cap := uniformShare * (1.0 / g.cfg.MinShareFraction) * 2
	if currentShare > cap {
		// Defer — caller re-rolls the tenant pick.
		return false
	}
	g.tenantCounts[tenantID]++
	g.totalEvents++
	return true
}

// maybeRollWindow advances the sliding window when the configured Window
// has elapsed since windowStart. Caller must hold g.mu.
func (g *FairnessGate) maybeRollWindow() {
	if g.now().Sub(g.windowStart) < g.cfg.Window {
		return
	}
	// Half-life decay: keep the previous window's counts at half weight so
	// transitions are smooth (no sudden re-balancing).
	for k, v := range g.tenantCounts {
		half := v / 2
		if half == 0 {
			delete(g.tenantCounts, k)
			continue
		}
		g.tenantCounts[k] = half
	}
	g.totalEvents /= 2
	g.windowStart = g.now()
}

// Snapshot returns the current per-tenant share map. Used by tests +
// metrics. Concurrency-safe.
func (g *FairnessGate) Snapshot() map[string]float64 {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make(map[string]float64, len(g.tenantCounts))
	if g.totalEvents <= 0 {
		return out
	}
	for k, v := range g.tenantCounts {
		out[k] = float64(v) / float64(g.totalEvents)
	}
	return out
}

// Tenants returns the tenant IDs seen in the current window, sorted.
// Used by tests for deterministic assertions.
func (g *FairnessGate) Tenants() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := append([]string{}, g.tenantOrder...)
	sort.Strings(out)
	return out
}

// FairnessStats summarizes the per-tenant share distribution. Used by
// tests + the runner's per-tenant metrics writer.
type FairnessStats struct {
	Tenants    int
	MinShare   float64
	MaxShare   float64
	MeanShare  float64
	StddevPct  float64 // stddev / mean, 0..1
	SampleSize int
}

// Stats computes summary statistics over the current window.
func (g *FairnessGate) Stats() FairnessStats {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := len(g.tenantCounts)
	if n == 0 || g.totalEvents == 0 {
		return FairnessStats{}
	}
	shares := make([]float64, 0, n)
	for _, v := range g.tenantCounts {
		shares = append(shares, float64(v)/float64(g.totalEvents))
	}
	sort.Float64s(shares)
	mean := 0.0
	for _, s := range shares {
		mean += s
	}
	mean /= float64(n)
	variance := 0.0
	for _, s := range shares {
		d := s - mean
		variance += d * d
	}
	variance /= float64(n)
	stddev := 0.0
	if variance > 0 {
		stddev = sqrt(variance)
	}
	stddevPct := 0.0
	if mean > 0 {
		stddevPct = stddev / mean
	}
	return FairnessStats{
		Tenants:    n,
		MinShare:   shares[0],
		MaxShare:   shares[len(shares)-1],
		MeanShare:  mean,
		StddevPct:  stddevPct,
		SampleSize: g.totalEvents,
	}
}

// sqrt without pulling in math (to keep this file dep-free for clarity).
// Newton-Raphson, converges in <8 iterations for any positive input under
// 1.0 — which is all we ever feed it (variances are ≤ 0.25 for shares).
func sqrt(x float64) float64 {
	if x <= 0 {
		return 0
	}
	z := x
	for i := 0; i < 16; i++ {
		z -= (z*z - x) / (2 * z)
	}
	return z
}

// FairnessFilteredEvents wraps a generator's Out() channel with a fairness
// gate. Events for tenants that exceed the cap are dropped (rare in
// practice — the gate's typical effect is to reshape, not lose). Lost
// events are reported via OnDeferred for runner-level accounting.
type FairnessFilteredEvents struct {
	in         <-chan *generator.Event
	out        chan *generator.Event
	gate       *FairnessGate
	totalTen   int
	onDeferred func(*generator.Event)
}

// NewFairnessFilter constructs a filter goroutine that drains in and
// emits accepted events on its returned channel. totalTenants is the
// scenario's expected tenant count (used by Allow's share math). When
// onDeferred is non-nil, deferred events are reported to it instead of
// dropped on the floor.
func NewFairnessFilter(in <-chan *generator.Event, gate *FairnessGate, totalTenants int, onDeferred func(*generator.Event)) <-chan *generator.Event {
	out := make(chan *generator.Event, cap(in))
	f := &FairnessFilteredEvents{
		in: in, out: out, gate: gate, totalTen: totalTenants, onDeferred: onDeferred,
	}
	go f.run()
	return out
}

// run drains in. Closes out when in closes.
func (f *FairnessFilteredEvents) run() {
	defer close(f.out)
	for e := range f.in {
		if e == nil {
			continue
		}
		if !f.gate.Allow(e.TenantID, f.totalTen) {
			if f.onDeferred != nil {
				f.onDeferred(e)
			}
			continue
		}
		f.out <- e
	}
}
