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

// MetricTemplateStub is the exported minimal shape of a dry-run template
// entry — Name, EventField, AggregationType. Downstream tests
// (internal/generator's TestDryRunStubMatchesRuntimeTemplates) consume
// this to cross-check the two hardcoded descriptor copies.
type MetricTemplateStub struct {
	Name            string
	EventField      string
	AggregationType string
}

// DryRunTemplatesForProductType returns the per-product-type template set
// used by fetchMetricTemplates in DryRun mode. The list mirrors the
// descriptor's metrics[].name/key/aggregation fields — see
// aforo-nextgen-common/src/main/resources/descriptors/*.json.
//
// Exported so the generator package's parity test
// (TestDryRunStubMatchesRuntimeTemplates) can cross-check that every
// EventField declared here is emitted by the runtime template — the two
// hardcoded descriptor copies would silently drift otherwise. The parity
// test complements TestTemplatesEmitEveryDescriptorKey which locks the
// runtime side against the descriptor key list.
func DryRunTemplatesForProductType(pt scenario.ProductType) []MetricTemplateStub {
	tmpl := dryRunTemplatesInternal(pt)
	out := make([]MetricTemplateStub, 0, len(tmpl))
	for _, t := range tmpl {
		out = append(out, MetricTemplateStub{
			Name:            t.Name,
			EventField:      t.EventField,
			AggregationType: t.AggregationType,
		})
	}
	return out
}

