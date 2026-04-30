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

	// Session 8 — per-tenant fairness summary. Computed when the runner
	// has per-tenant histograms recorded; nil otherwise.
	PerTenantP99Ms        map[string]float64           `json:"per_tenant_p99_ms,omitempty"`
	PerTenantPathP99Ms    map[string]map[string]float64 `json:"per_tenant_path_p99_ms,omitempty"`
	Fairness              *FairnessReport              `json:"fairness,omitempty"`
	FairnessGateDeferred  int64                        `json:"fairness_gate_deferred,omitempty"`
	PerTenantHistogramsMB float64                      `json:"per_tenant_histograms_mb,omitempty"`

	LatencyP50ms float64 `json:"latency_p50_ms"`
	LatencyP90ms float64 `json:"latency_p90_ms"`
	LatencyP99ms float64 `json:"latency_p99_ms"`
	LatencyMaxMs float64 `json:"latency_max_ms"`

	BackpressureEngagedAt  []time.Time `json:"backpressure_engaged_at,omitempty"`
	CircuitBreakerOpenedAt []time.Time `json:"circuit_breaker_opened_at,omitempty"`

	Errors []string `json:"errors,omitempty"`
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
