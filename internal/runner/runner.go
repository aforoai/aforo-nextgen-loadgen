package runner

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
	"gopkg.in/yaml.v3"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/driver"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/metrics"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// Config is the construction-time bag for the runner. The CLI maps its
// flags into this; tests construct it directly.
type Config struct {
	Scenario         *scenario.Scenario
	Manifest         *seed.Manifest
	Target           aforo.Target
	OutputDir        string
	Workers          int
	DurationOverride time.Duration
	BufferSize       int
	AdminToken       string

	// Metrics + pprof — Server is only constructed if MetricsAddr != "".
	MetricsAddr string
	PprofPort   int

	// Session 8 — multi-driver fanout. When non-empty, the runner uses the
	// driver registry + multiplex. When empty, the legacy rest_direct-only
	// behavior is preserved (Sessions 4-7 tests still pass).
	WebhookSources map[string]driver.WebhookSource

	// Session 8 — fairness scheduler. When >0, a fairness gate is wired
	// between the generator and the worker pool. 0 disables it.
	FairnessMinShareFraction float64

	// Now is for tests. nil → time.Now.
	Now func() time.Time

	// Output writers. nil → real files via RunResult.Save.
	Logger io.Writer
}

// Runner is the top-level orchestrator. One Runner per `run` invocation.
type Runner struct {
	cfg            Config
	now            func() time.Time
	runID          string
	startedAt      time.Time
	stoppedAt      time.Time
	histogram      *hdrhistogram.Histogram
	histMu         sync.Mutex
	eventsBuffer   bytes.Buffer
	eventsCount    atomic.Int64
	eventsCap      int
	eventsBufMu    sync.Mutex
	perTenant      sync.Map // tenant_id → *atomic.Int64
	perProductType sync.Map // product_type → *atomic.Int64
	perPath        sync.Map // ingestion_path → *atomic.Int64  (Session 8)
	bpEngaged      []time.Time
	cbOpened       []time.Time
	bpEvtMu        sync.Mutex
	scenarioYAML   []byte
	perTenantStore *perTenantStore // Session 8 — per-tenant fairness histograms
	fairnessGate   *driver.FairnessGate
	deferredCount  atomic.Int64 // events deferred by the fairness gate
	driverRegistry *driver.Registry

	registry *metrics.Registry
	mserver  *metrics.Server
	driver   driver.Driver
	pool     *driver.Pool
	bp       *driver.BackpressureController
	cb       *driver.CircuitBreaker
	gen      *generator.Generator
	pacer    generator.Pacer
}

