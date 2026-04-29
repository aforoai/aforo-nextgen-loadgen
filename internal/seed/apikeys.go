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
func credentialTypeForProductType(pt scenario.ProductType) string {
	switch pt {
	case scenario.ProductAIAgent, scenario.ProductMCPServer:
		return "CLIENT_CREDENTIALS"
	default:
		return "BEARER_TOKEN"
	}
}

type apiKeyCreateRequest struct {
	ExternalID     string `json:"externalId"`
	SubscriptionID string `json:"subscriptionId"`
	CredentialType string `json:"credentialType"`
	Description    string `json:"description,omitempty"`
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
func provisionAPIKey(ctx context.Context, c *Client, tenantID, externalID, subscriptionID string, pt scenario.ProductType) (apiKeyResponse, error) {
	if existing, ok, err := lookupAPIKeyByExternalID(ctx, c, tenantID, externalID); err != nil {
		return apiKeyResponse{}, fmt.Errorf("lookup api-key %q: %w", externalID, err)
	} else if ok {
		return existing, nil
	}
	body := apiKeyCreateRequest{
		ExternalID:     externalID,
		SubscriptionID: subscriptionID,
		CredentialType: credentialTypeForProductType(pt),
		Description:    fmt.Sprintf("Loadgen %s key for sub %s", pt, subscriptionID),
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
		resp.CredentialType = body.CredentialType
		switch body.CredentialType {
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
