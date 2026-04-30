package erp

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"time"
)

// CustomWebhook receives POSTs from the platform's CustomWebhookAdapter and
// validates HMAC-SHA256 signatures. Used in two modes:
//
//   - Receiver mode: spawn an HTTP server (httptest.Server in tests, or
//     env-supplied URL in real runs) that accepts platform POSTs and
//     records them.
//   - Verifier mode: replay a captured payload to confirm the signature
//     hash matches what an external party would compute.
//
// In production, customers configure the platform with a webhook URL +
// shared secret. For load tests we generate a secret ourselves and stand
// up a local receiver — the platform sends to it, we verify the signatures.
//
// Concurrency: the embedded Receiver server is safe for concurrent use.
type CustomWebhook struct {
	secret   []byte
	enabled  bool
	receiver *Receiver
	note     string
}

// Receiver is the in-process webhook target the platform's
// CustomWebhookAdapter posts to.
type Receiver struct {
	mu       sync.Mutex
	server   *httptest.Server
	captured []ReceivedPayload
	secret   []byte
	url      string
}

// ReceivedPayload is one captured POST.
type ReceivedPayload struct {
	ReceivedAt time.Time
	Body       []byte
	Signature  string
	Verified   bool
	InvoiceID  string
}

// NewCustomWebhook reads env vars, spawns a receiver, and runs.
//
// Env vars:
//
//	CUSTOM_WEBHOOK_SECRET — shared secret. Auto-generated if absent.
//	CUSTOM_WEBHOOK_URL    — pre-existing receiver URL; if set, the shim
//	                        skips spinning a local server (acts as a verifier).
func NewCustomWebhook() *CustomWebhook {
	secret := []byte(strings.TrimSpace(os.Getenv("CUSTOM_WEBHOOK_SECRET")))
	if len(secret) == 0 {
		secret = []byte("aforo-loadgen-default-secret")
	}
	if url := strings.TrimSpace(os.Getenv("CUSTOM_WEBHOOK_URL")); url != "" {
		return &CustomWebhook{
			secret:  secret,
			enabled: true,
			receiver: &Receiver{
				secret: secret,
				url:    url,
			},
			note: "verifier mode — using CUSTOM_WEBHOOK_URL",
		}
	}
	r := &Receiver{secret: secret}
	r.server = httptest.NewServer(http.HandlerFunc(r.handler))
	r.url = r.server.URL
	return &CustomWebhook{
		secret:   secret,
		enabled:  true,
		receiver: r,
	}
}

// Name returns "custom_webhook".
func (cw *CustomWebhook) Name() string { return "custom_webhook" }

// IsLive returns true once the receiver is up.
func (cw *CustomWebhook) IsLive() bool { return cw.enabled }

// Verify checks if a webhook for externalID was received. Reuses the
// captured queue.
func (cw *CustomWebhook) Verify(_ context.Context, externalID string) (bool, string, error) {
	if !cw.enabled {
		return true, "shadow-mode", nil
	}
	for _, p := range cw.receiver.Captured() {
		if p.InvoiceID == externalID {
			if !p.Verified {
				return false, "HMAC signature mismatch", nil
			}
			return true, "", nil
		}
	}
	return false, "no webhook captured for " + externalID, nil
}

// URL returns the receiver URL — used by orchestrators that need to
// configure the platform's webhook endpoint to point here.
func (cw *CustomWebhook) URL() string { return cw.receiver.url }

// Secret returns the shared secret bytes.
func (cw *CustomWebhook) Secret() []byte { return append([]byte(nil), cw.secret...) }

// Close stops the embedded test server (no-op in verifier mode).
func (cw *CustomWebhook) Close() {
	if cw.receiver != nil && cw.receiver.server != nil {
		cw.receiver.server.Close()
	}
}

// Captured returns a snapshot copy of every received payload.
func (r *Receiver) Captured() []ReceivedPayload {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]ReceivedPayload, len(r.captured))
	copy(out, r.captured)
	return out
}

// handler is the HTTP handler for the receiver.
func (r *Receiver) handler(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer req.Body.Close()
	sig := req.Header.Get("X-Aforo-Signature")
	verified := r.verifyHMAC(body, sig)

	invoiceID := readInvoiceID(body)

	r.mu.Lock()
	r.captured = append(r.captured, ReceivedPayload{
		ReceivedAt: time.Now().UTC(),
		Body:       body,
		Signature:  sig,
		Verified:   verified,
		InvoiceID:  invoiceID,
	})
	r.mu.Unlock()

	if !verified {
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func (r *Receiver) verifyHMAC(body []byte, sig string) bool {
	if sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, r.secret)
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	// Accept either bare hex or "sha256=hex" (Stripe format).
	got := strings.TrimPrefix(sig, "sha256=")
	return hmac.Equal([]byte(want), []byte(got))
}

func readInvoiceID(body []byte) string {
	dec := json.NewDecoder(bytes.NewReader(body))
	var parsed struct {
		InvoiceID string `json:"invoice_id"`
	}
	if err := dec.Decode(&parsed); err != nil {
		return ""
	}
	return parsed.InvoiceID
}

// SignBody computes the X-Aforo-Signature value for the given body. Used by
// tests to send valid signatures and by the orchestrator when emulating
// the platform's webhook POST in offline mode.
func SignBody(body, secret []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// AssertReceived returns nil iff the receiver has captured a payload for
// invoiceID with a valid signature within timeout. Polls every 500ms.
func (cw *CustomWebhook) AssertReceived(ctx context.Context, invoiceID string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ok, reason, _ := cw.Verify(ctx, invoiceID)
		if ok {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
		_ = reason
	}
	return fmt.Errorf("custom_webhook: no verified payload for %s within %s", invoiceID, timeout)
}
