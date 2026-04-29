package seed

import (
	"context"
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