// dryRunTemplatesInternal is the package-private source of truth. The
// exported DryRunTemplatesForProductType is a thin projection over it, and
// the fetchMetricTemplates DryRun path calls it directly to preserve the
// richer metricTemplateResponse shape (UnitLabel / Description / Required).
func dryRunTemplatesInternal(pt scenario.ProductType) []metricTemplateResponse {
	switch pt {
	case scenario.ProductAPI:
		return []metricTemplateResponse{
			{Name: "API Calls", EventField: "request_count", AggregationType: "COUNT"},
			{Name: "Data Transfer", EventField: "response_bytes", AggregationType: "SUM"},
			{Name: "Active Users", EventField: "user_id", AggregationType: "COUNT_DISTINCT"},
			{Name: "Compute Time", EventField: "latency_ms", AggregationType: "SUM"},
			{Name: "Error Requests", EventField: "error_count", AggregationType: "COUNT"},
		}
	case scenario.ProductAIAgent:
		return []metricTemplateResponse{
			{Name: "Agent Sessions", EventField: "session_count", AggregationType: "COUNT"},
			{Name: "Agent Steps", EventField: "step_count", AggregationType: "SUM"},
			{Name: "Tokens Consumed", EventField: "total_tokens", AggregationType: "SUM"},
			{Name: "Agent Tool Calls", EventField: "tool_call_count", AggregationType: "COUNT"},
			{Name: "Execution Minutes", EventField: "execution_minutes", AggregationType: "SUM"},
			{Name: "GPU Hours", EventField: "gpu_hours", AggregationType: "SUM"},
			{Name: "Knowledge Queries", EventField: "kb_query_count", AggregationType: "COUNT"},
			{Name: "Tasks Completed", EventField: "task_completed_count", AggregationType: "COUNT"},
			{Name: "Active Agents", EventField: "concurrent_agents", AggregationType: "MAX"},
		}
	case scenario.ProductMCPServer:
		return []metricTemplateResponse{
			{Name: "MCP Tool Calls", EventField: "tool_call_count", AggregationType: "COUNT"},
			{Name: "MCP Session Duration", EventField: "duration_minutes", AggregationType: "SUM"},
			{Name: "MCP Active Sessions", EventField: "concurrent_sessions", AggregationType: "MAX"},
			{Name: "MCP Connected Agents", EventField: "agent_id", AggregationType: "COUNT_DISTINCT"},
			{Name: "MCP Input Tokens", EventField: "input_tokens", AggregationType: "SUM"},
			{Name: "MCP Output Tokens", EventField: "output_tokens", AggregationType: "SUM"},
			{Name: "MCP Errors", EventField: "error_count", AggregationType: "COUNT"},
			{Name: "MCP P95 Latency", EventField: "response_time_ms", AggregationType: "PERCENTILE_95"},
		}
	case scenario.ProductAgenticAPI:
		return []metricTemplateResponse{
			{Name: "Agentic Requests", EventField: "request_count", AggregationType: "COUNT"},
			{Name: "Agentic Steps", EventField: "agent_step_count", AggregationType: "SUM"},
			{Name: "Agentic Tool Calls", EventField: "tool_call_count", AggregationType: "COUNT"},
			{Name: "Agentic Input Tokens", EventField: "input_tokens", AggregationType: "SUM"},
			{Name: "Agentic Output Tokens", EventField: "output_tokens", AggregationType: "SUM"},
			{Name: "Agentic Compute Time", EventField: "latency_ms", AggregationType: "SUM"},
			{Name: "Agentic Data Transfer", EventField: "response_bytes", AggregationType: "SUM"},
			{Name: "Agentic Active Users", EventField: "user_id", AggregationType: "COUNT_DISTINCT"},
		}
	default:
		return []metricTemplateResponse{{Name: fmt.Sprintf("dryrun-template-%s", pt)}}
	}
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
// EventField + AggregationType are optional wire fields — catalog's
// MetricResponse DTO carries them but historical loadgen didn't need
// them. They're now read to let the runtime generator stamp
// Envelope.Quantity from the descriptor's key field for SUM / MAX /
// PERCENTILE_95 aggregations (see generator.produce). When the backend
// doesn't send them (older builds), we cross-reference the template
// response by name inside provisionMetricsForProduct.
//
// Drift-fix (rename pass — see CONVENTIONS.md): the previous loadgen
// response struct read `json:"externalId"` which catalog-service has
// never returned. Dropped per the no-phantom-fields convention.
type metricResponse struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	EventField      string `json:"eventField,omitempty"`
	AggregationType string `json:"aggregationType,omitempty"`
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
		// `{}` body can't be unmarshalled into a slice.
		if err := c.Do(ctx, http.MethodPost, bulkURL, body, nil, RequestOptions{
			TenantID:    tenantID,
			Idempotency: externalIDPrefix,
		}); err != nil {
			return nil, fmt.Errorf("bulk-seed metrics (dry-run) for product %s: %w", productID, err)
		}
		// Synthesize ONE metricResponse per template so the manifest — and
		// any downstream coverage-check tool inspecting it — sees the full
		// descriptor-driven billable-unit set even in dry-run mode. Names,
		// EventField, and AggregationType come straight from the template
		// list so the manifest matches a real live-backend seed.
		stubs := make([]metricResponse, 0, len(templates))
		for i, t := range templates {
			if t.Name == "" {
				continue
			}
			stubs = append(stubs, metricResponse{
				ID:              fmt.Sprintf("dryrun-metric-%s-%02d", externalIDPrefix, i),
				Name:            t.Name,
				EventField:      t.EventField,
				AggregationType: t.AggregationType,
			})
		}
		return stubs, nil
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
	// Cross-reference the templates response by name so every created row
	// carries EventField + AggregationType even when the bulk endpoint
	// omits them (older catalog builds return only id/name). Downstream
	// generator uses these to stamp Envelope.Quantity from the descriptor's
	// key field for SUM / MAX / PERCENTILE_95 metrics.
	byName := make(map[string]metricTemplateResponse, len(templates))
	for _, t := range templates {
		if t.Name != "" {
			byName[t.Name] = t
		}
	}
	for i := range created {
		if created[i].EventField != "" && created[i].AggregationType != "" {
			continue
		}
		if tpl, ok := byName[created[i].Name]; ok {
			if created[i].EventField == "" {
				created[i].EventField = tpl.EventField
			}
			if created[i].AggregationType == "" {
				created[i].AggregationType = tpl.AggregationType
			}
		}
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
	// Return the hardcoded per-product-type template set so the manifest
	// reflects the real descriptor-driven coverage even in dry-run mode.
	// Kept in sync with aforo-nextgen-common/src/main/resources/descriptors/
	// (locked by TestTemplatesEmitEveryDescriptorKey which asserts the same
	// key set the templates list here declares). If descriptors evolve, the
	// live-backend path still fetches the current authoritative list — the
	// dry-run stub only affects manifests generated without a backend.
	if c.DryRun() {
		return dryRunTemplatesInternal(pt), nil
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
