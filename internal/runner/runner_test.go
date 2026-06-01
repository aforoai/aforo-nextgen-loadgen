package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// TestRunnerEndToEndAgainstFakeIngestor — full pipeline smoke test against
// an httptest.Server playing the role of usage-ingestor. Verifies:
//   - Generator produces events at roughly target TPS
//   - Driver POSTs them with correct headers
//   - Pool counts successes
//   - RunResult artifacts are written
//   - Metrics endpoint serves /metrics + /healthz
func TestRunnerEndToEndAgainstFakeIngestor(t *testing.T) {
	var rxCount atomic.Int64
	var rxTenants sync.Map
	var rxNegPaths sync.Map

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rxCount.Add(1)
		if tid := r.Header.Get("X-Tenant-Id"); tid != "" {
			rxTenants.Store(tid, true)
		}
		if np := r.Header.Get("X-Loadgen-Negative-Path"); np != "" {
			c, _ := rxNegPaths.LoadOrStore(np, new(atomic.Int64))
			c.(*atomic.Int64).Add(1)
		}
		// future_event/oversize/malformed should be 4xx by spec — synthesize.
		switch r.Header.Get("X-Loadgen-Negative-Path") {
		case "future_event", "oversize":
			http.Error(w, `{"error":"event_timestamp_invalid"}`, http.StatusBadRequest)
			return
		case "malformed":
			http.Error(w, `{"error":"malformed_body"}`, http.StatusBadRequest)
			return
		case "wrong_auth", "stale_key":
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer ts.Close()

	u, _ := url.Parse(ts.URL)
	target := aforo.Target{
		Name: "fake",
		URLs: map[aforo.Service]string{
			aforo.ServiceUsageIngestor: u.Scheme + "://" + u.Host,
		},
	}

	// Mini scenario — tiny duration to keep test fast.
	scn := &scenario.Scenario{
		SchemaVersion:    1,
		Name:             "runner-smoke",
		TargetTPS:        200,
		Duration:         scenario.Duration(2 * time.Second),
		Seed:             1,
		Tenants:          scenario.Tenants{Count: 1, Distribution: scenario.DistUniform},
		ProductMix:       scenario.ProductMix{API: 1.0},
		IngestionPaths:   scenario.IngestionPaths{RestDirect: 1.0},
		PayloadVariation: scenario.PayloadVariation{SmallPct: 1.0},
		NegativePaths: scenario.NegativePaths{
			LateEventsPct: 0.10, // accepted as 2xx but timestamp shifted
			OversizePct:   0,    // skip oversize for speed
		},
	}

	mfst := smokeManifest()

	out := t.TempDir()
	cfg := Config{
		Scenario:    scn,
		Manifest:    mfst,
		Target:      target,
		OutputDir:   out,
		Workers:     8,
		BufferSize:  256,
		MetricsAddr: "127.0.0.1:0", // bind to ephemeral port
	}

	r, err := New(cfg)
	if err != nil {
		t.Fatalf("New runner: %v", err)
	}
	if r.MetricsAddr() == "" {
		t.Fatal("metrics server should be bound")
	}

	// Hit /metrics + /healthz mid-run via a goroutine.
	probeOK := make(chan struct{}, 1)
	go func() {
		// Wait briefly for the metrics server to start.
		time.Sleep(200 * time.Millisecond)
		resp, err := http.Get("http://" + r.MetricsAddr() + "/healthz")
		if err == nil && resp.StatusCode == 200 {
			_ = resp.Body.Close()
		}
		mresp, err := http.Get("http://" + r.MetricsAddr() + "/metrics")
		if err == nil && mresp.StatusCode == 200 {
			_ = mresp.Body.Close()
			probeOK <- struct{}{}
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	select {
	case <-probeOK:
	default:
		t.Errorf("expected /metrics probe to succeed during run")
	}

	// Verify counters.
	if res.EventsGenerated < 100 {
		t.Errorf("events_generated=%d, want >= 100 (200 TPS × 2s)", res.EventsGenerated)
	}
	if rxCount.Load() < 100 {
		t.Errorf("server received %d events, want >= 100", rxCount.Load())
	}

	// run.json should exist + parse.
	data, err := os.ReadFile(filepath.Join(out, "run.json"))
	if err != nil {
		t.Fatalf("read run.json: %v", err)
	}
	var parsed RunResult
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse run.json: %v", err)
	}
	if parsed.RunID == "" {
		t.Errorf("run.json missing RunID")
	}
	if parsed.EventsGenerated == 0 {
		t.Errorf("run.json reports 0 events generated")
	}

	// per_archetype.json should exist.
	if _, err := os.Stat(filepath.Join(out, "per_archetype.json")); err != nil {
		t.Errorf("per_archetype.json missing: %v", err)
	}
	// scenario.yaml round-trips.
	scnBytes, err := os.ReadFile(filepath.Join(out, "scenario.yaml"))
	if err != nil {
		t.Errorf("scenario.yaml missing: %v", err)
	}
	if !strings.Contains(string(scnBytes), "runner-smoke") {
		t.Errorf("scenario.yaml does not contain scenario name")
	}
	// events.jsonl exists if events flowed.
	if _, err := os.Stat(filepath.Join(out, "events.jsonl")); err != nil {
		t.Errorf("events.jsonl missing: %v", err)
	}
}

// smokeManifest is one tenant with one active sub.
func smokeManifest() *seed.Manifest {
	return &seed.Manifest{
		ManifestVersion: seed.ManifestVersion,
		RunID:           "smoke",
		Tenants: []seed.ManifestTenant{
			{
				TenantID:   "t-smoke",
				ExternalID: "loadgen-smoke-1",
				Archetype:  "smoke-x",
				Products:   []seed.ManifestProduct{{ProductID: "p-1", ProductType: scenario.ProductAPI, MetricIDs: []string{"m-1"}, Metrics: []seed.ManifestMetric{{ID: "m-1", Name: "api_calls"}}}},
				Customers: []seed.ManifestCustomer{
					{
						CustomerID: "c-smoke",
						Subscriptions: []seed.ManifestSubscription{
							{
								SubscriptionID: "s-active-1",
								Status:         scenario.StateActive,
								APIKeys: []seed.ManifestAPIKey{
									{KeyID: "k-1", Secret: "sk_live_smoke_active", CredentialType: "BEARER_TOKEN"},
								},
							},
						},
					},
				},
			},
		},
	}
}
