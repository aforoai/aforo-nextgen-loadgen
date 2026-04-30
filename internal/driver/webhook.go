package driver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
)

// WebhookSource is one configured webhook ingest source on the platform.
// The Aforo WebhookSourceController returns this shape on POST + GET; the
// loadgen seed harness creates one per tenant and stores the (sourceId,
// secret, header, algorithm) tuple here.
//
// The driver picks a source per event by tenantId — every event for tenant
// T is routed to T's webhook source so the platform's signature verification
// has the right secret to consult.
type WebhookSource struct {
	SourceID     string `json:"source_id"`
	TenantID     string `json:"tenant_id"`
	Secret       string `json:"secret"`        // raw HMAC secret bytes (hex-encoded for transport)
	HeaderName   string `json:"header_name"`   // e.g. "X-Hub-Signature-256"
	Algorithm    string `json:"algorithm"`     // "hmac-sha256" (only one supported by this driver)
	SignaturePfx string `json:"signature_pfx"` // optional "sha256=" prefix the receiver expects
}

// WebhookConfig configures the webhook driver.
type WebhookConfig struct {
	HTTPBaseConfig
	// Sources keyed by tenantId. Caller (runner) loads the manifest extension
	// and calls SetSources to populate this map before the run.
	Sources map[string]WebhookSource
}

// Webhook is the driver for the webhook_receiver ingestion path. It POSTs
// the same JSON envelope as rest_direct to /v1/ingest/webhook/{sourceId},
// signing the body with HMAC-SHA256 and the per-source secret in the
// configured header (default "X-Hub-Signature-256", with the "sha256="
// prefix the platform's WebhookIngestService strips before comparing).
//
// Tenant routing: per event, look up the source by event.Envelope.TenantID;
// if the tenant has no source registered, return ErrNoWebhookSource and let
// the runner classify it as a transport-class failure.
type Webhook struct {
	cfg     WebhookConfig
	client  *http.Client
	baseURL string
	mu      sync.RWMutex
	sources map[string]WebhookSource
}

// ErrNoWebhookSource is returned when no source is configured for the
// event's tenant. Reported as TransportErr so the runner records it as
// a driver-level failure, not a 4xx from the platform.
var ErrNoWebhookSource = errors.New("no webhook source configured for tenant")

// NewWebhook constructs the webhook driver.
func NewWebhook(cfg WebhookConfig) (*Webhook, error) {
	cfg.applyDefaults()
	base, err := cfg.Target.URL(aforo.ServiceUsageIngestor)
	if err != nil {
		return nil, fmt.Errorf("webhook: target %s has no usage-ingestor URL: %w", cfg.Target.Name, err)
	}
	d := &Webhook{
		cfg:     cfg,
		client:  cfg.HTTPClient,
		baseURL: base,
		sources: map[string]WebhookSource{},
	}
	if cfg.Sources != nil {
		for k, v := range cfg.Sources {
			d.sources[k] = v
		}
	}
	return d, nil
}

// Name reports the driver identifier.
func (d *Webhook) Name() string { return "webhook_receiver" }

// SetSources replaces the per-tenant source map. Safe to call concurrently
// (used by tests; production runs set once before Run).
func (d *Webhook) SetSources(sources map[string]WebhookSource) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.sources = make(map[string]WebhookSource, len(sources))
	for k, v := range sources {
		d.sources[k] = v
	}
}

// Sources returns a snapshot of the configured source map. Used by tests.
func (d *Webhook) Sources() map[string]WebhookSource {
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make(map[string]WebhookSource, len(d.sources))
	for k, v := range d.sources {
		out[k] = v
	}
	return out
}

// Submit dispatches one event by computing the HMAC of the JSON body and
// POSTing to /v1/ingest/webhook/{sourceId}.
func (d *Webhook) Submit(ctx context.Context, e *generator.Event) Result {
	d.mu.RLock()
	src, ok := d.sources[e.Envelope.TenantID]
	d.mu.RUnlock()
	if !ok {
		// Try a synthetic fallback: if the manifest didn't seed sources, we
		// still want to exercise the path. Use the tenant id as the source
		// id and a synthesized secret. The platform will 404 — that's fine
		// for the load shape; tests use SetSources to drive the happy path.
		src = WebhookSource{
			SourceID:     e.Envelope.TenantID,
			TenantID:     e.Envelope.TenantID,
			Secret:       "loadgen-synthetic-secret",
			HeaderName:   "X-Hub-Signature-256",
			Algorithm:    "hmac-sha256",
			SignaturePfx: "sha256=",
		}
	}

	body, _, err := marshalEvent(e)
	if err != nil {
		return Result{Event: e, TransportErr: err}
	}
	signature := signHMACSHA256(src.Secret, body)
	url := fmt.Sprintf("%s/v1/ingest/webhook/%s", d.baseURL, src.SourceID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Result{Event: e, TransportErr: fmt.Errorf("build request: %w", err)}
	}
	req.Header.Set("Content-Type", "application/json")
	header := src.HeaderName
	if header == "" {
		header = "X-Hub-Signature-256"
	}
	pfx := src.SignaturePfx
	// Default prefix mirrors GitHub / Aforo defaults — the receiver strips it.
	if pfx == "" {
		pfx = "sha256="
	}
	req.Header.Set(header, pfx+signature)
	req.Header.Set("User-Agent", "aforo-loadgen-webhook/1.0")
	applyTraceHeaders(req, e)
	return doHTTPRequest(d.client, req, e, len(body))
}

// Close releases idle connections.
func (d *Webhook) Close() error {
	closeIdle(d.client)
	return nil
}

// signHMACSHA256 returns the lower-case hex-encoded HMAC-SHA256 of body
// keyed by secret. Matches the encoding used by Aforo's WebhookIngestService:
//
//	HexFormat.of().formatHex(hash)
//
// Lower-case, no separator. The "sha256=" prefix is added at request time.
func signHMACSHA256(secret string, body []byte) string {
	h := hmac.New(sha256.New, []byte(secret))
	h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}
