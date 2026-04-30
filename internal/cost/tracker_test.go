package cost

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

func TestEstimateZeroRunWindowReturnsZeros(t *testing.T) {
	tr := NewTracker(DefaultRates)
	bd := tr.Estimate()
	if bd.TotalUSD != 0 {
		t.Fatalf("zero window must yield zero cost; got %f", bd.TotalUSD)
	}
	if !bd.IsEstimate {
		t.Fatalf("breakdown must always be labeled as estimate")
	}
	if !strings.Contains(strings.ToLower(bd.EstimateNote), "estimate") {
		t.Fatalf("estimate note must clearly state estimate; got %q", bd.EstimateNote)
	}
}

func TestEstimateScalesLinearlyWithHours(t *testing.T) {
	tr := NewTracker(DefaultRates)
	tr.SetWorkerCount(8)
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	tr.Start(start)
	tr.Stop(start.Add(1 * time.Hour))
	one := tr.Estimate()

	tr2 := NewTracker(DefaultRates)
	tr2.SetWorkerCount(8)
	tr2.Start(start)
	tr2.Stop(start.Add(2 * time.Hour))
	two := tr2.Estimate()

	// Compute cost should double; egress depends on event count which is
	// zero here so totals come from compute + fixed-rate infra.
	if !approxEq(two.WorkerComputeUSD, 2*one.WorkerComputeUSD, 1e-6) {
		t.Fatalf("worker compute should scale linearly: 1h=%f 2h=%f", one.WorkerComputeUSD, two.WorkerComputeUSD)
	}
	if !approxEq(two.KafkaMSKUSD, 2*one.KafkaMSKUSD, 1e-6) {
		t.Fatalf("MSK cost should scale linearly")
	}
}

func TestEstimateEgressIsEventDriven(t *testing.T) {
	tr := NewTracker(DefaultRates)
	tr.SetWorkerCount(1)
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tr.Start(start)
	tr.Stop(start.Add(1 * time.Hour))
	tr.AddEventsIngested(2_000_000_000) // 2B events × 500B = 1TB egress

	bd := tr.Estimate()
	expectedGB := float64(2_000_000_000) * 500.0 / float64(1<<30)
	if !approxEq(bd.EgressGB, expectedGB, 0.001) {
		t.Fatalf("egress GB: want ~%f got %f", expectedGB, bd.EgressGB)
	}
	expectedEgressUSD := expectedGB * DefaultRates.EgressUSDPerGB
	if !approxEq(bd.EgressUSD, expectedEgressUSD, 0.01) {
		t.Fatalf("egress USD: want ~%f got %f", expectedEgressUSD, bd.EgressUSD)
	}
}

func TestPerMillionEventsHeadlineMetric(t *testing.T) {
	tr := NewTracker(DefaultRates)
	tr.SetWorkerCount(8)
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tr.Start(start)
	tr.Stop(start.Add(7 * 24 * time.Hour)) // 7 days
	tr.AddEventsIngested(9_000_000_000)    // ~15K TPS × 168h ≈ 9B
	tr.IncludeStorage = true

	bd := tr.Estimate()

	// Sanity: total cost in the thousands for a week-long 8-worker run.
	if bd.TotalUSD < 100 || bd.TotalUSD > 100_000 {
		t.Fatalf("total USD %f outside believable [100, 100k] range — rates may be wrong", bd.TotalUSD)
	}
	// Per-million-events should be in the cents-to-dollars range, not
	// hundreds of dollars per million.
	if bd.PerMillionEventsUSD < 0.01 || bd.PerMillionEventsUSD > 50 {
		t.Fatalf("per-million-events USD %f outside believable [0.01, 50] — math is suspect", bd.PerMillionEventsUSD)
	}
	// Storage line must be present when IncludeStorage=true.
	if bd.ClickHouseStorageGB <= 0 {
		t.Fatalf("expected ClickHouse storage line when IncludeStorage=true")
	}
}

func TestPreflightEstimate(t *testing.T) {
	bd := PreflightEstimate(DefaultRates, 15000, 7*24*time.Hour, 8)
	if bd.WorkerCount != 8 {
		t.Fatalf("worker count: %d", bd.WorkerCount)
	}
	if bd.EventsIngested != int64(15000*7*24*3600) {
		t.Fatalf("expected events: %d", bd.EventsIngested)
	}
	if bd.TotalUSD <= 0 {
		t.Fatalf("preflight total must be > 0")
	}
	if !bd.IsEstimate {
		t.Fatalf("preflight must be flagged as estimate")
	}
}

func TestBreakdownJSONRoundtrip(t *testing.T) {
	tr := NewTracker(DefaultRates)
	tr.SetWorkerCount(4)
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tr.Start(start)
	tr.Stop(start.Add(30 * time.Minute))
	tr.AddEventsIngested(10_000_000)
	bd := tr.Estimate()

	buf, err := json.Marshal(bd)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// is_estimate must be present in JSON for downstream consumers.
	if !strings.Contains(string(buf), `"is_estimate":true`) {
		t.Fatalf("is_estimate missing from JSON: %s", buf)
	}
	if !strings.Contains(string(buf), `"per_million_events_usd"`) {
		t.Fatalf("per_million_events_usd missing from JSON")
	}
}

func TestZeroEventsIngestedSkipsPerMillion(t *testing.T) {
	tr := NewTracker(DefaultRates)
	tr.SetWorkerCount(1)
	start := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tr.Start(start)
	tr.Stop(start.Add(1 * time.Hour))
	bd := tr.Estimate()
	if bd.PerMillionEventsUSD != 0 {
		t.Fatalf("zero events must yield zero per-million; got %f", bd.PerMillionEventsUSD)
	}
}

func TestEstimateNoteIncludesRegion(t *testing.T) {
	custom := DefaultRates
	custom.Region = "eu-west-1"
	tr := NewTracker(custom)
	bd := tr.Estimate()
	if !strings.Contains(bd.EstimateNote, "eu-west-1") {
		t.Fatalf("estimate note must include region: %q", bd.EstimateNote)
	}
}

func approxEq(a, b, tol float64) bool {
	return math.Abs(a-b) <= tol
}
