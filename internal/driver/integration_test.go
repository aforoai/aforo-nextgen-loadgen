//go:build integration

// Integration tests for the rest_direct driver against a live local
// usage-ingestor. Tag-gated — these only run with `go test -tags
// integration ./internal/driver/...`. The default `go test` invocation
// skips them.
//
// Required env:
//
//	AFORO_LOADGEN_INTEGRATION=1     enables the suite
//	AFORO_INGEST_URL                full URL of the ingest endpoint (defaults to local)
//	AFORO_TENANT_ID                 a real tenant from the seed manifest
//	AFORO_VALID_KEY                 a real BEARER_TOKEN secret for the tenant
//	AFORO_REVOKED_KEY               a revoked secret from a CANCELLED/EXPIRED sub
//
// Per the Session 4 spec, the test verifies four classification paths:
//
//	Active key + valid event       → 2xx
//	Active key + future timestamp  → 4xx (rejected by validator)
//	Stale key  + valid event       → 401/403
//	Fabricated key + valid event   → 401
package driver

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

func requireIntegration(t *testing.T) {
	if os.Getenv("AFORO_LOADGEN_INTEGRATION") != "1" {
		t.Skip("AFORO_LOADGEN_INTEGRATION=1 not set; skipping integration test")
	}
}

func newIntegrationDriver(t *testing.T) *RESTDirect {
	t.Helper()
	urlStr := os.Getenv("AFORO_INGEST_URL")
	if urlStr == "" {
		urlStr = "http://localhost:8084/v1/ingest"
	}
	u, err := url.Parse(urlStr)
	if err != nil {
		t.Fatalf("parse AFORO_INGEST_URL %q: %v", urlStr, err)
	}
	target := aforo.Target{
		Name: "integration",
		URLs: map[aforo.Service]string{
			aforo.ServiceUsageIngestor: u.Scheme + "://" + u.Host,
		},
	}
	d, err := NewRESTDirect(RESTDirectConfig{Target: target, RequestTimeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("driver: %v", err)
	}
	return d
}

func baseEvent() *generator.Event {
	return &generator.Event{
		Envelope: generator.Envelope{
			EventID:        randHex(16),
			EventTimestamp: time.Now().UTC(),
			TenantID:       os.Getenv("AFORO_TENANT_ID"),
			CustomerID:     "loadgen-int-cust",
			SubscriptionID: "loadgen-int-sub",
			ProductType:    "API",
			Body: map[string]any{
				"endpoint":       "/api/v1/health",
				"method":         "GET",
				"status_code":    200,
				"latency_ms":     42,
				"request_bytes":  0,
				"response_bytes": 200,
			},
		},
		IngestionPath: "rest_direct",
	}
}

func TestIntegrationActiveKeyValidEvent(t *testing.T) {
	requireIntegration(t)
	if os.Getenv("AFORO_VALID_KEY") == "" {
		t.Fatal("AFORO_VALID_KEY required")
	}
	d := newIntegrationDriver(t)
	defer d.Close()

	e := baseEvent()
	e.Auth.Token = os.Getenv("AFORO_VALID_KEY")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := d.Submit(ctx, e)
	if !res.IsSuccess() {
		t.Fatalf("active key + valid event: status=%d body=transport=%v", res.Status, res.TransportErr)
	}
}

func TestIntegrationActiveKeyFutureTimestamp(t *testing.T) {
	requireIntegration(t)
	if os.Getenv("AFORO_VALID_KEY") == "" {
		t.Fatal("AFORO_VALID_KEY required")
	}
	d := newIntegrationDriver(t)
	defer d.Close()

	e := baseEvent()
	e.Auth.Token = os.Getenv("AFORO_VALID_KEY")
	e.Envelope.EventTimestamp = time.Now().Add(10 * time.Minute) // >5min future

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := d.Submit(ctx, e)
	if !res.IsClientError() {
		t.Errorf("active key + future event: want 4xx, got status=%d transport=%v", res.Status, res.TransportErr)
	}
}

func TestIntegrationStaleKeyValidEvent(t *testing.T) {
	requireIntegration(t)
	if os.Getenv("AFORO_REVOKED_KEY") == "" {
		t.Fatal("AFORO_REVOKED_KEY required")
	}
	d := newIntegrationDriver(t)
	defer d.Close()

	e := baseEvent()
	e.Auth.Token = os.Getenv("AFORO_REVOKED_KEY")
	e.NegativePath = generator.NPStaleKey

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := d.Submit(ctx, e)
	if res.Status != 401 && res.Status != 403 {
		t.Errorf("stale key: want 401/403, got status=%d transport=%v", res.Status, res.TransportErr)
	}
}

func TestIntegrationFabricatedKey(t *testing.T) {
	requireIntegration(t)
	d := newIntegrationDriver(t)
	defer d.Close()

	e := baseEvent()
	e.Auth.Token = "sk_live_fab_" + randHex(16)
	e.Auth.IsFabricated = true
	e.NegativePath = generator.NPWrongAuth

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res := d.Submit(ctx, e)
	if res.Status != 401 && res.Status != 403 {
		t.Errorf("fabricated key: want 401/403, got status=%d transport=%v", res.Status, res.TransportErr)
	}
}

// TestIntegrationBatchClassifications — sends 100 events spanning the
// four classifications and verifies every outcome matches the spec.
func TestIntegrationBatchClassifications(t *testing.T) {
	requireIntegration(t)
	for _, k := range []string{"AFORO_VALID_KEY", "AFORO_REVOKED_KEY"} {
		if os.Getenv(k) == "" {
			t.Skipf("%s not set", k)
		}
	}
	d := newIntegrationDriver(t)
	defer d.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	type bucket struct {
		name      string
		makeEvent func() *generator.Event
		predicate func(Result) bool
	}
	buckets := []bucket{
		{"active+valid", func() *generator.Event {
			e := baseEvent()
			e.Auth.Token = os.Getenv("AFORO_VALID_KEY")
			return e
		}, func(r Result) bool { return r.IsSuccess() }},
		{"active+future", func() *generator.Event {
			e := baseEvent()
			e.Auth.Token = os.Getenv("AFORO_VALID_KEY")
			e.Envelope.EventTimestamp = time.Now().Add(10 * time.Minute)
			return e
		}, func(r Result) bool { return r.IsClientError() }},
		{"stale", func() *generator.Event {
			e := baseEvent()
			e.Auth.Token = os.Getenv("AFORO_REVOKED_KEY")
			return e
		}, func(r Result) bool { return r.Status == 401 || r.Status == 403 }},
		{"fabricated", func() *generator.Event {
			e := baseEvent()
			e.Auth.Token = "sk_live_fab_" + randHex(16)
			return e
		}, func(r Result) bool { return r.Status == 401 || r.Status == 403 }},
	}

	const perBucket = 25 // 4*25 = 100 events
	for _, b := range buckets {
		fails := 0
		for i := 0; i < perBucket; i++ {
			res := d.Submit(ctx, b.makeEvent())
			if !b.predicate(res) {
				fails++
				if fails <= 3 {
					t.Logf("%s mismatch: status=%d transport=%v", b.name, res.Status, res.TransportErr)
				}
			}
		}
		// Allow up to 5% flakes per bucket for transient platform hiccups.
		if fails > perBucket/20 {
			t.Errorf("%s: %d/%d events did not match expected predicate", b.name, fails, perBucket)
		}
	}
}

func randHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return strings.Repeat("0", n*2)
	}
	return hex.EncodeToString(buf)
}
