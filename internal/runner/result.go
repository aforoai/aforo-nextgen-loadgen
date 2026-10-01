// Package runner orchestrates the full Session 4 run pipeline:
//
//  1. Loads scenario + manifest.
//  2. Constructs Generator + Pacer + Pool + Driver.
//  3. Streams events through with metrics + pprof exposed.
//  4. Drains in-flight on SIGINT/SIGTERM and writes partial output.
package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// RunResult is the artifact written to <out>/run.json after the run.
//
// Format is intentionally flat + JSON-friendly so downstream validators
// (Session 5+) can consume it without dragging in this package's types.
type RunResult struct {
	RunID         string        `json:"run_id"`
	ScenarioName  string        `json:"scenario_name"`
	Target        string        `json:"target"`
	StartedAt     time.Time     `json:"started_at"`
	StoppedAt     time.Time     `json:"stopped_at"`
	Duration      time.Duration `json:"duration"`
	TargetTPS     int           `json:"target_tps"`
	TenantsActive int           `json:"tenants_active"`

	EventsGenerated int64 `json:"events_generated"`
	EventsSubmitted int64 `json:"events_submitted"`
	EventsSucceeded int64 `json:"events_succeeded"`
	EventsFailed    int64 `json:"events_failed"`

	ClientErrors       int64 `json:"client_errors"`        // 4xx (real)
	ServerErrors       int64 `json:"server_errors"`        // 5xx
	TransportFailures  int64 `json:"transport_failures"`   // DNS/dial/EOF
	CircuitOpenSkipped int64 `json:"circuit_open_skipped"` // dropped while breaker was open
	ExpectedFailures   int64 `json:"expected_failures"`    // negative-path-induced

	NegativePathCounts map[generator.NegativePathKind]int64 `json:"negative_path_counts"`
	PerArchetype       map[string]int64                     `json:"per_archetype"`
	PerTenant          map[string]int64                     `json:"per_tenant"`
	PerProductType     map[string]int64                     `json:"per_product_type"`
	PerIngestionPath   map[string]int64                     `json:"per_ingestion_path,omitempty"` // Session 8

	// PerTenantMetric is the accepted (2xx) usage per billable unit:
	// tenant_id → metric name → usage. The billing validator prices each
	// metric with its own config from this instead of pricing the tenant's
	// whole event count as one metric. Absent in run.json files written
	// before 2026-10-01 — the validator then estimates the old way.
	PerTenantMetric map[string]map[string]MetricUsage `json:"per_tenant_metric,omitempty"`

	// Session 8 — per-tenant fairness summary. Computed when the runner
	// has per-tenant histograms recorded; nil otherwise.
	PerTenantP99Ms        map[string]float64            `json:"per_tenant_p99_ms,omitempty"`
	PerTenantPathP99Ms    map[string]map[string]float64 `json:"per_tenant_path_p99_ms,omitempty"`
	Fairness              *FairnessReport               `json:"fairness,omitempty"`
	FairnessGateDeferred  int64                         `json:"fairness_gate_deferred,omitempty"`
	PerTenantHistogramsMB float64                       `json:"per_tenant_histograms_mb,omitempty"`

	LatencyP50ms float64 `json:"latency_p50_ms"`
	LatencyP90ms float64 `json:"latency_p90_ms"`
	LatencyP99ms float64 `json:"latency_p99_ms"`
	LatencyMaxMs float64 `json:"latency_max_ms"`

	BackpressureEngagedAt  []time.Time `json:"backpressure_engaged_at,omitempty"`
	CircuitBreakerOpenedAt []time.Time `json:"circuit_breaker_opened_at,omitempty"`

	Errors []string `json:"errors,omitempty"`
}

