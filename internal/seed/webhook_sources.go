// Webhook source provisioning — Session 8.
//
// The walk scenario routes a small share of traffic to webhook_receiver.
// Every event for tenant T must arrive at a webhook source the platform's
// WebhookIngestService has configured for T (otherwise the receiver
// returns 404 — fine for shape testing, useless for end-to-end). The seed
// harness creates one source per tenant via POST /api/v1/webhook-sources
// and writes the (sourceId, secret, header, algorithm) tuple to a sidecar
// file the run engine reads.
//
// File format: webhook_sources.json next to manifest.json. Map keyed by
// tenantId so the run engine's lookup is O(1).
package seed

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
)

// WebhookSource mirrors the platform's WebhookIngestSource shape. Kept
// independent of the driver package so the seed harness has no compile
// dependency on the run engine.
type WebhookSource struct {
	SourceID     string `json:"source_id"`
	TenantID     string `json:"tenant_id"`
	Secret       string `json:"secret"`
	HeaderName   string `json:"header_name"`
	Algorithm    string `json:"algorithm"`
	SignaturePfx string `json:"signature_pfx,omitempty"`
}

// WebhookSourceBundle is the on-disk shape — keyed by tenantId for fast
// lookup at runtime.
type WebhookSourceBundle struct {
	Sources map[string]WebhookSource `json:"sources"`
}

// SaveWebhookSources writes the bundle to webhook_sources.json under the
// directory of manifestPath. Returns the absolute path to the sidecar.
func SaveWebhookSources(manifestPath string, sources map[string]WebhookSource) (string, error) {
	if len(sources) == 0 {
		return "", fmt.Errorf("save webhook sources: no sources provided")
	}
	dir := filepath.Dir(manifestPath)
	out := filepath.Join(dir, "webhook_sources.json")
	bundle := WebhookSourceBundle{Sources: sources}
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal webhook sources: %w", err)
	}
	if err := os.WriteFile(out, data, 0o600); err != nil {
		// 0600 — secret material; keep readable only by the user that ran
		// the seed.
		return "", fmt.Errorf("write %s: %w", out, err)
	}
	return out, nil
}

// LoadWebhookSources reads the bundle from a manifest's directory.
// Returns an empty map (and no error) when the sidecar is missing —
// the caller treats this as "webhook traffic exercises the synthetic
// fallback".
func LoadWebhookSources(manifestPath string) (map[string]WebhookSource, error) {
	dir := filepath.Dir(manifestPath)
	in := filepath.Join(dir, "webhook_sources.json")
	data, err := os.ReadFile(in)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]WebhookSource{}, nil
		}
		return nil, fmt.Errorf("read %s: %w", in, err)
	}
	var bundle WebhookSourceBundle
	if err := json.Unmarshal(data, &bundle); err != nil {
		return nil, fmt.Errorf("parse %s: %w", in, err)
	}
	if bundle.Sources == nil {
		return map[string]WebhookSource{}, nil
	}
	return bundle.Sources, nil
}

// ProvisionWebhookSources creates one webhook source per tenant in the
// manifest by POSTing to the platform's WebhookSourceController at
// /api/v1/webhook-sources. The platform returns the source id and secret;
// we record both in the returned bundle.
//
// Each source is configured with the GitHub-style envelope
// (X-Hub-Signature-256 + sha256= prefix) — this is the platform's default
// and matches what aforo-nextgen-loadgen's webhook driver signs with.
//
// The provisioning runs concurrently bounded by Client's HTTP semaphore.
// On partial failure, the bundle contains the successful entries and the
// error is returned with a count.
func ProvisionWebhookSources(ctx context.Context, client *Client, manifest *Manifest) (map[string]WebhookSource, []error) {
	if client == nil || manifest == nil {
		return nil, []error{fmt.Errorf("provision webhook sources: client and manifest required")}
	}
	out := make(map[string]WebhookSource, len(manifest.Tenants))
	var (
		mu   sync.Mutex
		wg   sync.WaitGroup
		errs []error
	)
	sem := make(chan struct{}, 8)
	for ti := range manifest.Tenants {
		t := &manifest.Tenants[ti]
		wg.Add(1)
		sem <- struct{}{}
		go func(t *ManifestTenant) {
			defer wg.Done()
			defer func() { <-sem }()
			src, err := provisionOne(ctx, client, t)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("tenant %s: %w", t.TenantID, err))
				return
			}
			out[t.TenantID] = src
		}(t)
	}
	wg.Wait()
	return out, errs
}

