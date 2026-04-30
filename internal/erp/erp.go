// Package erp drives ERP-sync verification for Session 9.
//
// The platform's ErpSyncService is the source of truth — it owns retries
// (3 attempts, exponential backoff), per-provider OAuth/TBA flows, and
// erp_sync_log persistence. The loadgen's job is simpler: assert that
// every issued invoice landed in the configured ERP within the SLA
// (scenario.erp.sync_sla_seconds), with a non-empty externalDocumentId
// that we can independently look up via the provider sandbox.
//
// Provider clients here are SHIMS — they don't reimplement OAuth flows.
// They take a provider-specific access token from environment variables
// and verify the existence of a document by id. When credentials aren't
// set, the provider runs in "shadow" mode: assumes the platform's
// erp_sync_log is correct and validates only the platform-side artifact.
//
// Every provider satisfies the Provider interface so the validator can
// iterate over any tenant's configured ERP uniformly.
package erp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

// Provider is the validation contract for one ERP. Implementations: QuickBooks,
// Xero, NetSuite, CustomWebhook.
type Provider interface {
	// Name returns the canonical provider id (matches scenario.erp.providers_per_tenant_mix).
	Name() string

	// Verify checks that an invoice with the given external_document_id
	// exists in the provider's sandbox. Returns ok=false with a non-error
	// reason when the doc isn't present (caller treats as sync miss).
	//
	// In shadow mode (no creds), Verify returns ok=true unconditionally —
	// the validator falls back to checking only the platform's
	// erp_sync_log.
	Verify(ctx context.Context, externalID string) (ok bool, reason string, err error)

	// IsLive reports whether the provider is talking to a real sandbox or
	// running shadow-only. Reported in Check 15 for transparency.
	IsLive() bool
}

// SyncRecord is one row of erp_sync.jsonl — emitted by sync_validator after
// it observes the platform's erp_sync_log entry for one invoice.
type SyncRecord struct {
	Timestamp      time.Time `json:"ts"`
	InvoiceID      string    `json:"invoice_id"`
	TenantID       string    `json:"tenant_id"`
	Provider       string    `json:"provider"`
	ExternalID     string    `json:"external_id"`
	Status         string    `json:"status"` // "synced" | "failed" | "pending" | "missing"
	LatencySeconds float64   `json:"latency_seconds"`
	Attempts       int       `json:"attempts,omitempty"`
	Verified       bool      `json:"verified"` // ok from Provider.Verify
	VerifyReason   string    `json:"verify_reason,omitempty"`
	Note           string    `json:"note,omitempty"`
}

// httpDoer is the minimal http.Client surface — extracted so tests can stub.
type httpDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// commonClient is shared boilerplate across the four provider shims.
type commonClient struct {
	baseURL    string
	authHeader string
	httpClient httpDoer
	enabled    bool
	note       string
}

func (c *commonClient) get(ctx context.Context, path string) (*http.Response, error) {
	if !c.enabled {
		return nil, errors.New("erp: provider running in shadow mode")
	}
	endpoint := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if c.authHeader != "" {
		req.Header.Set("Authorization", c.authHeader)
	}
	req.Header.Set("Accept", "application/json")
	return c.httpClient.Do(req)
}

// notFound returns true for 404 responses.
func notFound(resp *http.Response) bool {
	return resp != nil && resp.StatusCode == http.StatusNotFound
}

// statusOK returns true for 2xx.
func statusOK(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// AllProviders lists the canonical set used by scenarios + validators.
var AllProviders = []string{"quickbooks", "xero", "netsuite", "custom_webhook"}

// IsKnown reports whether p is one of the supported provider ids.
func IsKnown(p string) bool {
	for _, x := range AllProviders {
		if p == x {
			return true
		}
	}
	return false
}

// Build constructs a Provider by id. Pulls credentials from per-provider env
// vars; falls back to shadow mode when unset.
func Build(name string) (Provider, error) {
	switch name {
	case "quickbooks":
		return NewQuickBooks(), nil
	case "xero":
		return NewXero(), nil
	case "netsuite":
		return NewNetSuite(), nil
	case "custom_webhook":
		return NewCustomWebhook(), nil
	default:
		return nil, fmt.Errorf("erp: unknown provider %q", name)
	}
}
