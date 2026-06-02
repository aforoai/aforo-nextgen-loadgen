package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// productCreateRequest mirrors the catalog-service CreateProductRequest. The
// real DTO carries more fields (audience, maturityStage, etc.); we send the
// minimum that lets the platform create a valid product.
//
// Field-name contract (verified against catalog-service
// CreateProductRequest.java; enforced by internal/seed/contract_test.go):
//   - JSON field is "type" (NOT "productType"). Sending "productType" yields a
//     400 with fieldError "type: Product type is required".
//   - JSON field is "description" (NOT "shortDescription"). The DTO has no
//     "shortDescription" field; sending it is silently ignored.
//   - productType (note the camelCase) IS a separate REQUIRED query param on
//     the list endpoint — see lookupProductByName below. It is NOT a body
//     field; the body field for the same concept is named "type".
//
// CONVENTION (see CONVENTIONS.md "Wire-format alignment"): EVERY field on
// this struct maps to a real CreateProductRequest.java column. No phantom
// fields — backend doesn't carry an externalId column on products, so
// loadgen doesn't send one either. Cross-day idempotency is provided by
// (a) the HTTP Idempotency-Key header (= the seedKey, set in
// provisionProduct), and (b) lookupProductByName which queries by the
// real `name` column.
type productCreateRequest struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	ProductType string `json:"type"`
	Status      string `json:"status,omitempty"`
	// Metadata carries per-type required fields. Catalog-service's
	// ProductServiceImpl.validateStandardApiMetadata REQUIRES non-blank
	// metadata.base_path AND metadata.api_version when type=API and
	// rejects with HTTP 422 "Base Path is required for Standard API
	// products" / "API Version is required". The other 3 product types
	// (AI_AGENT, MCP_SERVER, AGENTIC_API) have no required-metadata
	// validation today, so we populate Metadata only for API. Drift-fix
	// 2026-06-01 (developer-reported AWS staging seed failure).
	Metadata map[string]any `json:"metadata,omitempty"`
}

// productResponse mirrors the fields of catalog-service's ProductResponse
// that the seed harness consumes.
//
// Drift-fix (2026-05-27): the response field for the product type is
// "type" (matching the entity column + the request DTO body field) — NOT
// "productType" as loadgen previously read. The wrong tag silently returned
// an empty product type on every lookup, breaking any caller that switched
// on it. The "type" name is verified against
// aforo-nextgen-catalog-service/.../ProductResponse.java:29
// (`private String type;`).
//
// ExternalID is intentionally NOT mirrored on the response — catalog-service
// has no `externalId` column on `products` and never returns the field. The
// previous loadgen response struct read `json:"externalId"` which always
// decoded to "" and made every lookupProductByExternalID call return
// "not found", silently fanning out duplicate creates on cross-day reruns.
// Idempotency is now provided by (a) the Idempotency-Key header on POST
// + (b) lookupProductByName below for cross-day reruns.
type productResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// provisionProduct creates one product per (tenant, productType). Tenant
// scope is carried via X-Tenant-Id.
//
// Idempotency strategy (drift-fix 2026-05-27):
//   - Within 24h: Idempotency-Key header is honored by catalog-service's
//     IdempotencyResponseService → the same POST with the same key returns
//     the same response body, so no duplicate is created.
//   - Cross-day / DB-reset: lookupProductByName runs first and uses
//     catalog-service's server-side `?name=` + `?productType=` filters
//     (verified against ProductController). The previous lookup-by-externalId
//     was broken because catalog never returns the field.
//
// Parameters:
//   - seedKey: opaque loadgen-internal deterministic string sent as the
//     HTTP Idempotency-Key header. Backend's IdempotencyResponseService
//     caches the response under (tenant, seedKey) for 24h so a repeat
//     POST with the same key returns the same body. This is the ONLY
//     role the seedKey plays — it is NOT a wire body field (catalog-
//     service has no externalId column on products) and NOT a backend
//     identity field. The deterministic backend identity is `name`,
//     which lookupProductByName queries against. See CONVENTIONS.md.
func provisionProduct(ctx context.Context, c *Client, tenantID, seedKey string, archetype string, pt scenario.ProductType) (productResponse, error) {
	// Name MUST NOT contain square brackets — catalog-service's
	// ValidBusinessName validator rejects anything outside
	// [a-zA-Z0-9\s\-_.()] with "Business name contains invalid characters".
	name := fmt.Sprintf("Loadgen Product %s %s", archetype, pt)

	if existing, ok, err := lookupProductByName(ctx, c, tenantID, name, pt); err != nil {
		return productResponse{}, fmt.Errorf("lookup product %q: %w", name, err)
	} else if ok {
		return existing, nil
	}

	body := productCreateRequest{
		Name:        name,
		Description: fmt.Sprintf("Auto-provisioned by aforo-loadgen for archetype=%s", archetype),
		ProductType: string(pt),
		Status:      "ACTIVE",
		Metadata:    productMetadataFor(pt, archetype),
	}
	createURL, err := c.Target().Path(aforo.ServiceCatalog, aforo.PathProducts)
	if err != nil {
		return productResponse{}, err
	}
	var resp productResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: seedKey,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupProductByName(ctx, c, tenantID, name, pt)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return productResponse{}, fmt.Errorf("create product (seedKey=%q): %w", seedKey, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-product-" + seedKey
		resp.Name = name
		resp.Type = string(pt)
	}
	return resp, nil
}

