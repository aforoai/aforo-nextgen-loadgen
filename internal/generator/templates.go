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
//	AI_AGENT:     agent_id, model, input_tokens, output_tokens, trace_id, session_id
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
	return map[string]any{
		"agent_id":      genAgentID(rng, 32),
		"model":         defaultModelPicker.Pick(rng),
		"input_tokens":  in,
		"output_tokens": out,
		"trace_id":      genTraceID(rng),
		"session_id":    genSessionID(rng, 256),
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
// /v1/ingest. Body is the per-product-type fields from a Template; the rest
// is metadata required by the platform's UsageEventValidator.
type Envelope struct {
	EventID        string         `json:"event_id"`
	EventTimestamp time.Time      `json:"event_timestamp"`
	TenantID       string         `json:"tenant_id"`
	CustomerID     string         `json:"customer_id"`
	SubscriptionID string         `json:"subscription_id"`
	ProductType    string         `json:"product_type"`
	MetricID       string         `json:"metric_id,omitempty"`
	Body           map[string]any `json:"body"`
}