// New constructs a runner. Validates the scenario/manifest pairing,
// initializes metrics, and prepares (but does not start) the pipeline.
func New(cfg Config) (*Runner, error) {
	if cfg.Scenario == nil {
		return nil, errors.New("runner: scenario is required")
	}
	if cfg.Manifest == nil {
		return nil, errors.New("runner: manifest is required")
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Workers <= 0 {
		cfg.Workers = 32
	}
	if cfg.BufferSize <= 0 {
		cfg.BufferSize = 4096
	}
	if cfg.OutputDir == "" {
		return nil, errors.New("runner: OutputDir is required")
	}
	// Apply duration override before constructing the generator.
	scn := *cfg.Scenario
	if cfg.DurationOverride > 0 {
		scn.Duration = scenario.Duration(cfg.DurationOverride)
	}
	cfg.Scenario = &scn

	// Generator + planner.
	gen, err := generator.NewGenerator(generator.Config{
		Scenario:   cfg.Scenario,
		Manifest:   cfg.Manifest,
		Now:        cfg.Now,
		BufferSize: cfg.BufferSize,
	})
	if err != nil {
		return nil, fmt.Errorf("runner: generator: %w", err)
	}

	// Session 8 — driver fanout via registry + multiplex. The multiplex
	// dispatches each event to the per-event ingestion-path's underlying
	// driver. When the scenario only references rest_direct, the registry
	// constructs only the rest_direct driver — no extra cost for the
	// non-multi-path case.
	driverRegistry, err := driver.NewRegistry(driver.RegistryConfig{
		Target:         cfg.Target,
		AdminToken:     cfg.AdminToken,
		WebhookSources: cfg.WebhookSources,
	})
	if err != nil {
		return nil, fmt.Errorf("runner: driver registry: %w", err)
	}
	// Pre-warm: any ingestion path the scenario references, validate now so
	// constructor errors surface before the run starts.
	for _, name := range scenarioReferencedPaths(cfg.Scenario) {
		if _, err := driverRegistry.Get(name); err != nil {
			// Release any drivers already constructed before bailing.
			_ = driverRegistry.Close()
			return nil, fmt.Errorf("runner: %s: %w", name, err)
		}
	}
	mux := driver.NewMultiplex(driverRegistry)

	// Resilience layer — backpressure (5% / 30s / 50%) + circuit breaker
	// (50% / 60s / 30s pause).
	bp := driver.NewBackpressure(driver.BackpressureConfig{
		Now: cfg.Now,
	})
	cb := driver.NewCircuitBreaker(driver.CircuitBreakerConfig{
		Now: cfg.Now,
	})

	// Pacer.
	pacer := generator.NewPacer(generator.PacerConfig{
		TargetTPS:  cfg.Scenario.TargetTPS,
		Pattern:    cfg.Scenario.TimePattern,
		Start:      cfg.Now(),
		BurstySeed: cfg.Scenario.Seed,
		Now:        cfg.Now,
	})

	// HDR histogram — 1us..30s, 3 sig figs.
	hist := hdrhistogram.New(1, (30 * time.Second).Microseconds(), 3)

	// Metrics registry.
	reg := metrics.NewRegistry()

	r := &Runner{
		cfg:            cfg,
		now:            cfg.Now,
		runID:          randomRunID(),
		eventsCap:      1000,
		registry:       reg,
		driver:         mux,
		bp:             bp,
		cb:             cb,
		gen:            gen,
		pacer:          pacer,
		histogram:      hist,
		perTenantStore: newPerTenantStore(),
		driverRegistry: driverRegistry,
	}
	if cfg.FairnessMinShareFraction > 0 {
		r.fairnessGate = driver.NewFairnessGate(driver.FairnessConfig{
			MinShareFraction: cfg.FairnessMinShareFraction,
			Now:              cfg.Now,
		})
	}

	// Pool wires generator → multiplexed driver, with the backpressure / breaker.
	pool, err := driver.NewPool(driver.PoolConfig{
		Driver:         mux,
		Workers:        cfg.Workers,
		Backpressure:   bp,
		CircuitBreaker: cb,
		MaxQueueDepth:  cfg.BufferSize,
		OnResult:       r.onResult,
	})
	if err != nil {
		return nil, fmt.Errorf("runner: pool: %w", err)
	}
	r.pool = pool

	// Snapshot the scenario YAML for replay.
	if buf, err := yaml.Marshal(cfg.Scenario); err == nil {
		r.scenarioYAML = buf
	}

	// Metrics server — bind eagerly so config errors surface before the
	// generator starts.
	if cfg.MetricsAddr != "" {
		srv, err := metrics.NewServer(metrics.ServerConfig{
			Registry:    reg,
			Addr:        cfg.MetricsAddr,
			EnablePprof: cfg.PprofPort > 0,
		})
		if err != nil {
			return nil, fmt.Errorf("runner: metrics server: %w", err)
		}
		r.mserver = srv
	}

	// Initial gauge values.
	reg.TenantsActive.Set(float64(gen.TenantsActive()))
	reg.BackpressureActive.Set(0)
	reg.CircuitBreakerState.WithLabelValues(mux.Name()).Set(float64(driver.StateClosed))

	return r, nil
}

// scenarioReferencedPaths returns the list of ingestion paths the scenario
// references with positive weight. Used for registry pre-warming so
// constructor errors surface before the run starts.
func scenarioReferencedPaths(s *scenario.Scenario) []string {
	paths := []string{}
	add := func(name string, w float64) {
		if w > 0 {
			paths = append(paths, name)
		}
	}
	p := s.IngestionPaths
	add("rest_direct", p.RestDirect)
	add("sdk_node", p.SDKNode)
	add("sdk_python", p.SDKPython)
	add("sdk_java", p.SDKJava)
	add("sdk_go", p.SDKGo)
	add("gateway_kong", p.GatewayKong)
	add("gateway_apigee", p.GatewayApigee)
	add("gateway_aws", p.GatewayAWS)
	add("gateway_azure", p.GatewayAzure)
	add("gateway_mulesoft", p.GatewayMuleSoft)
	add("gateway_apisix", p.GatewayAPISIX)
	add("gateway_tyk", p.GatewayTyk)
	add("gateway_gravitee", p.GatewayGravitee)
	add("gateway_envoy", p.GatewayEnvoy)
	add("webhook_receiver", p.WebhookReceiver)
	add("csv_upload", p.CSVUpload)
	if len(paths) == 0 {
		paths = append(paths, "rest_direct")
	}
	return paths
}

// Run blocks until the scenario completes, ctx cancels, or a fatal error
// surfaces. Always emits the run artifacts to OutputDir, including on
// SIGINT/SIGTERM-driven cancellation.
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	r.startedAt = r.now()

	// Start metrics server in the background.
	mctx, mcancel := context.WithCancel(ctx)
	defer mcancel()
	if r.mserver != nil {
		go func() {
			_ = r.mserver.Start(mctx)
		}()
	}

	// Pacer multiplier — driver pool feeds backpressure → multiplier.
	// We poll the multiplier on each tick by wiring it into the pacer.
	go r.pollResilience(mctx)

	// Run the generator on its own goroutine; pool drains its Out() channel.
	genErrCh := make(chan error, 1)
	go func() {
		genErrCh <- r.gen.Run(ctx, r.pacerWithBackpressure())
	}()

	// Optional fairness filter — when enabled, the gate reshapes the
	// generator's distribution by deferring events for tenants that exceed
	// their per-window cap. Deferred events are counted but not dispatched
	// (re-pick happens at the generator's next tick).
	events := r.gen.Out()
	if r.fairnessGate != nil {
		events = driver.NewFairnessFilter(events, r.fairnessGate, r.gen.TenantsActive(), func(*generator.Event) {
			r.deferredCount.Add(1)
		})
	}
	// Pool blocks until the generator's channel closes.
	r.pool.Run(ctx, events)

	// Generator may still be in flight if it errored without closing — so
	// wait for it to return.
	err := <-genErrCh
	r.stoppedAt = r.now()
	// A non-context generator error is recorded into the result via
	// buildResult and surfaced to the caller after artifacts are written.

	// Build the result and write artifacts.
	result := r.buildResult(err)

	if r.mserver != nil {
		_ = r.mserver.Close()
	}
	_ = r.pool.Close()

	if saveErr := result.Save(r.cfg.OutputDir, r.histogram, r.flushEventsLog(), r.scenarioYAML); saveErr != nil {
		return result, saveErr
	}
	return result, err
}

