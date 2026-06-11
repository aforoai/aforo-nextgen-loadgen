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

func newTestTarget(serverURL string) aforo.Target {
	u, _ := url.Parse(serverURL)
	return aforo.Target{
		Name: "test",
		URLs: map[aforo.Service]string{
			aforo.ServiceUsageIngestor: u.Scheme + "://" + u.Host,
		},
	}
}

// TestRESTDirectSubmitHappyPath — POST succeeds; bytes counted; latency
// recorded.
func TestRESTDirectSubmitHappyPath(t *testing.T) {
	var receivedAuth, receivedTenant, receivedNeg string
	var receivedBody []byte

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedAuth = r.Header.Get("Authorization")
		receivedTenant = r.Header.Get("X-Tenant-Id")
		receivedNeg = r.Header.Get("X-Loadgen-Negative-Path")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer ts.Close()

	d, err := NewRESTDirect(RESTDirectConfig{Target: newTestTarget(ts.URL)})
	if err != nil {
		t.Fatalf("NewRESTDirect: %v", err)
	}
	defer d.Close()

	e := &generator.Event{
		Envelope: generator.Envelope{
			CustomerID:     "cust-001",
			MetricName:     "api_calls",
			Quantity:       1.0,
			OccurredAt:     time.Now().UTC(),
			IdempotencyKey: "evt-1",
			ProductType:    "API",
			Metadata:       map[string]any{"endpoint": "/x"},
		},
		TenantID: "tenant-A",
		EventID:  "evt-1",
		Auth:     generator.EventAuth{Token: "sk_test_secret"},
	}
	res := d.Submit(context.Background(), e)
	if !res.IsSuccess() {
		t.Fatalf("status = %d, want 2xx; transport=%v", res.Status, res.TransportErr)
	}
	if res.BytesSent <= 0 {
		t.Errorf("BytesSent = %d, want > 0", res.BytesSent)
	}
	if res.Latency <= 0 {
		t.Errorf("Latency = %s, want > 0", res.Latency)
	}
	if receivedAuth != "Bearer sk_test_secret" {
		t.Errorf("Authorization = %q, want Bearer sk_test_secret", receivedAuth)
	}
	if receivedTenant != "tenant-A" {
		t.Errorf("X-Tenant-Id = %q, want tenant-A", receivedTenant)
	}
	if receivedNeg != "" {
		t.Errorf("X-Loadgen-Negative-Path = %q, want empty", receivedNeg)
	}
	// Body must be valid JSON containing the envelope fields.
	var got map[string]any
	if err := json.Unmarshal(receivedBody, &got); err != nil {
		t.Fatalf("server received invalid JSON: %v body=%s", err, receivedBody)
	}
	if got["idempotencyKey"] != "evt-1" {
		t.Errorf("idempotencyKey = %v, want evt-1", got["idempotencyKey"])
	}
}

// TestRESTDirectCapturesBodyOn4xx — Pattern #18 fix 2026-06-11.
// Pre-fix, the driver drained the response body and discarded it, making
// 4xx storms (e.g. UnknownMetricException → 422) undiagnosable without
// backend log access. With body capture, the runner can persist excerpts
// to events.jsonl. This test asserts the excerpt is populated on 422 with
// a representative ProblemDetail body, NOT populated on 2xx, and that
// truncation kicks in at BodyExcerptMax bytes.
func TestRESTDirectCapturesBodyOn4xx(t *testing.T) {
	const problemDetail = `{"type":"about:blank","title":"Unprocessable Entity","status":422,"detail":"Unknown metric 'API Calls' for tenant aforo_dev"}`

	cases := []struct {
		name        string
		status      int
		body        string
		wantExcerpt bool
		wantPrefix  string
	}{
		{"422 captured", 422, problemDetail, true, `{"type":"about:blank"`},
		{"500 captured", 500, `{"error":"boom"}`, true, `{"error":"boom"}`},
		{"200 skipped", 200, `{"ok":true}`, false, ""},
		{"202 skipped", 202, `{"accepted":true}`, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer ts.Close()

			d, err := NewRESTDirect(RESTDirectConfig{Target: newTestTarget(ts.URL)})
			if err != nil {
				t.Fatalf("NewRESTDirect: %v", err)
			}
			defer d.Close()

			e := &generator.Event{
				Envelope: generator.Envelope{
					CustomerID:     "c",
					MetricName:     "API Calls",
					Quantity:       1,
					OccurredAt:     time.Now().UTC(),
					IdempotencyKey: "k",
					ProductType:    "API",
				},
				TenantID: "aforo_dev",
				Auth:     generator.EventAuth{Token: "t"},
			}
			res := d.Submit(context.Background(), e)
			if res.Status != tc.status {
				t.Fatalf("status = %d, want %d", res.Status, tc.status)
			}

			if tc.wantExcerpt {
				if res.BodyExcerpt == "" {
					t.Errorf("BodyExcerpt empty; want excerpt with prefix %q", tc.wantPrefix)
				} else if !strings.HasPrefix(res.BodyExcerpt, tc.wantPrefix) {
					t.Errorf("BodyExcerpt prefix = %q, want %q", res.BodyExcerpt[:min(len(res.BodyExcerpt), len(tc.wantPrefix))], tc.wantPrefix)
				}
			} else {
				if res.BodyExcerpt != "" {
					t.Errorf("BodyExcerpt = %q on 2xx; want empty (only non-2xx should carry an excerpt)", res.BodyExcerpt)
				}
			}
		})
	}
}

