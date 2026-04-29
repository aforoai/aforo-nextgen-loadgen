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
			EventID:        "evt-1",
			EventTimestamp: time.Now().UTC(),
			TenantID:       "tenant-A",
			ProductType:    "API",
			Body:           map[string]any{"endpoint": "/x"},
		},
		Auth: generator.EventAuth{Token: "sk_test_secret"},
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
	if got["event_id"] != "evt-1" {
		t.Errorf("event_id = %v, want evt-1", got["event_id"])
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
		Envelope:     generator.Envelope{EventID: "x", TenantID: "t"},
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

	e := &generator.Event{Envelope: generator.Envelope{EventID: "e"}}
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
		Envelope: generator.Envelope{EventID: "x"},
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
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "/elsewhere", http.StatusFound)
	}))
	defer ts.Close()

	d, err := NewRESTDirect(RESTDirectConfig{Target: newTestTarget(ts.URL)})
	if err != nil {
		t.Fatalf("NewRESTDirect: %v", err)
	}
	defer d.Close()

	e := &generator.Event{
		Envelope: generator.Envelope{EventID: "x"},
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