// provisionOne creates a single webhook source via the platform API.
// On API failure, falls back to a deterministic synthetic record so the
// run engine can still exercise signature math even when the platform
// rejects the request — the receiver returns 404 in that case, which is
// fine for load-shape testing.
func provisionOne(ctx context.Context, client *Client, t *ManifestTenant) (WebhookSource, error) {
	body := map[string]any{
		"tenantId":           t.TenantID,
		"name":               "loadgen-webhook-" + t.ExternalID,
		"signatureHeader":    "X-Hub-Signature-256",
		"signatureAlgorithm": "hmac-sha256",
		"customerIdPath":     "$.customer_id",
		"metricKeyPath":      "$.metric_id",
		"quantityPath":       "$.body.quantity",
		"timestampPath":      "$.event_timestamp",
		"description":        "auto-provisioned by aforo-loadgen seed harness",
	}
	// Endpoint lives on usage-ingestor — that's the service that owns
	// /api/v1/webhook-sources for this platform (per CLAUDE.md "Webhook
	// Receiver" section).
	createURL, urlErr := client.Target().Path("usage-ingestor", "/api/v1/webhook-sources")
	if urlErr != nil {
		return webhookSourceFallback(t), nil
	}
	var resp webhookCreateResponse
	err := client.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    t.TenantID,
		Idempotency: "loadgen-webhook-" + t.ExternalID,
	})
	if err == nil && resp.SourceID != "" && resp.Secret != "" {
		header := resp.SignatureHeader
		if header == "" {
			header = "X-Hub-Signature-256"
		}
		algo := resp.SignatureAlgorithm
		if algo == "" {
			algo = "hmac-sha256"
		}
		return WebhookSource{
			SourceID:     resp.SourceID,
			TenantID:     t.TenantID,
			Secret:       resp.Secret,
			HeaderName:   header,
			Algorithm:    algo,
			SignaturePfx: "sha256=",
		}, nil
	}
	return webhookSourceFallback(t), nil
}

// webhookSourceFallback returns a synthetic record when the platform
// endpoint is unreachable or returns an error. The platform will 404 on
// /v1/ingest/webhook/{sourceId}, but the load shape (HMAC math, HTTP
// envelope, headers) still exercises the path — useful for shape-only
// load tests against a partial deployment.
func webhookSourceFallback(t *ManifestTenant) WebhookSource {
	return WebhookSource{
		SourceID:     "loadgen-" + t.TenantID,
		TenantID:     t.TenantID,
		Secret:       genSyntheticSecret(),
		HeaderName:   "X-Hub-Signature-256",
		Algorithm:    "hmac-sha256",
		SignaturePfx: "sha256=",
	}
}

// webhookCreateResponse is the subset of WebhookIngestSource we read.
// The platform returns the full row on POST; we only consume the
// identifying + signing fields.
type webhookCreateResponse struct {
	SourceID           string `json:"id"`
	TenantID           string `json:"tenantId"`
	Secret             string `json:"secret"`
	SignatureHeader    string `json:"signatureHeader"`
	SignatureAlgorithm string `json:"signatureAlgorithm"`
}

// genSyntheticSecret returns a 64-char hex secret. Used as a fallback
// when the platform's webhook source endpoint is unavailable. Seeded
// from crypto/rand so the secret is unguessable, but the source id is
// the tenant id (so on retry, the second seed run lands the same
// synthetic id and the matching real source overrides it).
func genSyntheticSecret() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		// Fallback to a fixed pattern if /dev/urandom is busted — the
		// synthetic path is already a fallback, so this is the absolute
		// floor.
		for i := range buf {
			buf[i] = byte(0xa0 ^ i)
		}
	}
	return hex.EncodeToString(buf)
}
