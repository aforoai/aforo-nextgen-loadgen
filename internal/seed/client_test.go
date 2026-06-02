package seed

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

func TestNewClient_RequiresTokenWhenLive(t *testing.T) {
	_, err := NewClient(ClientConfig{
		Target:      aforo.LocalTarget,
		BearerToken: "",
		DryRun:      false,
	})
	if !errors.Is(err, aforo.ErrAuthMissing) {
		t.Errorf("expected ErrAuthMissing, got %v", err)
	}
}

func TestNewClient_DryRunSkipsTokenCheck(t *testing.T) {
	c, err := NewClient(ClientConfig{
		Target: aforo.LocalTarget,
		DryRun: true,
	})
	if err != nil {
		t.Fatalf("dry-run NewClient: %v", err)
	}
	defer c.Close()
}

func TestClient_RetriesOn5xx(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&attempts, 1)
		if n < 3 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"transient"}`)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"x"}`)
	}))
	defer server.Close()

	c, err := NewClient(ClientConfig{
		Target:      singleHostTarget(server.URL),
		BearerToken: "test",
		MaxRetries:  4,
		BaseBackoff: 1 * time.Millisecond,
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var resp map[string]any
	err = c.Do(context.Background(), http.MethodGet, server.URL+"/test", nil, &resp, RequestOptions{})
	if err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp["id"] != "x" {
		t.Errorf("got %v, want x", resp["id"])
	}
	if got := atomic.LoadInt32(&attempts); got != 3 {
		t.Errorf("attempts = %d, want 3", got)
	}
}

func TestClient_DoesNotRetryOn4xx(t *testing.T) {
	var attempts int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&attempts, 1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"bad request"}`)
	}))
	defer server.Close()

	c, err := NewClient(ClientConfig{
		Target:      singleHostTarget(server.URL),
		BearerToken: "test",
		MaxRetries:  4,
		BaseBackoff: 1 * time.Millisecond,
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	err = c.Do(context.Background(), http.MethodGet, server.URL+"/test", nil, nil, RequestOptions{})
	if err == nil {
		t.Fatalf("expected error")
	}
	if got := atomic.LoadInt32(&attempts); got != 1 {
		t.Errorf("attempts = %d, want 1 (no retry on 4xx)", got)
	}
}

func TestClient_PropagatesHeaders(t *testing.T) {
	var seenAuth, seenTenant, seenIdem string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenTenant = r.Header.Get("X-Tenant-Id")
		seenIdem = r.Header.Get("Idempotency-Key")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	c, err := NewClient(ClientConfig{
		Target:      singleHostTarget(server.URL),
		BearerToken: "tok-123",
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Do(context.Background(), http.MethodGet, server.URL+"/x", nil, nil, RequestOptions{
		TenantID:    "tn-1",
		Idempotency: "idem-1",
	}); err != nil {
		t.Fatal(err)
	}
	if seenAuth != "Bearer tok-123" {
		t.Errorf("Authorization = %q", seenAuth)
	}
	if seenTenant != "tn-1" {
		t.Errorf("X-Tenant-Id = %q", seenTenant)
	}
	if seenIdem != "idem-1" {
		t.Errorf("Idempotency-Key = %q", seenIdem)
	}
}

func TestClient_RateLimit_RespectsMinInterval(t *testing.T) {
	var hitTimes []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hitTimes = append(hitTimes, time.Now())
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{}`)
	}))
	defer server.Close()

	const interval = 50 * time.Millisecond
	c, err := NewClient(ClientConfig{
		Target:      singleHostTarget(server.URL),
		BearerToken: "test",
		MinInterval: interval,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	// Fire 4 requests sequentially. Total wall time must include ≥3 * interval
	// of waits (the first request consumes the initial token, the next 3 wait).
	start := time.Now()
	for i := 0; i < 4; i++ {
		if err := c.Do(context.Background(), http.MethodGet, server.URL+"/y", nil, nil, RequestOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 3*interval-10*time.Millisecond {
		t.Errorf("rate limiter did not space requests: elapsed=%v expected≥%v",
			elapsed, 3*interval)
	}
}

// TestUnmarshalAforoResponse_EnvelopeShape locks in the contract that the
// standard Aforo {success, data, meta} envelope unwraps into the target
// struct's fields. Regression guard for the 2026-06-01 bug where a prior
// version's "try direct unmarshal first" branch silently zero-valued the
// target when the body was an envelope. See unmarshalAforoResponse history.
func TestUnmarshalAforoResponse_EnvelopeShape(t *testing.T) {
	type entity struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	body := []byte(`{"success":true,"data":{"id":"t-1","name":"Acme"},"meta":{}}`)
	var got entity
	if err := unmarshalAforoResponse(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "t-1" || got.Name != "Acme" {
		t.Errorf("envelope not unwrapped: got %+v", got)
	}
}

// TestUnmarshalAforoResponse_PlainEntity covers internal admin endpoints
// (e.g. LoadgenInternalTenantController) that writeJson the entity directly,
// without the envelope.
func TestUnmarshalAforoResponse_PlainEntity(t *testing.T) {
	type entity struct {
		ID         string `json:"id"`
		ExternalID string `json:"externalId"`
	}
	body := []byte(`{"id":"t-2","externalId":"loadgen-x"}`)
	var got entity
	if err := unmarshalAforoResponse(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "t-2" || got.ExternalID != "loadgen-x" {
		t.Errorf("plain entity decode wrong: got %+v", got)
	}
}

// TestUnmarshalAforoResponse_PlainArray covers list endpoints that return
// bare arrays (some legacy internal endpoints).
func TestUnmarshalAforoResponse_PlainArray(t *testing.T) {
	body := []byte(`[{"id":"a"},{"id":"b"}]`)
	var got []map[string]string
	if err := unmarshalAforoResponse(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got) != 2 || got[0]["id"] != "a" || got[1]["id"] != "b" {
		t.Errorf("plain array decode wrong: got %+v", got)
	}
}

// TestUnmarshalAforoResponse_DataOnlyEnvelope covers list responses like the
// loadgen tenant lookup (LoadgenTenantListResponse: {"data":[...]} with no
// success/meta keys). isEnvelopeResponse requires BOTH success AND data, so
// these MUST fall through to the direct-unmarshal path. If this regresses,
// lookupTenantByExternalID will silently return zero hits.
func TestUnmarshalAforoResponse_DataOnlyEnvelope(t *testing.T) {
	type item struct {
		ID string `json:"id"`
	}
	var got struct {
		Data []item `json:"data"`
	}
	body := []byte(`{"data":[{"id":"t-3"}]}`)
	if err := unmarshalAforoResponse(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Data) != 1 || got.Data[0].ID != "t-3" {
		t.Errorf("data-only envelope decode wrong: got %+v", got)
	}
}

// TestUnmarshalAforoResponse_EnvelopeNullData covers a 2xx with success:true
// but data:null (e.g. successful no-op endpoints). Must leave out untouched
// and return no error.
func TestUnmarshalAforoResponse_EnvelopeNullData(t *testing.T) {
	type entity struct {
		ID string `json:"id"`
	}
	got := entity{ID: "pre"}
	body := []byte(`{"success":true,"data":null,"meta":{}}`)
	if err := unmarshalAforoResponse(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.ID != "pre" {
		t.Errorf("expected out untouched, got %+v", got)
	}
}

// TestClient_UnwrapsEnvelopeEndToEnd is the canonical regression test for the
// developer-reported bug: a backend that returns the {success,data,meta}
// envelope must produce a populated entity at the Do() call site. Prior to
// the fix this returned (err=nil, ID=""), which let the seeder log success
// while the manifest recorded empty IDs and downstream provisioners ran
// against an empty tenant context.
func TestClient_UnwrapsEnvelopeEndToEnd(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"success":true,"data":{"id":"tnt-42","externalId":"loadgen-foo","name":"Foo"},"meta":{"requestId":"r-1"}}`)
	}))
	defer server.Close()

	c, err := NewClient(ClientConfig{
		Target:      singleHostTarget(server.URL),
		BearerToken: "test",
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var resp struct {
		ID         string `json:"id"`
		ExternalID string `json:"externalId"`
		Name       string `json:"name"`
	}
	if err := c.Do(context.Background(), http.MethodPost, server.URL+"/create", map[string]string{"name": "Foo"}, &resp, RequestOptions{}); err != nil {
		t.Fatalf("Do: %v", err)
	}
	if resp.ID == "" {
		t.Fatalf("envelope was not unwrapped: resp=%+v (this is the developer-reported bug)", resp)
	}
	if resp.ID != "tnt-42" || resp.ExternalID != "loadgen-foo" || resp.Name != "Foo" {
		t.Errorf("decoded wrong fields: %+v", resp)
	}
}

// TestClient_DecodesBodyLargerThanTruncateCap is a regression test for the
// developer-reported "unmarshal: unexpected end of JSON input" on GET
// /api/v1/customers (2026-06-02). doOnce previously read the response through
// io.LimitReader(resp.Body, defaultBodyTruncate*4), capping the bytes it
// decoded at 16 KiB. The customer-service list endpoint has no server-side
// filter and returns every customer, so once a tenant had more than a handful
// of rows the JSON exceeded 16 KiB and got cut mid-stream — while a direct
// curl (no cap) worked fine. The full body must be read for decoding;
// defaultBodyTruncate is for capping error bodies shown to users only.
func TestClient_DecodesBodyLargerThanTruncateCap(t *testing.T) {
	// Build a list response well past the old 16 KiB read cap.
	const n = 500
	items := make([]map[string]string, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, map[string]string{
			"id":    "cus-" + string(rune('a'+i%26)) + string(rune('0'+i%10)),
			"email": "lg-padding-padding-padding-padding@loadgen.aforo.test",
		})
	}
	payload := struct {
		Success bool                `json:"success"`
		Data    []map[string]string `json:"data"`
	}{Success: true, Data: items}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) <= defaultBodyTruncate*4 {
		t.Fatalf("test payload (%d bytes) must exceed the old 16 KiB read cap to be a valid regression", len(body))
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	}))
	defer server.Close()

	c, err := NewClient(ClientConfig{
		Target:      singleHostTarget(server.URL),
		BearerToken: "test",
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	var got []map[string]string
	if err := c.Do(context.Background(), http.MethodGet, server.URL+"/api/v1/customers", nil, &got, RequestOptions{}); err != nil {
		t.Fatalf("Do: %v (this is the truncated-body bug regressing)", err)
	}
	if len(got) != n {
		t.Fatalf("decoded %d items, want %d — body was truncated", len(got), n)
	}
}

func TestClient_DryRunRecordsRequests(t *testing.T) {
	c, err := NewClient(ClientConfig{
		Target:      aforo.LocalTarget,
		DryRun:      true,
		MinInterval: 1 * time.Microsecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	if err := c.Do(context.Background(), http.MethodPost, "http://example/api", map[string]string{"foo": "bar"}, nil, RequestOptions{
		TenantID: "tn-1",
	}); err != nil {
		t.Fatalf("Do dry-run: %v", err)
	}
	recs := c.DryRunRecords()
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	if recs[0].Method != http.MethodPost {
		t.Errorf("method = %s", recs[0].Method)
	}
	if recs[0].URL != "http://example/api" {
		t.Errorf("url = %s", recs[0].URL)
	}
	if recs[0].Headers.Get("X-Tenant-Id") != "tn-1" {
		t.Errorf("tenant header = %s", recs[0].Headers.Get("X-Tenant-Id"))
	}
	// Body shape preserved.
	if string(recs[0].Body) != `{"foo":"bar"}` {
		t.Errorf("body = %s", string(recs[0].Body))
	}
}

func TestRateLimiter_GoroutineExitsAfterClose(t *testing.T) {
	rl := newRateLimiter(10 * time.Millisecond)
	rl.Close()
	// Calling Close twice must not panic.
	rl.Close()
}