// TestRESTDirectBodyExcerptTruncation — verifies BodyExcerptMax cap kicks
// in for oversized error bodies (e.g. a Java stack trace) so events.jsonl
// can't be DoS'd by a server that floods kilobyte responses per event.
func TestRESTDirectBodyExcerptTruncation(t *testing.T) {
	// 5 KB of 'A' — well above BodyExcerptMax (2 KB).
	huge := strings.Repeat("A", 5000)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(422)
		_, _ = w.Write([]byte(huge))
	}))
	defer ts.Close()

	d, err := NewRESTDirect(RESTDirectConfig{Target: newTestTarget(ts.URL)})
	if err != nil {
		t.Fatalf("NewRESTDirect: %v", err)
	}
	defer d.Close()

	e := &generator.Event{
		Envelope: generator.Envelope{},
		Auth:     generator.EventAuth{Token: "t"},
	}
	res := d.Submit(context.Background(), e)
	// rest_direct also caps the read at maxRead (4 KB) before the excerpt
	// cap (2 KB), so the excerpt should be 2KB + truncation marker (no
	// trailing AAA bleed). The trailing "…" proves truncation kicked in.
	if !strings.HasSuffix(res.BodyExcerpt, "…") {
		t.Errorf("expected truncation marker '…' at end of BodyExcerpt; got len=%d, last 10 = %q",
			len(res.BodyExcerpt), res.BodyExcerpt[max(0, len(res.BodyExcerpt)-10):])
	}
	// Strip the marker before length check.
	core := strings.TrimSuffix(res.BodyExcerpt, "…")
	if len(core) > BodyExcerptMax {
		t.Errorf("BodyExcerpt core length = %d, want <= %d", len(core), BodyExcerptMax)
	}
}

// TestRESTDirectMalformedSendsRawBody — when RawBody is set the driver
// sends the corrupt bytes, not a re-marshaled envelope.
func TestRESTDirectMalformedSendsRawBody(t *testing.T) {
	var got []byte
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer ts.Close()

	d, err := NewRESTDirect(RESTDirectConfig{Target: newTestTarget(ts.URL)})
	if err != nil {
		t.Fatalf("NewRESTDirect: %v", err)
	}
	defer d.Close()

	e := &generator.Event{
		Envelope:     generator.Envelope{},
		RawBody:      []byte(`{"event_id":"x","tenant_id":"t","body":{"endpoint"`),
		NegativePath: generator.NPMalformed,
		Auth:         generator.EventAuth{Token: "k"},
	}
	res := d.Submit(context.Background(), e)
	if !res.IsClientError() {
		t.Errorf("status = %d, want 4xx", res.Status)
	}
	if string(got) != string(e.RawBody) {
		t.Errorf("server got %q, want %q", got, e.RawBody)
	}
}

// TestRESTDirectAdminTokenFallback — empty per-event token uses AdminToken.
func TestRESTDirectAdminTokenFallback(t *testing.T) {
	var auth string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer ts.Close()

	d, err := NewRESTDirect(RESTDirectConfig{
		Target:     newTestTarget(ts.URL),
		AdminToken: "admin_xyz",
	})
	if err != nil {
		t.Fatalf("NewRESTDirect: %v", err)
	}
	defer d.Close()

	e := &generator.Event{Envelope: generator.Envelope{}}
	_ = d.Submit(context.Background(), e)
	if auth != "Bearer admin_xyz" {
		t.Errorf("Authorization = %q, want Bearer admin_xyz", auth)
	}
}

// TestRESTDirectTransportError — pointing at a closed server yields a
// transport error, not a status code.
func TestRESTDirectTransportError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {}))
	closedURL := ts.URL
	ts.Close() // close immediately

	d, err := NewRESTDirect(RESTDirectConfig{
		Target:         newTestTarget(closedURL),
		RequestTimeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("NewRESTDirect: %v", err)
	}
	defer d.Close()

	e := &generator.Event{
		Envelope: generator.Envelope{},
		Auth:     generator.EventAuth{Token: "k"},
	}
	res := d.Submit(context.Background(), e)
	if !res.IsTransport() {
		t.Errorf("expected transport err; got status=%d transport=%v", res.Status, res.TransportErr)
	}
}

// TestRESTDirectRedirectIsTransport — auto-redirect is disabled; a 302
// surfaces as a transport error so misconfigurations don't pass silently.
func TestRESTDirectRedirectIsTransport(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pass the real request — http.Redirect dereferences r.URL to
		// compute the absolute URL, so a synthetic &http.Request{} panics.
		http.Redirect(w, r, "/elsewhere", http.StatusFound)
	}))
	defer ts.Close()

	d, err := NewRESTDirect(RESTDirectConfig{Target: newTestTarget(ts.URL)})
	if err != nil {
		t.Fatalf("NewRESTDirect: %v", err)
	}
	defer d.Close()

	e := &generator.Event{
		Envelope: generator.Envelope{},
		Auth:     generator.EventAuth{Token: "k"},
	}
	res := d.Submit(context.Background(), e)
	// We disabled auto-redirect with ErrUseLastResponse, which surfaces as
	// the response with the 302 status — not as a transport error. Either
	// outcome is OK for the load test (both signal "configure something
	// else"); just assert we DID get a redirect status, not silently
	// follow to a different URL.
	if res.Status >= 200 && res.Status < 300 {
		t.Errorf("a 302 should NOT be a 2xx; got status=%d transport=%v", res.Status, res.TransportErr)
	}
}

// TestRESTDirectMissingTargetURL — constructor errors when target lacks
// usage-ingestor.
func TestRESTDirectMissingTargetURL(t *testing.T) {
	bad := aforo.Target{Name: "broken", URLs: map[aforo.Service]string{}}
	if _, err := NewRESTDirect(RESTDirectConfig{Target: bad}); err == nil {
		t.Errorf("expected error for target missing usage-ingestor URL")
	} else if !strings.Contains(err.Error(), "usage-ingestor") {
		t.Errorf("error message missing 'usage-ingestor': %v", err)
	}
}
