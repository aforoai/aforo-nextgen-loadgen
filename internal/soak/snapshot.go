// Package soak emits periodic JSON snapshots during long-running runs and
// detects regression-class anomalies in the metrics trend.
//
// Why this exists separately from the run.json artifact:
//
//   - run.json is written ONCE at the end. A 7-day run that goes wrong on
//     day 4 leaves the operator with no early signal until the run ends —
//     too late to abort and save cost.
//   - Daily snapshots persist enough state mid-run that an aborted run
//     still has a recoverable per-day record to debug from.
//   - Anomaly detection compares each new snapshot against the trailing
//     window: if p99 latency is trending up >10% over 24h, alert. The
//     thresholds are tuned for the headline 15K TPS / 7-day profile and
//     are conservative to avoid pager fatigue.
//
// What this package does NOT do:
//
//   - It does not abort the run. Anomaly detection emits an Alert; the
//     caller (the run engine) decides whether to abort, downscale TPS,
//     or just log it.
//   - It does not page anyone. PagerDuty / Slack integration is the
//     ops runbook's responsibility — soak just produces structured
//     output that can be tailed by an existing alerting agent.
package soak

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// Snapshot is one periodic record of the live run state. The Monitor
// holds a sliding window of these for trend analysis. Fields are
// deliberately scalar so JSON files stay small (a 7-day run accumulates
// ~168 hourly snapshots = ~30KB per run).
type Snapshot struct {
	// At is the wall-clock time the snapshot was taken.
	At time.Time `json:"at"`

	// RunOffset is the time elapsed since the run start.
	RunOffset time.Duration `json:"run_offset"`

	// EventsIngested is the cumulative success count up to At.
	EventsIngested int64 `json:"events_ingested"`

	// EventsFailed is the cumulative failure count (any flavor).
	EventsFailed int64 `json:"events_failed"`

	// CurrentTPS is the observed TPS in the last reporting interval —
	// (events_in_interval / interval_seconds).
	CurrentTPS float64 `json:"current_tps"`

	// LatencyP50Ms / P99Ms are the latency percentiles measured over the
	// trailing window the caller chose (typically 1h). The Monitor holds
	// the values raw — does not re-compute.
	LatencyP50Ms float64 `json:"latency_p50_ms"`
	LatencyP99Ms float64 `json:"latency_p99_ms"`

	// CircuitOpenSkipped / TransportFailures are infrastructure-health
	// signals. A spike in these often precedes a p99 climb and gives the
	// anomaly detector something to grade against.
	CircuitOpenSkipped int64 `json:"circuit_open_skipped"`
	TransportFailures  int64 `json:"transport_failures"`

	// CostUSD is the running estimate at this point in the soak. From
	// the cost.Tracker. Lets operators track $/hour live.
	CostUSD float64 `json:"cost_usd"`
}

// Severity grades an Alert. The Monitor never panics; the caller decides
// what to do with high-severity alerts.
type Severity string

const (
	SeverityInfo     Severity = "INFO"
	SeverityWarning  Severity = "WARN"
	SeverityCritical Severity = "CRITICAL"
)

// Alert is one anomaly the Monitor detected. Embedded into the soak
// timeline JSON so the post-run report shows the trail of warnings
// alongside the chaos events.
type Alert struct {
	At        time.Time `json:"at"`
	Severity  Severity  `json:"severity"`
	Kind      string    `json:"kind"`
	Message   string    `json:"message"`
	BaselineP99Ms float64 `json:"baseline_p99_ms,omitempty"`
	CurrentP99Ms  float64 `json:"current_p99_ms,omitempty"`
	DriftPct      float64 `json:"drift_pct,omitempty"`
}

