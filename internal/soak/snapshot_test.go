package soak

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMonitorTakeRecordsSnapshotAndPersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	m := NewMonitor(MonitorConfig{
		OutputDir:        dir,
		SnapshotInterval: 1 * time.Hour,
	})
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	snap := Snapshot{
		At:             at,
		RunOffset:      1 * time.Hour,
		EventsIngested: 50_000_000,
		LatencyP50Ms:   12,
		LatencyP99Ms:   80,
		CurrentTPS:     14_500,
		CostUSD:        13.42,
	}
	alerts, err := m.Take(snap)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(alerts) != 0 {
		t.Fatalf("first snapshot should produce no alerts; got %v", alerts)
	}
	if got := m.Snapshots(); len(got) != 1 {
		t.Fatalf("expected 1 snapshot in monitor; got %d", len(got))
	}

	// Disk record must exist.
	entries, err := os.ReadDir(filepath.Join(dir, "snapshots"))
	if err != nil {
		t.Fatalf("readdir snapshots: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected one snapshot file on disk; got %d", len(entries))
	}
}

func TestP99DriftDetectionAtTenPercent(t *testing.T) {
	m := NewMonitor(MonitorConfig{
		AnomalyWindow:        24 * time.Hour,
		P99DriftThresholdPct: 0.10,
	})
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// 12 snapshots over 12h, all p99=80ms — establishes the baseline.
	for i := 0; i < 12; i++ {
		_, _ = m.Take(Snapshot{
			At:             base.Add(time.Duration(i) * time.Hour),
			RunOffset:      time.Duration(i) * time.Hour,
			LatencyP99Ms:   80,
			EventsIngested: int64(i+1) * 1_000_000,
		})
	}
	// 13th snapshot p99=92 (15% drift) → alert.
	alerts, err := m.Take(Snapshot{
		At:             base.Add(13 * time.Hour),
		RunOffset:      13 * time.Hour,
		LatencyP99Ms:   92,
		EventsIngested: 14_000_000,
	})
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	if len(alerts) == 0 {
		t.Fatal("expected p99 drift alert at 15% drift")
	}
	hit := false
	for _, a := range alerts {
		if a.Kind == "p99_drift" {
			hit = true
			if a.DriftPct < 0.10 || a.DriftPct > 0.20 {
				t.Fatalf("drift %f outside expected ~0.15 ± noise", a.DriftPct)
			}
		}
	}
	if !hit {
		t.Fatalf("p99_drift alert kind missing; got: %v", alerts)
	}
}

func TestP99DriftCriticalAtTwentyFivePercent(t *testing.T) {
	m := NewMonitor(MonitorConfig{
		AnomalyWindow:        24 * time.Hour,
		P99DriftThresholdPct: 0.10,
	})
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 6; i++ {
		_, _ = m.Take(Snapshot{
			At:           base.Add(time.Duration(i) * time.Hour),
			LatencyP99Ms: 100,
		})
	}
	alerts, _ := m.Take(Snapshot{
		At:           base.Add(7 * time.Hour),
		LatencyP99Ms: 140, // 40% above baseline
	})
	for _, a := range alerts {
		if a.Kind == "p99_drift" && a.Severity != SeverityCritical {
			t.Fatalf("40%% drift should be CRITICAL, got %s", a.Severity)
		}
	}
}

func TestNoAlertsBelowMinimumWindowSamples(t *testing.T) {
	m := NewMonitor(MonitorConfig{
		AnomalyWindow:        24 * time.Hour,
		P99DriftThresholdPct: 0.10,
	})
	base := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// Only 2 prior snapshots (need ≥3 in window).
	_, _ = m.Take(Snapshot{At: base.Add(0), LatencyP99Ms: 80})
	_, _ = m.Take(Snapshot{At: base.Add(1 * time.Hour), LatencyP99Ms: 80})
	alerts, _ := m.Take(Snapshot{At: base.Add(2 * time.Hour), LatencyP99Ms: 200})
	for _, a := range alerts {
		if a.Kind == "p99_drift" {
			t.Fatalf("must not alert with <3 baseline samples; got %v", alerts)
		}
	}
}

func TestFailureRateAlertWhenAboveThreshold(t *testing.T) {
	m := NewMonitor(MonitorConfig{
		FailureRateThresholdPct: 0.01, // 1%
	})
	at := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	alerts, _ := m.Take(Snapshot{
		At:             at,
		EventsIngested: 990_000,
		EventsFailed:   20_000, // ~2% failure rate
	})
	hit := false
	for _, a := range alerts {
		if a.Kind == "high_failure_rate" {
			hit = true
		}
	}
	if !hit {
		t.Fatal("expected high_failure_rate alert")
	}
}

func TestNoFailureAlertAtZeroEvents(t *testing.T) {
	m := NewMonitor(MonitorConfig{FailureRateThresholdPct: 0.01})
	alerts, _ := m.Take(Snapshot{
		At:             time.Now(),
		EventsIngested: 0,
		EventsFailed:   0,
	})
	for _, a := range alerts {
		if a.Kind == "high_failure_rate" {
			t.Fatal("zero events must not trigger failure-rate alert")
		}
	}
}

func TestSaveWritesSummaryJSON(t *testing.T) {
	dir := t.TempDir()
	m := NewMonitor(MonitorConfig{OutputDir: dir})
	_, _ = m.Take(Snapshot{
		At:           time.Now(),
		LatencyP99Ms: 50,
	})
	if err := m.Save(dir); err != nil {
		t.Fatal(err)
	}
	buf, err := os.ReadFile(filepath.Join(dir, "soak-summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(buf, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc["snapshots"]; !ok {
		t.Fatal("summary missing snapshots key")
	}
	if !strings.Contains(string(buf), "anomaly_window") {
		t.Fatal("summary missing anomaly_window key")
	}
}

func TestMonitorIsConcurrencySafeForReadDuringTake(t *testing.T) {
	m := NewMonitor(MonitorConfig{})
	done := make(chan struct{})
	go func() {
		for i := 0; i < 50; i++ {
			_, _ = m.Take(Snapshot{At: time.Now(), LatencyP99Ms: float64(i)})
		}
		close(done)
	}()
	for {
		_ = m.Snapshots()
		_ = m.Alerts()
		select {
		case <-done:
			return
		default:
		}
	}
}