// pacerWithBackpressure wraps the pacer so the backpressure multiplier is
// re-applied before each Wait. Keeps the generator package independent of
// the driver package.
type pacerWrapper struct {
	pacer generator.Pacer
	bp    *driver.BackpressureController
}

func (p pacerWrapper) Wait(ctx context.Context) (time.Time, error) {
	if p.bp != nil {
		p.pacer.SetMultiplier(p.bp.Multiplier())
	}
	return p.pacer.Wait(ctx)
}
func (p pacerWrapper) Multiplier() float64     { return p.pacer.Multiplier() }
func (p pacerWrapper) SetMultiplier(m float64) { p.pacer.SetMultiplier(m) }
func (p pacerWrapper) Stop()                   { p.pacer.Stop() }

func (r *Runner) pacerWithBackpressure() generator.Pacer {
	return pacerWrapper{pacer: r.pacer, bp: r.bp}
}

// pollResilience updates Prometheus gauges (and tracks state-change times
// for run.json) every second until ctx cancels.
func (r *Runner) pollResilience(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	prevBp := false
	prevCb := driver.StateClosed
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			cur := r.bp.Active()
			if cur {
				r.registry.BackpressureActive.Set(1)
				if !prevBp {
					r.bpEvtMu.Lock()
					r.bpEngaged = append(r.bpEngaged, r.now())
					r.bpEvtMu.Unlock()
				}
			} else {
				r.registry.BackpressureActive.Set(0)
			}
			prevBp = cur

			st := r.cb.State()
			r.registry.CircuitBreakerState.WithLabelValues(r.driver.Name()).Set(float64(st))
			if st == driver.StateOpen && prevCb != driver.StateOpen {
				r.bpEvtMu.Lock()
				r.cbOpened = append(r.cbOpened, r.now())
				r.bpEvtMu.Unlock()
			}
			prevCb = st
		}
	}
}

