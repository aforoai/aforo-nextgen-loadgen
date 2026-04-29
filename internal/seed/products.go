package seed

import (
	"context"
	"fmt"
	"net/http"
	"net/url"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// productCreateRequest mirrors the catalog-service product create body. The
// real DTO carries more fields (audience, maturityStage, etc.); we send the
// minimum that lets the platform create a valid product.
type productCreateRequest struct {
	ExternalID  string `json:"externalId"`
	Name        string `json:"name"`
	ShortDesc   string `json:"shortDescription,omitempty"`
	ProductType string `json:"productType"`
	Status      string `json:"status,omitempty"`
}

type productResponse struct {
	ID          string `json:"id"`
	ExternalID  string `json:"externalId"`
	ProductType string `json:"productType"`
}

// provisionProduct creates one product per (tenant, productType). Tenant
// scope is carried via X-Tenant-Id.
func provisionProduct(ctx context.Context, c *Client, tenantID, externalID string, archetype string, pt scenario.ProductType) (productResponse, error) {
	if existing, ok, err := lookupProductByExternalID(ctx, c, tenantID, externalID); err != nil {
		return productResponse{}, fmt.Errorf("lookup product %q: %w", externalID, err)
	} else if ok {
		return existing, nil
	}

	body := productCreateRequest{
		ExternalID:  externalID,
		Name:        fmt.Sprintf("Loadgen Product [%s] %s", archetype, pt),
		ShortDesc:   fmt.Sprintf("Auto-provisioned by aforo-loadgen for archetype=%s", archetype),
		ProductType: string(pt),
		Status:      "ACTIVE",
	}
	createURL, err := c.Target().Path(aforo.ServiceCatalog, aforo.PathProducts)
	if err != nil {
		return productResponse{}, err
	}
	var resp productResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: externalID,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupProductByExternalID(ctx, c, tenantID, externalID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return productResponse{}, fmt.Errorf("create product %q: %w", externalID, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-product-" + externalID
		resp.ExternalID = externalID
		resp.ProductType = string(pt)
	}
	return resp, nil
}

func lookupProductByExternalID(ctx context.Context, c *Client, tenantID, externalID string) (productResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServiceCatalog, aforo.PathProducts)
	if err != nil {
		return productResponse{}, false, err
	}
	q := url.Values{}
	q.Set("externalId", externalID)
	var page struct {
		Data []productResponse `json:"data"`
	}
	err = c.Do(ctx, http.MethodGet, listURL, nil, &page, RequestOptions{TenantID: tenantID, Query: q})
	if err != nil {
		if aforo.IsNotFound(err) {
			return productResponse{}, false, nil
		}
		return productResponse{}, false, err
	}
	for _, p := range page.Data {
		if p.ExternalID == externalID {
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
