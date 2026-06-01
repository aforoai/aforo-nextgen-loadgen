package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// tenantCreateRequest is the body sent to organization-service's
// POST /api/v1/internal/tenants. Field names mirror the documented internal
// admin contract; production callers also send a tier and a contact e-mail.
//
// EXCEPTION TO THE CONVENTIONS.md "no externalId" rule: tenants are the
// ONE entity where backend (LoadgenTenantResponse on the /internal/admin
// path) genuinely carries an `externalId` column. The field round-trips
// through Springdoc → create returns it → list filter ?externalId= honors
// it. So `externalId` is a real wire field here, not a phantom — keep it.
type tenantCreateRequest struct {
	ExternalID  string            `json:"externalId"`
	Name        string            `json:"name"`
	Tier        string            `json:"tier,omitempty"`
	Description string            `json:"description,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
}

// tenantResponse is the normalized response we extract. Real org-service
// responses include more fields; we only persist the IDs we need downstream.
type tenantResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
	Name       string `json:"name"`
}

// provisionTenant creates (or returns the existing) tenant with the given
// external ID. Idempotent: a GET by externalId precedes the POST.
func provisionTenant(ctx context.Context, c *Client, externalID, name, archetype string) (tenantResponse, error) {
	// Idempotent GET via the list endpoint with an ?externalId= filter.
	if existing, ok, err := lookupTenantByExternalID(ctx, c, externalID); err != nil {
		return tenantResponse{}, fmt.Errorf("lookup tenant %q: %w", externalID, err)
	} else if ok {
		return existing, nil
	}

	body := tenantCreateRequest{
		ExternalID:  externalID,
		Name:        name,
		Tier:        "starter",
		Description: fmt.Sprintf("Loadgen seed tenant (archetype=%s)", archetype),
		Metadata: map[string]string{
			"loadgen":   "true",
			"archetype": archetype,
		},
	}
	createURL, err := c.Target().Path(aforo.ServiceOrganization, aforo.PathInternalTenants)
	if err != nil {
		return tenantResponse{}, err
	}
	var resp tenantResponse
	if err := c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		Idempotency: externalID,
	}); err != nil {
		// 409 Conflict → fetch existing
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupTenantByExternalID(ctx, c, externalID)
			if lookupErr != nil {
				return tenantResponse{}, fmt.Errorf("create returned 409 and lookup failed: %w", lookupErr)
			}
			if ok {
				return existing, nil
			}
		}
		return tenantResponse{}, fmt.Errorf("create tenant %q: %w", externalID, err)
	}
	if c.DryRun() {
		// Synthesize a stable ID so downstream provisioners have something to
		// reference. Tests assert against the request, not the synthesized ID.
		resp.ID = "dryrun-tenant-" + externalID
		resp.ExternalID = externalID
		resp.Name = name
	}
	return resp, nil
}

// lookupTenantByExternalID returns the tenant if one exists, ok=false if not.
// 404 is treated as "not found" rather than an error.
func lookupTenantByExternalID(ctx context.Context, c *Client, externalID string) (tenantResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServiceOrganization, aforo.PathInternalTenants)
	if err != nil {
		return tenantResponse{}, false, err
	}
	q := url.Values{}
	q.Set("externalId", externalID)
	// Backend (LoadgenInternalTenantController.list) returns
	// LoadgenTenantListResponse, shape `{"data":[...]}`. The unmarshal layer
	// in client.go strips the standard ApiResponse envelope ({success,data,
	// meta}) when present but does NOT strip this list-only envelope, so we
	// keep the {Data:[]} wrapper struct here. If the controller ever moves
	// behind ResponseEntity advice (gaining the success/meta keys), the
	// envelope stripper will surface `[...]` directly and this struct will
	// fail to decode — at which point fall back to plain `[]tenantResponse`.
	var page struct {
		Data []tenantResponse `json:"data"`
	}
	err = c.Do(ctx, http.MethodGet, listURL, nil, &page, RequestOptions{Query: q})
	if err != nil {
		if aforo.IsNotFound(err) {
			return tenantResponse{}, false, nil
		}
		return tenantResponse{}, false, err
	}
	for _, t := range page.Data {
		if t.ExternalID == externalID {
			return t, true, nil
		}
	}
	return tenantResponse{}, false, nil
}

// archiveTenant is the --clean entry point for a single tenant. We
// soft-archive via DELETE /api/v1/internal/tenants/{id}; org-service treats
// this as setting status=ARCHIVED rather than a hard delete.
func archiveTenant(ctx context.Context, c *Client, tenantID string) error {
	if tenantID == "" {
		return nil
	}
	delURL, err := c.Target().Path(aforo.ServiceOrganization, fmt.Sprintf(aforo.PathInternalTenant, tenantID))
	if err != nil {
		return err
	}
	if err := c.Do(ctx, http.MethodDelete, delURL, nil, nil, RequestOptions{}); err != nil {
		// Already archived → 404. Treat as success.
		if aforo.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("archive tenant %s: %w", tenantID, err)
	}
	return nil
}
