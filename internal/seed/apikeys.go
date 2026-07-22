package seed

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// credentialTypeForProductType returns the credential family Aforo issues for
// each product type. CLAUDE.md "Credential Types" table:
//
//	Standard API     → BEARER_TOKEN  (sk_live_xxx)
//	Agentic API      → BEARER_TOKEN  (sk_live_xxx)
//	AI Agent         → CLIENT_CREDENTIALS  (client_id + client_secret)
//	MCP Server       → CLIENT_CREDENTIALS  (client_id + client_secret)
//
// The credentialType is NOT a request input — pricing-service resolves it
// internally from the accessor's product type. This helper is kept to drive
// loadgen's manifest + dry-run synthesis only.
func credentialTypeForProductType(pt scenario.ProductType) string {
	switch pt {
	case scenario.ProductAIAgent, scenario.ProductMCPServer:
		return "CLIENT_CREDENTIALS"
	default:
		return "BEARER_TOKEN"
	}
}

// accessorTypeForProductType maps the product type to pricing-service's
// AccessorType enum (APP | AGENT). CLAUDE.md "Hierarchy":
//
//	Standard API / Agentic API → App   → API Key (accessorType=APP)
//	AI Agent      / MCP Server → Agent → API Key (accessorType=AGENT)
func accessorTypeForProductType(pt scenario.ProductType) string {
	switch pt {
	case scenario.ProductAIAgent, scenario.ProductMCPServer:
		return "AGENT"
	default:
		return "APP"
	}
}

// apiKeyCreateRequest mirrors pricing-service's CreateApiKeyRequest.
//
// Field-name + required-field contract (verified against pricing-service
// CreateApiKeyRequest.java):
//   - accessorId     — @NotBlank, the App or Agent id that owns this key.
//     pricing-service does NOT validate that the row exists
//     in customer-service (it just stamps the column), so
//     loadgen synthesizes "loadgen-{app|agent}-{subId}".
//   - accessorType   — @NotBlank, APP | AGENT.
//   - customerId     — @NotBlank, must match the subscription's customer.
//   - subscriptionIds — @NotEmpty list. Subscription must be ACTIVE; key
//     create is therefore wired BEFORE
//     transitionSubscription in the seeder so the sub is
//     still in its initial ACTIVE state.
//   - name           — optional human-readable key name (carries what the
//     previous "description" field tried to record).
//   - environment    — optional "live" | "test"; defaults server-side to
//     "live" so we send live for stability.
//   - scopes         — optional comma-separated permissions; defaults to
//     "read" server-side.
//
// CONVENTION (see CONVENTIONS.md "Wire-format alignment"): EVERY field on
// this struct maps to a real CreateApiKeyRequest column. Deterministic
// identity for cross-day lookup is the (customerId, accessorId) pair
// (queried via real ?customerId=&accessorId= filters by
// lookupAPIKeyByAccessor). Idempotency-Key is the loadgen-internal
// seedKey set by provisionAPIKey.
type apiKeyCreateRequest struct {
	AccessorID      string   `json:"accessorId"`
	AccessorType    string   `json:"accessorType"`
	CustomerID      string   `json:"customerId"`
	SubscriptionIDs []string `json:"subscriptionIds"`
	Name            string   `json:"name,omitempty"`
	Environment     string   `json:"environment,omitempty"`
	Scopes          string   `json:"scopes,omitempty"`
}

