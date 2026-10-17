package driver

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

func batchEvent(tenant, key string, i int) *generator.Event {
	return &generator.Event{
		Envelope: generator.Envelope{
			CustomerID:     fmt.Sprintf("cust-%d", i%3),
			MetricName:     "API Calls",
			Quantity:       1,
			OccurredAt:     time.Now().UTC(),
			IdempotencyKey: fmt.Sprintf("%s-evt-%d", tenant, i),
		},
		TenantID: tenant,
		EventID:  fmt.Sprintf("e-%d", i),
		Auth:     generator.EventAuth{Token: "sk_live_pricing_key", IngestKey: key},
	}
}

// submitAll fires n Submits concurrently and returns the results in order.
func submitAll(t *testing.T, d *RESTBatch, events []*generator.Event) []Result {
	t.Helper()
	out := make([]Result, len(events))
	var wg sync.WaitGroup
	for i, e := range events {
		wg.Add(1)
		go func(i int, e *generator.Event) {
			defer wg.Done()
			out[i] = d.Submit(context.Background(), e)
		}(i, e)
	}
	wg.Wait()
	return out
}

// TestRESTBatchSendsOneRequestPerFullBatch: Size events for one tenant go
// out as ONE POST /v1/ingest/batch with the envelope list, the tenant ingest
// key as X-API-Key, and no per-subscription Bearer key or X-Customer-Id.
func TestRESTBatchSendsOneRequestPerFullBatch(t *testing.T) {
	var calls atomic.Int32
	var gotPath, gotKey, gotAuth, gotCustomer, gotTenant string
	var gotCount int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		gotPath = r.URL.Path
		gotKey = r.Header.Get("X-API-Key")
		gotAuth = r.Header.Get("Authorization")
		gotCustomer = r.Header.Get("X-Customer-Id")
		gotTenant = r.Header.Get("X-Tenant-Id")
		var body struct {
			Events []map[string]any `json:"events"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		gotCount = len(body.Events)
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"success":true,"data":{"accepted":%d,"duplicates":0,"failed":0,"errors":[]}}`, gotCount)
	}))
	defer ts.Close()

	d, err := NewRESTBatch(RESTBatchConfig{HTTPBaseConfig: HTTPBaseConfig{Target: newTestTarget(ts.URL)}, Size: 5, MaxWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()

	events := make([]*generator.Event, 5)
	for i := range events {
		events[i] = batchEvent("tenant-A", "sk_test_ingest", i)
	}
	results := submitAll(t, d, events)

	if calls.Load() != 1 {
		t.Fatalf("want 1 request, got %d", calls.Load())
	}
	if gotPath != PathUsageIngestBatch || gotCount != 5 {
		t.Fatalf("want 5 events to %s, got %d to %s", PathUsageIngestBatch, gotCount, gotPath)
	}
	if gotKey != "sk_test_ingest" || gotAuth != "" {
		t.Fatalf("want X-API-Key ingest key and no Authorization, got key=%q auth=%q", gotKey, gotAuth)
	}
	if gotCustomer != "" || gotTenant != "tenant-A" {
		t.Fatalf("want X-Tenant-Id only, got customer=%q tenant=%q", gotCustomer, gotTenant)
	}
	for i, r := range results {
		if r.Status != http.StatusAccepted || r.Event != events[i] {
			t.Fatalf("event %d: want 202 for its own event, got %+v", i, r)
		}
	}
}

// TestRESTBatchMapsPerIndexErrors: an index the server lists in errors is
// reported as 422 with the message; the rest succeed.
func TestRESTBatchMapsPerIndexErrors(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []map[string]any `json:"events"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		// Fail whichever position carries idempotency key "tenant-A-evt-1".
		bad := -1
		for i, ev := range body.Events {
			if ev["idempotencyKey"] == "tenant-A-evt-1" {
				bad = i
			}
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprintf(w, `{"accepted":2,"duplicates":0,"failed":1,"errors":[{"index":%d,"message":"Unknown metric"}]}`, bad)
	}))
	defer ts.Close()

	d, err := NewRESTBatch(RESTBatchConfig{HTTPBaseConfig: HTTPBaseConfig{Target: newTestTarget(ts.URL)}, Size: 3, MaxWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	events := []*generator.Event{batchEvent("tenant-A", "k", 0), batchEvent("tenant-A", "k", 1), batchEvent("tenant-A", "k", 2)}
	results := submitAll(t, d, events)
	for i, r := range results {
		if i == 1 {
			if r.Status != http.StatusUnprocessableEntity || r.BodyExcerpt != "batch error: Unknown metric" {
				t.Fatalf("event 1: want 422 + message, got %+v", r)
			}
			continue
		}
		if r.Status != http.StatusAccepted {
			t.Fatalf("event %d: want 202, got %+v", i, r)
		}
	}
}

// TestRESTBatchWholeRequestFailure: a non-2xx answer applies to every event.
func TestRESTBatchWholeRequestFailure(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":"RATE_LIMITED"}]}`))
	}))
	defer ts.Close()
	d, err := NewRESTBatch(RESTBatchConfig{HTTPBaseConfig: HTTPBaseConfig{Target: newTestTarget(ts.URL)}, Size: 2, MaxWait: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	results := submitAll(t, d, []*generator.Event{batchEvent("t", "k", 0), batchEvent("t", "k", 1)})
	for i, r := range results {
		if r.Status != http.StatusTooManyRequests || r.BodyExcerpt == "" {
			t.Fatalf("event %d: want 429 with body, got %+v", i, r)
		}
	}
}

// TestRESTBatchPartialBatchAfterMaxWait: a batch that never fills is sent
// when MaxWait expires; tenants are never mixed in one request.
func TestRESTBatchPartialBatchAfterMaxWait(t *testing.T) {
	var mu sync.Mutex
	perTenant := map[string]int{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Events []map[string]any `json:"events"`
		}
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		mu.Lock()
		perTenant[r.Header.Get("X-Tenant-Id")] += len(body.Events)
		mu.Unlock()
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"data":{"accepted":1,"errors":[]}}`))
	}))
	defer ts.Close()
	d, err := NewRESTBatch(RESTBatchConfig{HTTPBaseConfig: HTTPBaseConfig{Target: newTestTarget(ts.URL)}, Size: 100, MaxWait: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	results := submitAll(t, d, []*generator.Event{batchEvent("A", "ka", 0), batchEvent("B", "kb", 1), batchEvent("A", "ka", 2)})
	for i, r := range results {
		if !r.IsSuccess() {
			t.Fatalf("event %d: want success, got %+v", i, r)
		}
	}
	if perTenant["A"] != 2 || perTenant["B"] != 1 {
		t.Fatalf("want A=2 B=1 events, got %v", perTenant)
	}
}

// TestRESTBatchNegativePathGoesAlone: a negative-path event is sent through
// /v1/ingest by itself so its bad credential cannot fail a batch.
func TestRESTBatchNegativePathGoesAlone(t *testing.T) {
	var paths []string
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer ts.Close()
	d, err := NewRESTBatch(RESTBatchConfig{HTTPBaseConfig: HTTPBaseConfig{Target: newTestTarget(ts.URL)}, Size: 10, MaxWait: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	e := batchEvent("A", "", 0)
	e.NegativePath = generator.NPWrongAuth
	r := d.Submit(context.Background(), e)
	if r.Status != http.StatusUnauthorized || len(paths) != 1 || paths[0] != "/v1/ingest" {
		t.Fatalf("want one 401 from /v1/ingest, got %+v paths=%v", r, paths)
	}
}

func TestRESTBatchRejectsOversizeConfig(t *testing.T) {
	_, err := NewRESTBatch(RESTBatchConfig{HTTPBaseConfig: HTTPBaseConfig{Target: newTestTarget("http://127.0.0.1:1")}, Size: 1001})
	if err == nil {
		t.Fatal("size above the server's 1000-event cap must be rejected")
	}
}

// TestIngestKeyHeader: rest_direct and the shared HTTP helper send the
// tenant ingest key as X-API-Key instead of the per-subscription key.
func TestIngestKeyHeader(t *testing.T) {
	var gotKey, gotAuth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey, gotAuth = r.Header.Get("X-API-Key"), r.Header.Get("Authorization")
		w.WriteHeader(http.StatusAccepted)
	}))
	defer ts.Close()
	d, err := NewRESTDirect(RESTDirectConfig{Target: newTestTarget(ts.URL)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	_ = d.Submit(context.Background(), batchEvent("A", "sk_test_ingest", 0))
	if gotKey != "sk_test_ingest" || gotAuth != "" {
		t.Fatalf("rest_direct: want X-API-Key only, got key=%q auth=%q", gotKey, gotAuth)
	}
	_ = d.Submit(context.Background(), batchEvent("A", "", 1))
	if gotKey != "" || gotAuth != "Bearer sk_live_pricing_key" {
		t.Fatalf("rest_direct without ingest key: want Bearer fallback, got key=%q auth=%q", gotKey, gotAuth)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/ingest", nil)
	applyAuthHeaders(req, batchEvent("A", "sk_test_ingest", 0), "admin")
	if req.Header.Get("X-API-Key") != "sk_test_ingest" || req.Header.Get("Authorization") != "" {
		t.Fatalf("applyAuthHeaders: want X-API-Key only, got %v", req.Header)
	}
}