// onResult is the per-event hook invoked by the worker pool. Updates HDR,
// per-tenant counters, prometheus counters, and the events.jsonl debug log.
func (r *Runner) onResult(res driver.Result) {
	if res.Event == nil {
		return
	}
	// Prometheus + HDR — only count successful HTTP rounds in latency.
	pt := res.Event.Envelope.ProductType
	arch := res.Event.Archetype
	ipath := res.Event.IngestionPath

	if res.IsSuccess() {
		r.registry.EventsSent.WithLabelValues(pt, arch, ipath).Inc()
		r.registry.IngestLatency.WithLabelValues(pt, arch).Observe(res.Latency.Seconds())
		r.histMu.Lock()
		_ = r.histogram.RecordValue(res.Latency.Microseconds())
		r.histMu.Unlock()
		// Per-tenant fairness — Session 8.
		if r.perTenantStore != nil {
			r.perTenantStore.Record(res.Event.Envelope.TenantID, ipath, pt, res.Latency)
		}
	} else {
		switch {
		case res.IsExpectedFailure():
			r.registry.EventsFailed.WithLabelValues(pt, arch, "expected").Inc()
		case res.IsClientError():
			r.registry.EventsFailed.WithLabelValues(pt, arch, "client").Inc()
		case res.IsServerError():
			r.registry.EventsFailed.WithLabelValues(pt, arch, "server").Inc()
		case errors.Is(res.TransportErr, driver.ErrCircuitOpen):
			r.registry.EventsFailed.WithLabelValues(pt, arch, "circuit_open").Inc()
		case res.IsTransport():
			r.registry.EventsFailed.WithLabelValues(pt, arch, "transport").Inc()
		}
	}
	if res.Event.NegativePath != "" {
		r.registry.NegativePathTotal.WithLabelValues(string(res.Event.NegativePath)).Inc()
	}

	// Per-tenant + per-product-type + per-path counters.
	atomic64Inc(&r.perTenant, res.Event.Envelope.TenantID)
	atomic64Inc(&r.perProductType, pt)
	atomic64Inc(&r.perPath, ipath)

	// events.jsonl — capped at first eventsCap events so 7-day runs don't
	// blow disk. We log irrespective of outcome so debug includes failures.
	r.eventsBufMu.Lock()
	defer r.eventsBufMu.Unlock()
	if r.eventsCount.Load() >= int64(r.eventsCap) {
		return
	}
	rec := struct {
		EventID             string                     `json:"event_id"`
		TenantID            string                     `json:"tenant_id"`
		Archetype           string                     `json:"archetype"`
		ProductType         string                     `json:"product_type"`
		IngestionPath       string                     `json:"ingestion_path"`
		PayloadSize         generator.PayloadSize      `json:"payload_size"`
		NegativePath        generator.NegativePathKind `json:"negative_path,omitempty"`
		StaleReason         string                     `json:"stale_reason,omitempty"`
		StaleSince          *time.Time                 `json:"stale_since,omitempty"`
		StaleSubscriptionID string                     `json:"subscription_id,omitempty"`
		Status              int                        `json:"status"`
		LatencyMs           float64                    `json:"latency_ms"`
		BytesSent           int                        `json:"bytes_sent"`
		EventTimestamp      time.Time                  `json:"event_timestamp"`
	}{
		EventID:        res.Event.Envelope.EventID,
		TenantID:       res.Event.Envelope.TenantID,
		Archetype:      arch,
		ProductType:    pt,
		IngestionPath:  ipath,
		PayloadSize:    res.Event.PayloadSize,
		NegativePath:   res.Event.NegativePath,
		StaleReason:    res.Event.StaleReason,
		StaleSince:     res.Event.StaleSince,
		Status:         res.Status,
		LatencyMs:      float64(res.Latency.Microseconds()) / 1000.0,
		BytesSent:      res.BytesSent,
		EventTimestamp: res.Event.Envelope.EventTimestamp,
	}
	if res.Event.NegativePath == generator.NPStaleKey {
		rec.StaleSubscriptionID = res.Event.StaleSubscriptionID
	}
	if buf, err := json.Marshal(rec); err == nil {
		r.eventsBuffer.Write(buf)
		r.eventsBuffer.WriteByte('\n')
		r.eventsCount.Add(1)
	}
}

