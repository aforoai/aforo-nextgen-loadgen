package seed

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// IngestKeyScope is the organization-service API-key scope that the
// ingest path accepts. organization-service ApiKeyServiceImpl.rolesForScopes
// maps "usage:ingest" (or "usage-events:ingest") to ROLE_METERING_AGENT,
// which is one of the roles every POST /v1/ingest* endpoint allows
// (usage-ingestor UsageEventController @PreAuthorize).
const IngestKeyScope = "usage:ingest"

// ManifestIngestKey is the tenant-level credential the run engine uses to
// POST events. It is NOT the per-subscription key in ManifestAPIKey.
//
// Why two kinds of key: the keys pricing-service issues per subscription are
// the end customer's keys for the provider's own API. usage-ingestor
// authenticates API keys through aforo-common ApiKeyAuthFilter, which
// validates them against organization-service
// /internal/v1/api-keys/validate — a lookup over organization-service's own
// api_keys table only. A pricing-service key is unknown there and is
// answered 401. Ingest traffic must carry an organization-service key with
// the usage:ingest scope.
//
// RawKey is a live secret. It is written to the manifest so `run` can use
// it; treat the manifest file like any other credential file and revoke the
// key with `seed --clean` when the run is over.
type ManifestIngestKey struct {
	KeyID     string   `json:"key_id"`
	KeyPrefix string   `json:"key_prefix,omitempty"`
	RawKey    string   `json:"raw_key"`
	Scopes    []string `json:"scopes,omitempty"`
}

// orgAPIKeyCreateRequest mirrors organization-service CreateApiKeyRequest
// (name @NotBlank; description, scopes, expiresAt, environment optional).
type orgAPIKeyCreateRequest struct {
	Name        string   `json:"name"`
	Description string   `json:"description,omitempty"`
	Scopes      []string `json:"scopes"`
	Environment string   `json:"environment"`
}

// orgAPIKeyResponse mirrors the subset of organization-service
// ApiKeyResponse the seeder reads. rawKey is only present on create.
type orgAPIKeyResponse struct {
	ID        string   `json:"id"`
	TenantID  string   `json:"tenantId"`
	KeyPrefix string   `json:"keyPrefix"`
	Scopes    []string `json:"scopes"`
	RawKey    string   `json:"rawKey"`
}

// errNoRawKey is returned when organization-service answers 2xx without the
// one-time rawKey — the key exists but its secret can never be read again.
var errNoRawKey = errors.New("organization-service returned no rawKey for the new ingest key")

// provisionIngestKey creates one SANDBOX (sk_test_) organization-service
// API key with the usage:ingest scope for the tenant.
//
// Not idempotent across runs by design: organization-service returns the
// secret only on create, so a re-seed always mints a new key. seedKey is
// still sent as Idempotency-Key so a retried POST inside one run does not
// mint two.
func provisionIngestKey(ctx context.Context, c *Client, tenantID, seedKey string) (ManifestIngestKey, error) {
	createURL, err := c.Target().Path(aforo.ServiceOrganization, aforo.PathAPIKeys)
	if err != nil {
		return ManifestIngestKey{}, err
	}
	body := orgAPIKeyCreateRequest{
		Name:        "Loadgen ingest key " + seedKey,
		Description: "Created by aforo-loadgen seed for load testing; revoke with `aforo-loadgen seed --clean`.",
		Scopes:      []string{IngestKeyScope},
		Environment: "SANDBOX",
	}
	var resp orgAPIKeyResponse
	if err := c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: seedKey,
	}); err != nil {
		return ManifestIngestKey{}, fmt.Errorf("create ingest api-key for tenant %s: %w", tenantID, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-ingest-key-" + tenantID
		resp.KeyPrefix = "sk_test_dry"
		resp.RawKey = "sk_test_dryrun_" + seedKey
		resp.Scopes = []string{IngestKeyScope}
	}
	if resp.RawKey == "" {
		return ManifestIngestKey{}, fmt.Errorf("tenant %s key %s: %w", tenantID, resp.ID, errNoRawKey)
	}
	return ManifestIngestKey{
		KeyID:     resp.ID,
		KeyPrefix: resp.KeyPrefix,
		RawKey:    resp.RawKey,
		Scopes:    resp.Scopes,
	}, nil
}

// revokeIngestKey revokes the tenant's ingest key
// (organization-service DELETE /api/v1/api-keys/{id}).
func revokeIngestKey(ctx context.Context, c *Client, tenantID, keyID string) error {
	u, err := c.Target().Path(aforo.ServiceOrganization, fmt.Sprintf(aforo.PathAPIKeyByID, keyID))
	if err != nil {
		return err
	}
	return c.Do(ctx, http.MethodDelete, u, nil, nil, RequestOptions{TenantID: tenantID})
}
