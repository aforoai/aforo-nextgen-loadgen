package driver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// newTestEvent returns a minimal Event suitable for driver round-trips.
// All driver tests use this so payload-shape changes only need updating
// in one place.
func newTestEvent() *generator.Event {
	return &generator.Event{
		Envelope: generator.Envelope{
			OccurredAt:     time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
			CustomerID:     "cust-001",
			ProductType:    "API",
			MetricName:     "metric-api-calls",
			Quantity:       1.0,
			IdempotencyKey: "evt_loadgen_test_0001",
			Metadata:       map[string]any{"endpoint": "/v1/whoami", "method": "GET", "status_code": 200},
		},
		TenantID:      "tenant-alpha",
		EventID:       "evt_loadgen_test_0001",
		IngestionPath: "rest_direct",
		Archetype:     "test-archetype",
		Auth: generator.EventAuth{
			Token:    "tk_test_secret",
			ClientID: "cid_test_001",
		},
		GeneratedAt: time.Date(2026, 4, 30, 12, 0, 0, 0, time.UTC),
	}
}

// targetForServer returns an aforo.Target whose usage-ingestor URL points
// at the given httptest server. Other services point at an unused stub —
// the driver tests only hit usage-ingestor.
func targetForServer(srv *httptest.Server) aforo.Target {
	u, _ := url.Parse(srv.URL)
	return aforo.Target{
		Name: "test",
		URLs: map[aforo.Service]string{
			aforo.ServiceUsageIngestor: u.Scheme + "://" + u.Host,
		},
	}
}

// TestSDKDrivers_AllSendCanonicalEnvelope verifies each SDK driver POSTs
// the same JSON envelope, attaches its identification headers, and lands
// on /v1/ingest. This is the main contract test for the SDK family.
func TestSDKDrivers_AllSendCanonicalEnvelope(t *testing.T) {
	cases := []struct {
		name       string
		newDriver  func(HTTPBaseConfig) (Driver, error)
		expectUA   string
		expectLang string
		expectVer  string
	}{
		{
			name: "sdk_node",
			newDriver: func(c HTTPBaseConfig) (Driver, error) {
				d, err := NewSDKNode(c)
				return d, err
			},
			expectUA:   "aforo-metering-node/",
			expectLang: "node",
			expectVer:  SDKNodeVersion,
		},
		{
			name: "sdk_python",
			newDriver: func(c HTTPBaseConfig) (Driver, error) {
				d, err := NewSDKPython(c)
				return d, err
			},
			expectUA:   "aforo-metering-python/",
			expectLang: "python",
			expectVer:  SDKPythonVersion,
		},
		{
			name: "sdk_java",
			newDriver: func(c HTTPBaseConfig) (Driver, error) {
				d, err := NewSDKJava(c)
				return d, err
			},
			expectUA:   "com.aforo.metering/",
			expectLang: "java",
			expectVer:  SDKJavaVersion,
		},
		{
			name: "sdk_go",
			newDriver: func(c HTTPBaseConfig) (Driver, error) {
				d, err := NewSDKGo(c)
				return d, err
			},
			expectUA:   "metering-go/",
			expectLang: "go",
			expectVer:  SDKGoVersion,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured *http.Request
			var capturedBody []byte
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = r.Clone(context.Background())
				body, _ := io.ReadAll(r.Body)
				capturedBody = body
				w.WriteHeader(http.StatusAccepted)
			}))
			defer srv.Close()

			d, err := tc.newDriver(HTTPBaseConfig{Target: targetForServer(srv)})
			if err != nil {
				t.Fatalf("construct: %v", err)
			}
			defer func() { _ = d.Close() }()

			res := d.Submit(context.Background(), newTestEvent())
			if !res.IsSuccess() {
				t.Fatalf("expected 2xx, got status=%d err=%v", res.Status, res.TransportErr)
			}
			if d.Name() != tc.name {
				t.Errorf("driver name: got %q want %q", d.Name(), tc.name)
			}
			if captured == nil {
				t.Fatal("server captured no request")
			}
			if got := captured.URL.Path; got != "/v1/ingest" {
				t.Errorf("URL path: got %q want /v1/ingest", got)
			}
			if ua := captured.Header.Get("User-Agent"); !strings.HasPrefix(ua, tc.expectUA) {
				t.Errorf("User-Agent: got %q want prefix %q", ua, tc.expectUA)
			}
			if got := captured.Header.Get("X-SDK-Lang"); got != tc.expectLang {
				t.Errorf("X-SDK-Lang: got %q want %q", got, tc.expectLang)
			}
			if got := captured.Header.Get("X-SDK-Version"); got != tc.expectVer {
				t.Errorf("X-SDK-Version: got %q want %q", got, tc.expectVer)
			}
			// Body must be the envelope JSON. Field-name contract verified
			// against backend's IngestUsageEventRequest (camelCase).
			var env generator.Envelope
			if err := json.Unmarshal(capturedBody, &env); err != nil {
				t.Fatalf("unmarshal envelope: %v", err)
			}
			if env.IdempotencyKey != "evt_loadgen_test_0001" {
				t.Errorf("envelope.idempotencyKey: got %q", env.IdempotencyKey)
			}
			if env.CustomerID == "" {
				t.Errorf("envelope.customerId empty — driver should propagate from Event")
			}
			// Tenant + auth headers must be set.
			if got := captured.Header.Get("Authorization"); got != "Bearer tk_test_secret" {
				t.Errorf("Authorization: got %q", got)
			}
			if got := captured.Header.Get("X-Tenant-Id"); got != "tenant-alpha" {
				t.Errorf("X-Tenant-Id: got %q", got)
			}
		})
	}
}

// TestSDKDrivers_UnknownTargetService surfaces a clear error message —
// the driver must reject construction when the target lacks a usage-
// ingestor URL rather than panicking on first Submit.
func TestSDKDrivers_UnknownTargetService(t *testing.T) {
	bad := aforo.Target{Name: "broken", URLs: map[aforo.Service]string{}}
	for _, ctor := range []func(HTTPBaseConfig) error{
		func(c HTTPBaseConfig) error { _, err := NewSDKNode(c); return err },
		func(c HTTPBaseConfig) error { _, err := NewSDKPython(c); return err },
		func(c HTTPBaseConfig) error { _, err := NewSDKJava(c); return err },
		func(c HTTPBaseConfig) error { _, err := NewSDKGo(c); return err },
	} {
		if err := ctor(HTTPBaseConfig{Target: bad}); err == nil {
			t.Fatal("expected construction error for target with no usage-ingestor URL")
		}
	}
}
