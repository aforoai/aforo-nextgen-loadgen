package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// fakeIngestorWithKeyAuth answers like usage-ingestor's ApiKeyAuthFilter:
// only the organization-service ingest key is accepted; the per-subscription
// pricing-service key (sent as Bearer) is unknown to org-service → 401.
// That 401 is the failure every pre-fix run would have hit on current code.
func fakeIngestorWithKeyAuth(t *testing.T, ingestKey string, singles, batches, events *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != ingestKey || r.Header.Get("Authorization") != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","detail":"Invalid or expired API key"}`))
			return
		}
		switch r.URL.Path {
		case "/v1/ingest":
			singles.Add(1)
			events.Add(1)
			w.WriteHeader(http.StatusAccepted)
			_, _ = w.Write([]byte(`{"success":true,"data":{"status":"ACCEPTED"}}`))
		case "/v1/ingest/batch":
			var body struct {
				Events []json.RawMessage `json:"events"`
			}
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)
			batches.Add(1)
			events.Add(int64(len(body.Events)))
			w.WriteHeader(http.StatusAccepted)
			_, _ = fmt.Fprintf(w, `{"success":true,"data":{"accepted":%d,"duplicates":0,"failed":0,"errors":[]}}`, len(body.Events))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func withIngestKey(m *seed.Manifest, key string) *seed.Manifest {
	for i := range m.Tenants {
		m.Tenants[i].IngestKey = &seed.ManifestIngestKey{KeyID: "ik-1", RawKey: key}
	}
	return m
}

func runAgainst(t *testing.T, srv *httptest.Server, scn *scenario.Scenario, m *seed.Manifest, workers int) *RunResult {
	t.Helper()
	u, _ := url.Parse(srv.URL)
	target := aforo.Target{Name: "fake", URLs: map[aforo.Service]string{aforo.ServiceUsageIngestor: u.Scheme + "://" + u.Host}}
	r, err := New(Config{Scenario: scn, Manifest: m, Target: target, OutputDir: t.TempDir(), Workers: workers, BufferSize: 1024})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func benchScenario(paths scenario.IngestionPaths, batch scenario.RESTBatch) *scenario.Scenario {
	return &scenario.Scenario{
		SchemaVersion:    2,
		Name:             "ingest-auth-smoke",
		TargetTPS:        200,
		Duration:         scenario.Duration(2 * time.Second),
		Seed:             1,
		Tenants:          scenario.Tenants{Count: 1, Distribution: scenario.DistUniform},
		ProductMix:       scenario.ProductMix{API: 1.0},
		IngestionPaths:   paths,
		RESTBatch:        batch,
		PayloadVariation: scenario.PayloadVariation{SmallPct: 1.0},
	}
}

// TestRunUsesIngestKeyEndToEnd: with the tenant ingest key on the manifest,
// rest_direct traffic is accepted by a key-checking ingestor.
func TestRunUsesIngestKeyEndToEnd(t *testing.T) {
	var singles, batches, events atomic.Int64
	srv := fakeIngestorWithKeyAuth(t, "sk_test_ingest", &singles, &batches, &events)
	defer srv.Close()
	res := runAgainst(t, srv, benchScenario(scenario.IngestionPaths{RestDirect: 1.0}, scenario.RESTBatch{}), withIngestKey(smokeManifest(), "sk_test_ingest"), 8)
	if res.EventsSucceeded < 100 || res.ClientErrors != 0 {
		t.Fatalf("want >=100 accepted and no 4xx, got succeeded=%d 4xx=%d", res.EventsSucceeded, res.ClientErrors)
	}
}

// TestRunWithoutIngestKeyIsRefused documents the failure mode the ingest key
// closes: the per-subscription key alone is answered 401.
func TestRunWithoutIngestKeyIsRefused(t *testing.T) {
	var singles, batches, events atomic.Int64
	srv := fakeIngestorWithKeyAuth(t, "sk_test_ingest", &singles, &batches, &events)
	defer srv.Close()
	res := runAgainst(t, srv, benchScenario(scenario.IngestionPaths{RestDirect: 1.0}, scenario.RESTBatch{}), smokeManifest(), 8)
	if res.EventsSucceeded != 0 || res.ClientErrors == 0 {
		t.Fatalf("want every event refused, got succeeded=%d 4xx=%d", res.EventsSucceeded, res.ClientErrors)
	}
}

// TestRunRestBatchEndToEnd: rest_batch packs events into /v1/ingest/batch
// requests and the runner counts every event in them.
func TestRunRestBatchEndToEnd(t *testing.T) {
	var singles, batches, events atomic.Int64
	srv := fakeIngestorWithKeyAuth(t, "sk_test_ingest", &singles, &batches, &events)
	defer srv.Close()
	res := runAgainst(t, srv,
		benchScenario(scenario.IngestionPaths{RestBatch: 1.0}, scenario.RESTBatch{Size: 20, MaxWait: scenario.Duration(50 * time.Millisecond)}),
		withIngestKey(smokeManifest(), "sk_test_ingest"), 64)
	if res.EventsSucceeded < 100 || res.ClientErrors != 0 {
		t.Fatalf("want >=100 accepted and no 4xx, got succeeded=%d 4xx=%d", res.EventsSucceeded, res.ClientErrors)
	}
	if batches.Load() == 0 || singles.Load() != 0 {
		t.Fatalf("want batch requests only, got batches=%d singles=%d", batches.Load(), singles.Load())
	}
	if events.Load() < res.EventsSucceeded {
		t.Fatalf("server saw %d events in batches, runner counted %d accepted", events.Load(), res.EventsSucceeded)
	}
	if events.Load()/batches.Load() < 2 {
		t.Fatalf("batches carried %.1f events on average; expected real batching", float64(events.Load())/float64(batches.Load()))
	}
}
