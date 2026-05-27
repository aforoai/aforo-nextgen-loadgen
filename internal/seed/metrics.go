package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// metricTemplateResponse mirrors catalog-service's MetricTemplateResponse.
// We use the `name` field to drive the bulk create.
type metricTemplateResponse struct {
	Name            string `json:"name"`
	UnitLabel       string `json:"unitLabel"`
	AggregationType string `json:"aggregationType"`
	EventField      string `json:"eventField"`
	Description     string `json:"description"`
	Required        bool   `json:"required"`
}

// bulkSeedRequest mirrors catalog-service's BulkCreateMetricRequest. Only
// productType + templateNames are read; older Loadgen fields (productId,
// externalIdPrefix) were silently dropped by the server and made the request
// fail with "At least one metric template name is required".
//
// Field-name contract (verified against
// catalog-service BulkCreateMetricRequest.java):
//   - productType — @NotBlank, one of API|AI_AGENT|AGENTIC_API|MCP_SERVER.
//   - templateNames — @NotEmpty list of template display names. We discover
//     these via GET /api/v1/metrics/templates/{productType} so the platform
//     can add new templates without a loadgen change.
type bulkSeedRequest struct {
	ProductType   string   `json:"productType"`
	TemplateNames []string `json:"templateNames"`
}

// metricResponse mirrors catalog-service's MetricResponse subset the
// bulk endpoint returns.
//
// Drift-fix (rename pass — see CONVENTIONS.md): the previous loadgen
// response struct read `json:"externalId"` which catalog-service has
// never returned. Dropped per the no-phantom-fields convention.
type metricResponse struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// provisionMetricsForProduct calls /api/v1/metrics/bulk to instantiate the
// product-type-specific billable units (e.g. API → API Calls, Bandwidth GB;
// MCP_SERVER → Tool Invocations, Session Duration; etc.). Returns the IDs
// to wire into the rate plan.
//
// The bulk endpoint is idempotent on (tenant, template name) — re-running
// returns the existing metric without creating a duplicate. The platform's
// own MetricServiceImpl.bulkCreateFromTemplates checks for an existing row
// by name + event field before insert, so calling repeatedly is safe.
//
// productID is NOT a bulk-endpoint parameter — catalog metrics are scoped to
// the tenant + product type, not to a specific product. The product↔metric
// association is wired separately via the rate-plan's productIds[] field.
// We keep productID in the signature so the seeder can log it.
func provisionMetricsForProduct(ctx context.Context, c *Client, tenantID, productID string, pt scenario.ProductType, externalIDPrefix string) ([]metricResponse, error) {
	templates, err := fetchMetricTemplates(ctx, c, tenantID, pt)
	if err != nil {
		return nil, fmt.Errorf("fetch metric templates for productType=%s: %w", pt, err)
	}
	if len(templates) == 0 {
		return nil, fmt.Errorf("no metric templates returned for productType=%s — descriptor missing or empty", pt)
	}

	names := make([]string, 0, len(templates))
	for _, t := range templates {
		if t.Name != "" {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("templates returned for productType=%s carried empty names", pt)
	}

	body := bulkSeedRequest{
		ProductType:   string(pt),
		TemplateNames: names,
	}
	bulkURL, err := c.Target().Path(aforo.ServiceCatalog, aforo.PathMetricsBulk)
	if err != nil {
		return nil, err
	}

	if c.DryRun() {
		// Record the POST shape for assertions but skip decode — the canned
		// `{}` body can't be unmarshalled into a slice and the rate plan
		// just needs a stub metric reference to render.
		if err := c.Do(ctx, http.MethodPost, bulkURL, body, nil, RequestOptions{
			TenantID:    tenantID,
			Idempotency: externalIDPrefix,
		}); err != nil {
			return nil, fmt.Errorf("bulk-seed metrics (dry-run) for product %s: %w", productID, err)
		}
		return []metricResponse{{
			ID:   "dryrun-metric-" + externalIDPrefix,
			Name: fmt.Sprintf("dryrun %s metric", pt),
		}}, nil
	}

	// Catalog returns List<MetricResponse> at the top level (not wrapped in
	// {created: [...]}). Decode directly into the slice.
	var created []metricResponse
	if err := c.Do(ctx, http.MethodPost, bulkURL, body, &created, RequestOptions{
		TenantID:    tenantID,
		Idempotency: externalIDPrefix,
	}); err != nil {
		return nil, fmt.Errorf("bulk-seed metrics for product %s: %w", productID, err)
	}
	return created, nil
}

// fetchMetricTemplates hits GET /api/v1/metrics/templates/{productType} and
// returns the platform-managed list of template metadata. The display name
// is the field the bulk endpoint matches on; eventField/unitLabel are
// returned for diagnostics only.
func fetchMetricTemplates(ctx context.Context, c *Client, tenantID string, pt scenario.ProductType) ([]metricTemplateResponse, error) {
	// Dry-run short-circuit: the test transport's canned {} response can't
	// be decoded into a slice (Go errors on json.Unmarshal `{}` → `[]T`).
	// We synthesize one template so the downstream bulk request still gets
	// a non-empty templateNames payload to exercise.
	if c.DryRun() {
		return []metricTemplateResponse{{Name: fmt.Sprintf("dryrun-template-%s", pt)}}, nil
	}
	url, err := c.Target().Path(aforo.ServiceCatalog, fmt.Sprintf(aforo.PathMetricsTemplate, string(pt)))
	if err != nil {
		return nil, err
	}
	var out []metricTemplateResponse
	if err := c.Do(ctx, http.MethodGet, url, nil, &out, RequestOptions{TenantID: tenantID}); err != nil {
		if aforo.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return out, nil
}