func (r *Runner) flushEventsLog() []byte {
	r.eventsBufMu.Lock()
	defer r.eventsBufMu.Unlock()
	out := make([]byte, r.eventsBuffer.Len())
	copy(out, r.eventsBuffer.Bytes())
	return out
}

func (r *Runner) buildResult(genErr error) *RunResult {
	stats := r.gen.Stats()
	pstats := r.pool.Stats()

	res := &RunResult{
		RunID:              r.runID,
		ScenarioName:       r.cfg.Scenario.Name,
		Target:             r.cfg.Target.Name,
		StartedAt:          r.startedAt,
		StoppedAt:          r.stoppedAt,
		Duration:           r.stoppedAt.Sub(r.startedAt),
		TargetTPS:          r.cfg.Scenario.TargetTPS,
		TenantsActive:      r.gen.TenantsActive(),
		EventsGenerated:    stats.Generated.Load(),
		EventsSubmitted:    pstats.Submitted.Load(),
		EventsSucceeded:    pstats.Succeeded.Load(),
		ClientErrors:       pstats.ClientErrors.Load(),
		ServerErrors:       pstats.ServerErrors.Load(),
		TransportFailures:  pstats.TransportFailures.Load(),
		CircuitOpenSkipped: pstats.CircuitOpenSkipped.Load(),
		ExpectedFailures:   pstats.ExpectedFailures.Load(),

		NegativePathCounts: stats.NegativeSnapshot(),
		PerArchetype:       stats.ArchetypeSnapshot(),
		PerTenant:          atomicMap(&r.perTenant),
		PerProductType:     atomicMap(&r.perProductType),
		PerIngestionPath:   atomicMap(&r.perPath),
	}
	if r.perTenantStore != nil {
		res.PerTenantP99Ms = r.perTenantStore.PerTenantP99()
		res.PerTenantPathP99Ms = r.perTenantStore.PerTenantPathBreakdown()
		fr := r.perTenantStore.FairnessReport()
		res.Fairness = &fr
		res.PerTenantHistogramsMB = float64(r.perTenantStore.MemoryFootprint()) / (1 << 20)
	}
	res.FairnessGateDeferred = r.deferredCount.Load()
	res.EventsFailed = res.ClientErrors + res.ServerErrors + res.TransportFailures + res.CircuitOpenSkipped

	r.histMu.Lock()
	if r.histogram.TotalCount() > 0 {
		res.LatencyP50ms = float64(r.histogram.ValueAtQuantile(50.0)) / 1000.0
		res.LatencyP90ms = float64(r.histogram.ValueAtQuantile(90.0)) / 1000.0
		res.LatencyP99ms = float64(r.histogram.ValueAtQuantile(99.0)) / 1000.0
		res.LatencyMaxMs = float64(r.histogram.Max()) / 1000.0
	}
	r.histMu.Unlock()

	r.bpEvtMu.Lock()
	res.BackpressureEngagedAt = append([]time.Time(nil), r.bpEngaged...)
	res.CircuitBreakerOpenedAt = append([]time.Time(nil), r.cbOpened...)
	r.bpEvtMu.Unlock()

	if genErr != nil && !errors.Is(genErr, context.Canceled) {
		res.Errors = append(res.Errors, genErr.Error())
	}
	return res
}

// MetricsAddr returns the bound metrics server address ("" if disabled).
func (r *Runner) MetricsAddr() string {
	if r.mserver == nil {
		return ""
	}
	return r.mserver.Addr()
}

// --- helpers ---

func atomic64Inc(m *sync.Map, key string) {
	if v, ok := m.Load(key); ok {
		v.(*atomic.Int64).Add(1)
		return
	}
	c := new(atomic.Int64)
	c.Store(1)
	if actual, loaded := m.LoadOrStore(key, c); loaded {
		actual.(*atomic.Int64).Add(1)
	}
}

func atomicMap(m *sync.Map) map[string]int64 {
	out := map[string]int64{}
	m.Range(func(k, v any) bool {
		out[k.(string)] = v.(*atomic.Int64).Load()
		return true
	})
	return out
}

func randomRunID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(buf)
}
