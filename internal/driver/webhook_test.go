package driver

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestWebhook_HMACSigningMatchesAforoVerification reproduces the
// platform's WebhookIngestService verification:
//
//	HexFormat.of().formatHex(HMAC-SHA256(secret, body))
//
// The receiver strips the "sha256=" prefix and compares the hex digest
// using MessageDigest.isEqual (constant-time). We compute the same digest
// and assert byte-for-byte equality.
func TestWebhook_HMACSigningMatchesAforoVerification(t *testing.T) {
	secret := "shhh-very-secret-1234"
	body := []byte(`{"event":"test","payload":{"id":42}}`)

	got := signHMACSHA256(secret, body)

	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))

	if got != want {
		t.Fatalf("HMAC mismatch:\n  got  %s\n  want %s", got, want)
	}
	if len(got) != 64 {
		t.Errorf("hex digest length: got %d want 64", len(got))
	}
	if strings.ToLower(got) != got {
		t.Errorf("hex digest must be lower-case: got %q", got)
	}
}

// TestWebhook_DriverPostsToCorrectEndpoint verifies the URL shape and
// the signature header / prefix the receiver expects.
func TestWebhook_DriverPostsToCorrectEndpoint(t *testing.T) {
	var captured *http.Request
	var capturedBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(context.Background())
		body, _ := io.ReadAll(r.Body)
		capturedBody = body
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()

	d, err := NewWebhook(WebhookConfig{
		HTTPBaseConfig: HTTPBaseConfig{Target: targetForServer(srv)},
		Sources: map[string]WebhookSource{
			"tenant-alpha": {
				SourceID:     "src-tenant-alpha",
				TenantID:     "tenant-alpha",
				Secret:       "test-secret",
				HeaderName:   "X-Hub-Signature-256",
				Algorithm:    "hmac-sha256",
				SignaturePfx: "sha256=",
			},
		},
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	defer func() { _ = d.Close() }()

	res := d.Submit(context.Background(), newTestEvent())
	if !res.IsSuccess() {
		t.Fatalf("expected 2xx, got status=%d err=%v", res.Status, res.TransportErr)
	}
	if got := captured.URL.Path; got != "/v1/ingest/webhook/src-tenant-alpha" {
		t.Errorf("URL: got %q want /v1/ingest/webhook/src-tenant-alpha", got)
	}
	sig := captured.Header.Get("X-Hub-Signature-256")
	if !strings.HasPrefix(sig, "sha256=") {
		t.Errorf("signature must carry sha256= prefix, got %q", sig)
	}
	hex := strings.TrimPrefix(sig, "sha256=")
	if hex != signHMACSHA256("test-secret", capturedBody) {
		t.Errorf("signature does not match HMAC of captured body")
	}
}

// TestWebhook_FallbackWhenNoSourceConfigured ensures the driver still
// fires (even if the receiver returns 404) when no source bundle was
// loaded — the load shape is still useful for circuit-breaker testing.
func TestWebhook_FallbackWhenNoSourceConfigured(t *testing.T) {
	var captured *http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured = r.Clone(context.Background())
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	d, err := NewWebhook(WebhookConfig{
		HTTPBaseConfig: HTTPBaseConfig{Target: targetForServer(srv)},
	})
	if err != nil {
		t.Fatalf("NewWebhook: %v", err)
	}
	defer func() { _ = d.Close() }()

	res := d.Submit(context.Background(), newTestEvent())
	if res.Status != http.StatusNotFound {
		t.Fatalf("expected 404 from synthetic fallback, got status=%d err=%v", res.Status, res.TransportErr)
	}
	if captured == nil || !strings.HasPrefix(captured.URL.Path, "/v1/ingest/webhook/") {
		t.Errorf("synthetic fallback should still POST to /v1/ingest/webhook/...; got path=%q", captured.URL.Path)
	}
}