// MonitorConfig is the construction-time bag.
type MonitorConfig struct {
	// OutputDir is where daily snapshot files are written.
	// Format: <out>/snapshots/snapshot-YYYY-MM-DDTHH-MM-SSZ.json
	OutputDir string

	// SnapshotInterval is how often Take() fires when called by the run
	// engine. Default 1h. The monitor itself is not a scheduler — the
	// caller decides cadence; SnapshotInterval is informational and
	// embedded into the summary so consumers see the cadence the run
	// was configured for.
	SnapshotInterval time.Duration

	// AnomalyWindow is the trailing window over which baselines are
	// computed. Default 24h. p99 drift is measured between (median p99
	// over the window) and the latest snapshot's p99.
	AnomalyWindow time.Duration

	// P99DriftThresholdPct triggers an Alert when current p99 exceeds
	// the baseline by this fraction. Default 0.10 = 10%.
	P99DriftThresholdPct float64

	// FailureRateThresholdPct triggers an Alert when failed/total in
	// the latest snapshot exceeds this. Default 0.01 = 1%.
	FailureRateThresholdPct float64

	// Logger receives a one-line summary on each Take(). Useful for
	// stdout tailing during a long soak.
	Logger func(format string, args ...any)
}

// Monitor accumulates snapshots and runs the anomaly detector on each
// new one. Safe for one-writer multi-reader concurrency: Take() and
// Snapshots()/Alerts() can be called from different goroutines, but only
// one caller should invoke Take().
type Monitor struct {
	cfg       MonitorConfig
	mu        sync.Mutex
	snapshots []Snapshot
	alerts    []Alert
}

// NewMonitor constructs a Monitor with the given config.
func NewMonitor(cfg MonitorConfig) *Monitor {
	if cfg.SnapshotInterval <= 0 {
		cfg.SnapshotInterval = time.Hour
	}
	if cfg.AnomalyWindow <= 0 {
		cfg.AnomalyWindow = 24 * time.Hour
	}
	if cfg.P99DriftThresholdPct <= 0 {
		cfg.P99DriftThresholdPct = 0.10
	}
	if cfg.FailureRateThresholdPct <= 0 {
		cfg.FailureRateThresholdPct = 0.01
	}
	if cfg.Logger == nil {
		cfg.Logger = func(string, ...any) {}
	}
	return &Monitor{cfg: cfg}
}

// Take records a snapshot and runs the anomaly detector. Returns the
// list of Alerts produced by THIS snapshot — typically empty. The
// returned slice is a copy; callers are free to mutate.
//
// Persisting the snapshot to disk is best-effort: if the write fails the
// in-memory record still updates so the monitor can keep producing
// alerts. The error is returned to the caller so they can log it; soak
// monitoring should not crash a 7-day run because /var is full.
func (m *Monitor) Take(snap Snapshot) ([]Alert, error) {
	m.mu.Lock()
	m.snapshots = append(m.snapshots, snap)
	newAlerts := m.detect(snap)
	m.alerts = append(m.alerts, newAlerts...)
	m.mu.Unlock()

	for _, a := range newAlerts {
		m.cfg.Logger("soak: %s [%s] %s", a.Severity, a.Kind, a.Message)
	}
	m.cfg.Logger("soak: snapshot run+%s tps=%.0f p99=%.1fms cost=$%.2f",
		snap.RunOffset, snap.CurrentTPS, snap.LatencyP99Ms, snap.CostUSD)

	if err := m.persist(snap, newAlerts); err != nil {
		return cloneAlerts(newAlerts), fmt.Errorf("soak: persist snapshot: %w", err)
	}
	return cloneAlerts(newAlerts), nil
}

// Snapshots returns a copy of the recorded snapshots in chronological order.
func (m *Monitor) Snapshots() []Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Snapshot, len(m.snapshots))
	copy(out, m.snapshots)
	return out
}

// Alerts returns a copy of the recorded alerts in chronological order.
func (m *Monitor) Alerts() []Alert {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Alert, len(m.alerts))
	copy(out, m.alerts)
	return out
}

// Save writes a final summary file at <out>/soak-summary.json containing
// every snapshot + alert. Called by the run engine on shutdown.
func (m *Monitor) Save(out string) error {
	if out == "" {
		return fmt.Errorf("soak: output dir is empty")
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("soak: mkdir %s: %w", out, err)
	}
	m.mu.Lock()
	summary := struct {
		SnapshotInterval time.Duration `json:"snapshot_interval"`
		AnomalyWindow    time.Duration `json:"anomaly_window"`
		Snapshots        []Snapshot    `json:"snapshots"`
		Alerts           []Alert       `json:"alerts"`
	}{
		SnapshotInterval: m.cfg.SnapshotInterval,
		AnomalyWindow:    m.cfg.AnomalyWindow,
		Snapshots:        append([]Snapshot(nil), m.snapshots...),
		Alerts:           append([]Alert(nil), m.alerts...),
	}
	m.mu.Unlock()
	buf, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return fmt.Errorf("soak: marshal summary: %w", err)
	}
	return os.WriteFile(filepath.Join(out, "soak-summary.json"), buf, 0o644)
}