// MetricUsage is the usage loadgen sent for one billable unit of one tenant
// and the platform accepted (2xx). Rejected events — negative paths, 4xx,
// 5xx, transport failures — are not counted, because billing never sees
// them.
type MetricUsage struct {
	// Events is the number of accepted events (the unit of a COUNT metric).
	Events int64 `json:"events"`
	// Quantity is the summed Envelope.Quantity (the unit of a SUM metric).
	Quantity float64 `json:"quantity"`
	// MaxQuantity is the largest single-event quantity (a MAX metric).
	MaxQuantity float64 `json:"max_quantity,omitempty"`
	// ByStatus splits Events / Quantity by the event's execution status
	// (canonical upper-case; "" = sent without a status). OUTCOME_BASED
	// metrics are weighted from this.
	ByStatus map[string]StatusUsage `json:"by_status,omitempty"`
}

// StatusUsage is one execution-status row of MetricUsage.ByStatus.
type StatusUsage struct {
	Events   int64   `json:"events"`
	Quantity float64 `json:"quantity"`
}

// add folds one accepted event into the usage.
func (u *MetricUsage) add(quantity float64, status string) {
	u.Events++
	u.Quantity += quantity
	if quantity > u.MaxQuantity {
		u.MaxQuantity = quantity
	}
	if u.ByStatus == nil {
		u.ByStatus = map[string]StatusUsage{}
	}
	row := u.ByStatus[status]
	row.Events++
	row.Quantity += quantity
	u.ByStatus[status] = row
}

// merge folds another partition's usage for the same metric into u.
func (u *MetricUsage) merge(o MetricUsage) {
	u.Events += o.Events
	u.Quantity += o.Quantity
	if o.MaxQuantity > u.MaxQuantity {
		u.MaxQuantity = o.MaxQuantity
	}
	if len(o.ByStatus) > 0 && u.ByStatus == nil {
		u.ByStatus = map[string]StatusUsage{}
	}
	for status, row := range o.ByStatus {
		cur := u.ByStatus[status]
		cur.Events += row.Events
		cur.Quantity += row.Quantity
		u.ByStatus[status] = cur
	}
}

// metricUsageStore accumulates PerTenantMetric during a run.
type metricUsageStore struct {
	mu sync.Mutex
	m  map[string]map[string]*MetricUsage
}

func newMetricUsageStore() *metricUsageStore {
	return &metricUsageStore{m: map[string]map[string]*MetricUsage{}}
}

// Record adds one accepted event. Events without a tenant or metric name
// are ignored — there is nothing to attribute them to.
func (s *metricUsageStore) Record(tenantID, metric string, quantity float64, status string) {
	if tenantID == "" || metric == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byMetric := s.m[tenantID]
	if byMetric == nil {
		byMetric = map[string]*MetricUsage{}
		s.m[tenantID] = byMetric
	}
	u := byMetric[metric]
	if u == nil {
		u = &MetricUsage{}
		byMetric[metric] = u
	}
	u.add(quantity, status)
}

// Snapshot returns a deep copy, or nil when nothing was recorded (so
// run.json omits the field).
func (s *metricUsageStore) Snapshot() map[string]map[string]MetricUsage {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.m) == 0 {
		return nil
	}
	out := make(map[string]map[string]MetricUsage, len(s.m))
	for tenant, byMetric := range s.m {
		cp := make(map[string]MetricUsage, len(byMetric))
		for metric, u := range byMetric {
			var c MetricUsage
			c.merge(*u)
			cp[metric] = c
		}
		out[tenant] = cp
	}
	return out
}

// mergeMetricUsage folds src into dst (allocating dst when needed) and
// returns it.
func mergeMetricUsage(dst, src map[string]map[string]MetricUsage) map[string]map[string]MetricUsage {
	if len(src) == 0 {
		return dst
	}
	if dst == nil {
		dst = make(map[string]map[string]MetricUsage, len(src))
	}
	for tenant, byMetric := range src {
		cur := dst[tenant]
		if cur == nil {
			cur = make(map[string]MetricUsage, len(byMetric))
			dst[tenant] = cur
		}
		for metric, u := range byMetric {
			c := cur[metric]
			c.merge(u)
			cur[metric] = c
		}
	}
	return dst
}