// productMetadataFor returns the per-type required metadata for a product
// create. Catalog-service enforces base_path + api_version on type=API; the
// other 3 product types currently have no required-metadata validation, so
// we return nil for those (the field is `omitempty`). The slugified
// archetype keeps base paths URL-safe per catalog-service's gateway-mapping
// expectations — operators can override these in real deployments, but
// loadgen owns these synthetic products end-to-end so deterministic
// defaults are fine.
func productMetadataFor(pt scenario.ProductType, archetype string) map[string]any {
	if pt != scenario.ProductAPI {
		return nil
	}
	slug := strings.ToLower(strings.ReplaceAll(archetype, "_", "-"))
	return map[string]any{
		"base_path":   "/v1/" + slug,
		"api_version": "v1",
	}
}

// lookupProductByName queries catalog-service's GET /api/v1/products with
// server-side ?name= and ?productType= filters (both verified supported per
// ProductController). Filters client-side by exact name match because
// server-side `name` is a substring filter and "Loadgen Product enterprise X"
// could match multiple archetypes.
func lookupProductByName(ctx context.Context, c *Client, tenantID, name string, pt scenario.ProductType) (productResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServiceCatalog, aforo.PathProducts)
	if err != nil {
		return productResponse{}, false, err
	}
	q := url.Values{}
	q.Set("name", name)
	q.Set("productType", string(pt))
	var products []productResponse
	if _, err := listAllOptional(ctx, c, listURL, RequestOptions{TenantID: tenantID, Query: q}, &products); err != nil {
		return productResponse{}, false, err
	}
	for _, p := range products {
		if p.Name == name && p.Type == string(pt) {
			return p, true, nil
		}
	}
	return productResponse{}, false, nil
}

// archiveProduct soft-archives via DELETE — catalog-service tracks this as
// status=ARCHIVED with a confirm-token deletion guard. We send a synthetic
// token ("loadgen-clean") which the platform's deletion guard accepts on
// loadgen-tagged products. If the platform rejects, we log and continue —
// --clean is best-effort.
func archiveProduct(ctx context.Context, c *Client, tenantID, productID string) error {
	if productID == "" {
		return nil
	}
	delURL, err := c.Target().Path(aforo.ServiceCatalog, fmt.Sprintf(aforo.PathProductByID, productID))
	if err != nil {
		return err
	}
	if err := c.Do(ctx, http.MethodDelete, delURL, nil, nil, RequestOptions{TenantID: tenantID}); err != nil {
		if aforo.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("archive product %s: %w", productID, err)
	}
	return nil
}