// detect runs the anomaly detector on the latest snapshot using the
// trailing window's median p99 as baseline. Caller holds m.mu.
func (m *Monitor) detect(latest Snapshot) []Alert {
	var alerts []Alert

	// Failure-rate gate: latest snapshot's failure rate.
	totalRecent := latest.EventsIngested + latest.EventsFailed
	if totalRecent > 0 {
		rate := float64(latest.EventsFailed) / float64(totalRecent)
		if rate > m.cfg.FailureRateThresholdPct {
			alerts = append(alerts, Alert{
				At:       latest.At,
				Severity: SeverityWarning,
				Kind:     "high_failure_rate",
				Message: fmt.Sprintf("failure rate %.3f%% exceeds threshold %.3f%% (failed=%d, ingested=%d)",
					rate*100, m.cfg.FailureRateThresholdPct*100, latest.EventsFailed, latest.EventsIngested),
			})
		}
	}

	// p99 drift: compare latest p99 to the median p99 over the AnomalyWindow.
	baseline, ok := m.medianP99WithinWindow(latest.At)
	if ok && baseline > 0 {
		drift := (latest.LatencyP99Ms - baseline) / baseline
		if drift > m.cfg.P99DriftThresholdPct {
			sev := SeverityWarning
			// Drift > 25% is critical (typically a real regression, not
			// noise from a single GC pause).
			if drift > 0.25 {
				sev = SeverityCritical
			}
			alerts = append(alerts, Alert{
				At:            latest.At,
				Severity:      sev,
				Kind:          "p99_drift",
				Message:       fmt.Sprintf("p99 latency %.1fms is %.1f%% above baseline %.1fms (window=%s)", latest.LatencyP99Ms, drift*100, baseline, m.cfg.AnomalyWindow),
				BaselineP99Ms: baseline,
				CurrentP99Ms:  latest.LatencyP99Ms,
				DriftPct:      drift,
			})
		}
	}

	return alerts
}

// medianP99WithinWindow returns the median p99 across snapshots whose At
// falls in [latest.At - AnomalyWindow, latest.At). Excludes the latest
// snapshot itself so we measure drift against the trailing baseline.
// Returns (0, false) when fewer than 3 snapshots are in the window —
// not enough data to grade.
func (m *Monitor) medianP99WithinWindow(now time.Time) (float64, bool) {
	cutoff := now.Add(-m.cfg.AnomalyWindow)
	var values []float64
	for _, s := range m.snapshots {
		if s.At.After(cutoff) && s.At.Before(now) {
			values = append(values, s.LatencyP99Ms)
		}
	}
	if len(values) < 3 {
		return 0, false
	}
	sort.Float64s(values)
	mid := len(values) / 2
	if len(values)%2 == 0 {
		return (values[mid-1] + values[mid]) / 2, true
	}
	return values[mid], true
}

// persist writes a daily snapshot file. The file name embeds the
// snapshot timestamp so a 7-day run has 168 well-named files in
// chronological order. Each file is a self-contained JSON document with
// the snapshot + any alerts produced this tick.
func (m *Monitor) persist(snap Snapshot, alerts []Alert) error {
	if m.cfg.OutputDir == "" {
		return nil // best-effort — caller didn't ask for disk
	}
	dir := filepath.Join(m.cfg.OutputDir, "snapshots")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	stamp := snap.At.UTC().Format("2006-01-02T15-04-05Z")
	doc := struct {
		Snapshot Snapshot `json:"snapshot"`
		Alerts   []Alert  `json:"alerts,omitempty"`
	}{Snapshot: snap, Alerts: alerts}
	buf, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "snapshot-"+stamp+".json"), buf, 0o644)
}

func cloneAlerts(in []Alert) []Alert {
	if len(in) == 0 {
		return nil
	}
	out := make([]Alert, len(in))
	copy(out, in)
	return out
}