// Save writes the run result + ancillary artifacts to a directory.
//
//	<out>/run.json           — this struct, pretty-printed
//	<out>/per_archetype.json — flat map for dashboards
//	<out>/latencies.hdr      — HDR histogram binary
//	<out>/events.jsonl       — first 1000 events, JSON lines
//	<out>/scenario.yaml      — copy of the scenario for replay
func (r *RunResult) Save(out string, hist *hdrhistogram.Histogram, eventsLog []byte, scenarioYAML []byte) error {
	if out == "" {
		return fmt.Errorf("runner: output dir is empty")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("runner: mkdir %s: %w", out, err)
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return fmt.Errorf("runner: marshal run.json: %w", err)
	}
	if err := os.WriteFile(filepath.Join(out, "run.json"), data, 0o644); err != nil {
		return fmt.Errorf("runner: write run.json: %w", err)
	}

	// per_archetype.json — sorted for stable output across runs.
	perArch := struct {
		ByArchetype map[string]int64 `json:"by_archetype"`
	}{ByArchetype: r.PerArchetype}
	sortedKeys := make([]string, 0, len(r.PerArchetype))
	for k := range r.PerArchetype {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	archData, err := json.MarshalIndent(perArch, "", "  ")
	if err != nil {
		return fmt.Errorf("runner: marshal per_archetype: %w", err)
	}
	if err := os.WriteFile(filepath.Join(out, "per_archetype.json"), archData, 0o644); err != nil {
		return fmt.Errorf("runner: write per_archetype.json: %w", err)
	}

	if len(eventsLog) > 0 {
		if err := os.WriteFile(filepath.Join(out, "events.jsonl"), eventsLog, 0o644); err != nil {
			return fmt.Errorf("runner: write events.jsonl: %w", err)
		}
	}

	if hist != nil {
		// HDR's V2 binary encoding — tiny + lossless.
		if buf, err := encodeHistogram(hist); err != nil {
			return fmt.Errorf("runner: encode histogram: %w", err)
		} else if err := os.WriteFile(filepath.Join(out, "latencies.hdr"), buf, 0o644); err != nil {
			return fmt.Errorf("runner: write latencies.hdr: %w", err)
		}
	}

	if len(scenarioYAML) > 0 {
		if err := os.WriteFile(filepath.Join(out, "scenario.yaml"), scenarioYAML, 0o644); err != nil {
			return fmt.Errorf("runner: write scenario.yaml: %w", err)
		}
	}
	return nil
}

// encodeHistogram serializes the HDR histogram bins to a portable form.
// Format: { "lowest_trackable":N, "highest_trackable":N, "significant":N,
//
//	"values": [{ "value":N, "count":N }, ...] }
//
// The raw HDR V2 binary encoding requires importing internal helpers; this
// JSON shape is enough for run-to-run comparison + downstream tooling and
// is round-trippable for replay tests.
func encodeHistogram(h *hdrhistogram.Histogram) ([]byte, error) {
	out := struct {
		LowestTrackable  int64      `json:"lowest_trackable"`
		HighestTrackable int64      `json:"highest_trackable"`
		Significant      int        `json:"significant"`
		TotalCount       int64      `json:"total_count"`
		Min              int64      `json:"min"`
		Max              int64      `json:"max"`
		Mean             float64    `json:"mean"`
		StdDev           float64    `json:"std_dev"`
		Bars             []histoBar `json:"bars"`
	}{
		LowestTrackable:  h.LowestTrackableValue(),
		HighestTrackable: h.HighestTrackableValue(),
		Significant:      int(h.SignificantFigures()),
		TotalCount:       h.TotalCount(),
		Min:              h.Min(),
		Max:              h.Max(),
		Mean:             h.Mean(),
		StdDev:           h.StdDev(),
	}
	for _, b := range h.Distribution() {
		if b.Count == 0 {
			continue
		}
		out.Bars = append(out.Bars, histoBar{From: b.From, To: b.To, Count: b.Count})
	}
	return json.MarshalIndent(out, "", "  ")
}

type histoBar struct {
	From  int64 `json:"from"`
	To    int64 `json:"to"`
	Count int64 `json:"count"`
}