// apiKeyResponse mirrors the subset of pricing-service's ApiKeyResponse
// that the seed harness consumes.
//
// Drift-fix (2026-05-27):
//   - Removed `json:"externalId"` — pricing-service has no such field on
//     the ApiKey entity or DTO (verified against
//     aforo-nextgen-pricing-service/.../ApiKeyResponse.java). The previous
//     loadgen response struct read `json:"externalId"` which always
//     decoded to "" and made every lookupAPIKeyByExternalID call return
//     "not found", silently fanning out duplicate key creates on cross-day
//     reruns.
//   - Removed `json:"revoked"` — backend uses `status: REVOKED` enum, not
//     a boolean. The previous boolean tag would always decode to false.
//   - Added `accessorId` + `customerId` — the deterministic identity pair
//     that drives lookupAPIKeyByAccessor. (Backend supports both as
//     server-side filters: GET /api/v1/api-keys?customerId=X&accessorId=Y.)
//   - Added `status` enum mirror so callers see PENDING_REVOCATION /
//     REVOKED / ACTIVE etc. instead of a lossy boolean.
type apiKeyResponse struct {
	ID             string     `json:"id"`
	CustomerID     string     `json:"customerId"`
	AccessorID     string     `json:"accessorId"`
	AccessorType   string     `json:"accessorType"`
	CredentialType string     `json:"credentialType"`
	Status         string     `json:"status"`
	Secret         string     `json:"secret,omitempty"`
	ClientID       string     `json:"clientId,omitempty"`
	ClientSecret   string     `json:"clientSecret,omitempty"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
}

// Revoked reports whether the key has been revoked. Backend models this as
// `status = REVOKED` (string enum), not as a boolean column — this helper
// keeps loadgen callers source-compatible with the previous bool field.
func (k apiKeyResponse) Revoked() bool { return k.Status == "REVOKED" }

// provisionAPIKey creates one credential per subscription. After Aforo
// revokes the key (via subscription cancel), GET /api-keys/{id} reflects
// status=REVOKED and we record that on the manifest.
//
// customerId is required by pricing-service and must match the
// subscription's customer; the seeder passes it from the same Customer the
// subscription was created against.
//
// Idempotency strategy (drift-fix 2026-05-27):
//   - Within 24h: Idempotency-Key header on POST.
//   - Cross-day / DB-reset: lookupAPIKeyByAccessor runs first using the
//     server-side `?customerId=&accessorId=` filters (verified
//     ApiKeyController.list accepts both). loadgen's accessorId is
//     deterministic per subscription so the lookup is exact.
//
// Parameters:
//   - seedKey: loadgen-internal opaque deterministic string sent as the
//     HTTP Idempotency-Key header. See CONVENTIONS.md.
func provisionAPIKey(ctx context.Context, c *Client, tenantID, seedKey, customerID, subscriptionID string, pt scenario.ProductType) (apiKeyResponse, error) {
	accessorType := accessorTypeForProductType(pt)
	// pricing-service stamps accessorId on the key without verifying the App
	// or Agent row exists in customer-service, so a synthetic id is safe.
	// Per-subscription scope keeps it stable across loadgen re-runs.
	accessorPrefix := "loadgen-app-"
	if accessorType == "AGENT" {
		accessorPrefix = "loadgen-agent-"
	}
	accessorID := accessorPrefix + subscriptionID

	if existing, ok, err := lookupAPIKeyByAccessor(ctx, c, tenantID, customerID, accessorID); err != nil {
		return apiKeyResponse{}, fmt.Errorf("lookup api-key for customer=%q accessor=%q: %w", customerID, accessorID, err)
	} else if ok {
		return existing, nil
	}

	body := apiKeyCreateRequest{
		AccessorID:      accessorID,
		AccessorType:    accessorType,
		CustomerID:      customerID,
		SubscriptionIDs: []string{subscriptionID},
		Name:            fmt.Sprintf("Loadgen %s key for sub %s", pt, subscriptionID),
		Environment:     "live",
		Scopes:          "read",
	}
	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathAPIKeys)
	if err != nil {
		return apiKeyResponse{}, err
	}
	var resp apiKeyResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: seedKey,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupAPIKeyByAccessor(ctx, c, tenantID, customerID, accessorID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return apiKeyResponse{}, fmt.Errorf("create api-key (seedKey=%q): %w", seedKey, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-key-" + seedKey
		resp.CustomerID = customerID
		resp.AccessorID = accessorID
		resp.AccessorType = accessorType
		resp.Status = "ACTIVE"
		ct := credentialTypeForProductType(pt)
		resp.CredentialType = ct
		switch ct {
		case "CLIENT_CREDENTIALS":
			resp.ClientID = "dryrun-client-id-" + seedKey
			resp.ClientSecret = "dryrun-client-secret-" + seedKey
		default:
			resp.Secret = "dryrun-sk_live_" + seedKey
		}
	}
	return resp, nil
}

// fetchAPIKey returns the current state of an API key. Used after a
// subscription cancel to verify status=REVOKED.
func fetchAPIKey(ctx context.Context, c *Client, tenantID, keyID string) (apiKeyResponse, error) {
	getURL, err := c.Target().Path(aforo.ServicePricing, fmt.Sprintf(aforo.PathAPIKeyByID, keyID))
	if err != nil {
		return apiKeyResponse{}, err
	}
	var resp apiKeyResponse
	if err := c.Do(ctx, http.MethodGet, getURL, nil, &resp, RequestOptions{TenantID: tenantID}); err != nil {
		return apiKeyResponse{}, err
	}
	if c.DryRun() {
		// In dry-run, fabricate a revoked snapshot — keeps the stale-keys
		// post-verification path testable end-to-end.
		now := time.Now().UTC()
		resp.ID = keyID
		resp.Status = "REVOKED"
		resp.RevokedAt = &now
	}
	return resp, nil
}

// lookupAPIKeyByAccessor queries pricing-service's GET /api/v1/api-keys
// with server-side filters `?customerId=&accessorId=` (both verified
// supported by ApiKeyController.list) and matches client-side. The
// (customerId, accessorId) pair is deterministic per subscription in
// loadgen's seed scenarios, so at most one row matches.
//
// Returns the FIRST match regardless of status (2026-07-22, Issue 4). The
// earlier implementation filtered out `REVOKED` keys because that maps
// most naturally to "give me a usable secret." But the 409 recovery path
// depends on this helper finding whatever row the backend UNIQUE index
// (customer_id, accessor_id) tripped over — and that row is often the
// previous run's revoked key. Filtering it out surfaced the 409 as a
// "create api-key" error even though the row genuinely exists. The
// seeder's downstream code treats the REVOKED status as informational
// (transitionSubscription is a no-op for CANCELLED/EXPIRED slots that
// most often carry revoked keys) and the manifest still records the
// state accurately via apiKeyResponse.Revoked().
//
// If a caller genuinely needs an ACTIVE-only lookup, they should check
// resp.Status themselves — that keeps the intent explicit and doesn't
// hide previously-created rows from the 409 recovery path.
func lookupAPIKeyByAccessor(ctx context.Context, c *Client, tenantID, customerID, accessorID string) (apiKeyResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathAPIKeys)
	if err != nil {
		return apiKeyResponse{}, false, err
	}
	var keys []apiKeyResponse
	if _, err := listAllOptional(ctx, c, listURL, RequestOptions{
		TenantID: tenantID,
		Query: map[string][]string{
			"customerId": {customerID},
			"accessorId": {accessorID},
		},
	}, &keys); err != nil {
		return apiKeyResponse{}, false, err
	}
	// Prefer an ACTIVE key when one exists; fall back to whatever match we
	// have (REVOKED / SUSPENDED / PENDING_REVOCATION / ...). This keeps
	// happy-path callers on a usable secret while still returning something
	// for the 409 recovery path when only a revoked row remains.
	var fallback apiKeyResponse
	haveFallback := false
	for _, k := range keys {
		if k.CustomerID != customerID || k.AccessorID != accessorID {
			continue
		}
		if k.Status == "ACTIVE" {
			return k, true, nil
		}
		if !haveFallback {
			fallback = k
			haveFallback = true
		}
	}
	if haveFallback {
		return fallback, true, nil
	}
	return apiKeyResponse{}, false, nil
}

// toManifestAPIKey is the conversion from the wire DTO to the manifest entry.
// Centralized so the secret-vs-client-secret branch is in one place.
func toManifestAPIKey(k apiKeyResponse) ManifestAPIKey {
	out := ManifestAPIKey{
		KeyID:          k.ID,
		CredentialType: k.CredentialType,
		Revoked:        k.Revoked(),
		RevokedAt:      k.RevokedAt,
	}
	switch k.CredentialType {
	case "CLIENT_CREDENTIALS":
		out.ClientID = k.ClientID
		out.Secret = k.ClientSecret
	default:
		out.Secret = k.Secret
	}
	return out
}
