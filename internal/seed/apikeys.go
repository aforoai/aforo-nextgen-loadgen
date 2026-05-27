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
// The previous body shape ({externalId, subscriptionId, credentialType,
// description}) was rejected by the server with 400 because none of those
// fields are in the DTO — accessorId, accessorType, customerId, and
// subscriptionIds were all missing. ExternalID is silently dropped today.
type apiKeyCreateRequest struct {
	ExternalID      string   `json:"externalId,omitempty"`
	AccessorID      string   `json:"accessorId"`
	AccessorType    string   `json:"accessorType"`
	CustomerID      string   `json:"customerId"`
	SubscriptionIDs []string `json:"subscriptionIds"`
	Name            string   `json:"name,omitempty"`
	Environment     string   `json:"environment,omitempty"`
	Scopes          string   `json:"scopes,omitempty"`
}

type apiKeyResponse struct {
	ID             string     `json:"id"`
	ExternalID     string     `json:"externalId"`
	CredentialType string     `json:"credentialType"`
	Secret         string     `json:"secret,omitempty"`
	ClientID       string     `json:"clientId,omitempty"`
	ClientSecret   string     `json:"clientSecret,omitempty"`
	Revoked        bool       `json:"revoked"`
	RevokedAt      *time.Time `json:"revokedAt,omitempty"`
}

// provisionAPIKey creates one credential per subscription. After Aforo
// revokes the key (via subscription cancel), GET /api-keys/{id} reflects
// revoked=true and we record that on the manifest.
//
// customerId is required by pricing-service and must match the
// subscription's customer; the seeder passes it from the same Customer the
// subscription was created against.
func provisionAPIKey(ctx context.Context, c *Client, tenantID, externalID, customerID, subscriptionID string, pt scenario.ProductType) (apiKeyResponse, error) {
	if existing, ok, err := lookupAPIKeyByExternalID(ctx, c, tenantID, externalID); err != nil {
		return apiKeyResponse{}, fmt.Errorf("lookup api-key %q: %w", externalID, err)
	} else if ok {
		return existing, nil
	}
	accessorType := accessorTypeForProductType(pt)
	// pricing-service stamps accessorId on the key without verifying the App
	// or Agent row exists in customer-service, so a synthetic id is safe.
	// Per-subscription scope keeps it stable across loadgen re-runs.
	accessorPrefix := "loadgen-app-"
	if accessorType == "AGENT" {
		accessorPrefix = "loadgen-agent-"
	}
	body := apiKeyCreateRequest{
		ExternalID:      externalID,
		AccessorID:      accessorPrefix + subscriptionID,
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
		Idempotency: externalID,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupAPIKeyByExternalID(ctx, c, tenantID, externalID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return apiKeyResponse{}, fmt.Errorf("create api-key %q: %w", externalID, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-key-" + externalID
		resp.ExternalID = externalID
		ct := credentialTypeForProductType(pt)
		resp.CredentialType = ct
		switch ct {
		case "CLIENT_CREDENTIALS":
			resp.ClientID = "dryrun-client-id-" + externalID
			resp.ClientSecret = "dryrun-client-secret-" + externalID
		default:
			resp.Secret = "dryrun-sk_live_" + externalID
		}
	}
	return resp, nil
}

// fetchAPIKey returns the current state of an API key. Used after a
// subscription cancel to verify revoked=true.
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
		resp.Revoked = true
		resp.RevokedAt = &now
	}
	return resp, nil
}

func lookupAPIKeyByExternalID(ctx context.Context, c *Client, tenantID, externalID string) (apiKeyResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathAPIKeys)
	if err != nil {
		return apiKeyResponse{}, false, err
	}
	var page struct {
		Data []apiKeyResponse `json:"data"`
	}
	err = c.Do(ctx, http.MethodGet, listURL, nil, &page, RequestOptions{
		TenantID: tenantID,
		Query:    map[string][]string{"externalId": {externalID}},
	})
	if err != nil {
		if aforo.IsNotFound(err) {
			return apiKeyResponse{}, false, nil
		}
		return apiKeyResponse{}, false, err
	}
	for _, k := range page.Data {
		if k.ExternalID == externalID {
			return k, true, nil
		}
	}
	return apiKeyResponse{}, false, nil
}

// toManifestAPIKey is the conversion from the wire DTO to the manifest entry.
// Centralized so the secret-vs-client-secret branch is in one place.
func toManifestAPIKey(k apiKeyResponse) ManifestAPIKey {
	out := ManifestAPIKey{
		KeyID:          k.ID,
		CredentialType: k.CredentialType,
		Revoked:        k.Revoked,
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
