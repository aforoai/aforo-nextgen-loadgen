package generator

import (
	"math/rand"
	"strconv"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// EventTemplate produces the per-product-type event payload.
//
// The shape mirrors Aforo's MetricTemplateRegistry per CLAUDE.md:
//
//	API:          endpoint, method, status_code, latency_ms, request_bytes, response_bytes
//	AI_AGENT:     agent_id, model, input_tokens, output_tokens, trace_id, session_id,
//	              capability_name, execution_status, execution_duration_ms
//	MCP_SERVER:   agent_id, tool_name, execution_status, execution_duration_ms,
//	              session_id, transport
//	AGENTIC_API:  trace_id, agent_id, endpoint, latency_ms
//
// Every template returns a JSON-serializable map[string]any. The runner wraps
// it in the canonical envelope (event_id, event_timestamp, tenant_id,
// customer_id, subscription_id, product_type, metric_id) before dispatch —
// see envelope.go.
type EventTemplate func(rng *rand.Rand) map[string]any

// TemplateForProductType picks the template by product type string. Unknown
// product types fall back to the API template — this matches what the
// scenario validator already rejects, but defensive at the generator layer
// keeps the worker pool from panicking if a manifest contains a stale value.
func TemplateForProductType(pt scenario.ProductType) EventTemplate {
	switch pt {
	case scenario.ProductAPI:
		return apiTemplate
	case scenario.ProductAIAgent:
		return aiAgentTemplate
	case scenario.ProductMCPServer:
		return mcpServerTemplate
	case scenario.ProductAgenticAPI:
		return agenticAPITemplate
	default:
		return apiTemplate
	}
}

func apiTemplate(rng *rand.Rand) map[string]any {
	statusCodeStr := defaultStatusPicker.Pick(rng)
	statusCode, _ := strconv.Atoi(statusCodeStr)
	return map[string]any{
		"endpoint":       genEndpoint(rng),
		"method":         defaultMethodPicker.Pick(rng),
		"status_code":    statusCode,
		"latency_ms":     genLatencyMs(rng),
		"request_bytes":  genRequestBytes(rng),
		"response_bytes": genResponseBytes(rng),
	}
}

func aiAgentTemplate(rng *rand.Rand) map[string]any {
	in, out := genTokens(rng)
	capability := defaultAgentCapabilities[rng.Intn(len(defaultAgentCapabilities))]
	status := mcpExecStatusPicker.Pick(rng)
	// Capability execution durations: quick classification / extraction, longer
	// for generation / search / planning. Mirrors mcpServerTemplate's tool
	// duration shaping so per-capability latency distributions look realistic.
	dur := genLatencyMs(rng)
	if capability == "generate_response" || capability == "search_knowledge" ||
		capability == "plan_actions" || capability == "translate_document" ||
		capability == "summarize_email" {
		dur += 200 + rng.Intn(800)
	}
	return map[string]any{
		"agent_id":              genAgentID(rng, 32),
		"model":                 defaultModelPicker.Pick(rng),
		"input_tokens":          in,
		"output_tokens":         out,
		"trace_id":              genTraceID(rng),
		"session_id":            genSessionID(rng, 256),
		"capability_name":       capability,
		"execution_status":      status,
		"execution_duration_ms": dur,
	}
}

func mcpServerTemplate(rng *rand.Rand) map[string]any {
	tool := defaultMCPTools[rng.Intn(len(defaultMCPTools))]
	status := mcpExecStatusPicker.Pick(rng)
	// Tool execution durations: short for read-only, longer for queries/AI.
	dur := genLatencyMs(rng)
	if tool == "vector_search" || tool == "summarize" || tool == "extract_entities" {
		dur += 200 + rng.Intn(800)
	}
	return map[string]any{
		"agent_id":              genAgentID(rng, 32),
		"tool_name":             tool,
		"execution_status":      status,
		"execution_duration_ms": dur,
		"session_id":            genSessionID(rng, 256),
		"transport":             mcpTransportPicker.Pick(rng),
	}
}

func agenticAPITemplate(rng *rand.Rand) map[string]any {
	return map[string]any{
		"trace_id":   genTraceID(rng),
		"agent_id":   genAgentID(rng, 32),
		"endpoint":   genEndpoint(rng),
		"latency_ms": genLatencyMs(rng),
	}
}

// Envelope is the canonical event-shaped wrapper that callers POST to
// /v1/ingest. Field names MUST match the backend's IngestUsageEventRequest
// (verified against aforo-nextgen-usage-ingestor-service/.../dto/
// IngestUsageEventRequest.java) — every field uses camelCase, three are
// @NotNull/@NotBlank server-side and MUST be present on every event:
// customerId, metricName, quantity, occurredAt, idempotencyKey.
//
// In-memory routing fields (TenantID, SubscriptionID) carry on Event
// rather than Envelope because they're not part of the request body —
// X-Tenant-Id flows through an HTTP header instead.
//
// Drift-fix 2026-06-01: the prior shape used snake_case field names
// (event_id, event_timestamp, tenant_id, product_type, metric_id) and
// emitted a `body` wrapper with per-template fields. Every event 400'd
// on AWS staging because none of those names match the deployed
// contract and quantity/idempotencyKey/occurredAt were absent. The new
// shape sends template fields as `metadata` and uses MetricName (not
// metric UUID) so backend's name-based metric lookup succeeds.
type Envelope struct {
	CustomerID     string         `json:"customerId"`
	MetricName     string         `json:"metricName"`
	Quantity       float64        `json:"quantity"`
	OccurredAt     time.Time      `json:"occurredAt"`
	IdempotencyKey string         `json:"idempotencyKey"`
	ProductType    string         `json:"productType,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
}
