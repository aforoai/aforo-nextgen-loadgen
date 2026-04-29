package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// bulkSeedRequest asks catalog-service to instantiate the standard template
// set for a product type. Far cheaper than enumerating each metric ourselves
// and keeps the loadgen tool aligned with the platform's evolving template
// registry (add a metric → no loadgen change required).
type bulkSeedRequest struct {
	ProductID        string `json:"productId"`
	ProductType      string `json:"productType"`
	ExternalIDPrefix string `json:"externalIdPrefix"`
}

type metricResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
	Name       string `json:"name"`
}

type bulkSeedResponse struct {
	Created []metricResponse `json:"created"`
}

// provisionMetricsForProduct calls /api/v1/metrics/bulk to instantiate the
// product-type-specific billable units (e.g. API → API Calls, Bandwidth GB;
// MCP_SERVER → Tool Invocations, Session Duration; etc.). Returns the IDs
// to wire into the rate plan.
//
// Idempotency is naturally handled by the externalIdPrefix — re-running
// with the same prefix asks the bulk endpoint to skip already-created units.
func provisionMetricsForProduct(ctx context.Context, c *Client, tenantID, productID string, pt scenario.ProductType, externalIDPrefix string) ([]metricResponse, error) {
	body := bulkSeedRequest{
		ProductID:        productID,
		ProductType:      string(pt),
		ExternalIDPrefix: externalIDPrefix,
	}
	bulkURL, err := c.Target().Path(aforo.ServiceCatalog, aforo.PathMetricsBulk)
	if err != nil {
		return nil, err
	}
	var resp bulkSeedResponse
	if err := c.Do(ctx, http.MethodPost, bulkURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: externalIDPrefix,
	}); err != nil {
		return nil, fmt.Errorf("bulk-seed metrics for product %s: %w", productID, err)
	}
	if c.DryRun() {
		// Fabricate one stub metric so the rate plan can reference it.
		return []metricResponse{{
			ID:         "dryrun-metric-" + externalIDPrefix,
			ExternalID: externalIDPrefix + "-1",
			Name:       fmt.Sprintf("dryrun %s metric", pt),
		}}, nil
	}
	return resp.Created, nil
}
