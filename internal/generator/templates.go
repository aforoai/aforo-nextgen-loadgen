package generator

import (
	"math/rand"
	"strconv"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// EventTemplate produces the per-product-type event payload.
//
// Every template emits EVERY field the descriptor's metrics[].key names,
// so the runner can stamp Envelope.Quantity from the descriptor-picked
// field (see generator.produce's Quantity resolution) and every metric's
// aggregation gets exercised end-to-end. Fields the descriptor doesn't
// use are still emitted for realistic payload shape (endpoint / method /
// status_code / trace_id / session_id) — the analytics MV projects them
// via eventSchema.columns[].
//
// Descriptor coverage (verified against
// aforo-nextgen-common/src/main/resources/descriptors/*.json):
//
//	API           request_count / response_bytes / user_id / latency_ms / error_count
//	AI_AGENT      session_count / step_count / total_tokens / tool_call_count /
//	              execution_minutes / gpu_hours / kb_query_count /
//	              task_completed_count / concurrent_agents
//	MCP_SERVER    tool_call_count / duration_minutes / concurrent_sessions /
//	              agent_id / input_tokens / output_tokens / error_count /
//	              response_time_ms
//	AGENTIC_API   request_count / agent_step_count / tool_call_count /
//	              input_tokens / output_tokens / latency_ms / response_bytes /
//	              user_id
//
// AI_AGENT metadata key casing — CRITICAL: usage-ingestor's
// ProductTypeEventExtractor.extractAiAgentFields reads specific keys with
// specific casing. `capability_name` is read snake_case (bridge to
// event.toolName for per-dimension pricing parity with MCP). `agentId`,
// `executionStatus`, `executionDurationMs` are read camelCase (fallback
// path when the top-level DTO fields are absent, which they are because
// the loadgen Envelope struct only carries top-level DTO fields for the
// @NotBlank ones). Emitting these under snake_case keys silently drops
// them at the extractor — no 4xx, just missing analytics data. Regression-
// locked by TestAIAgentTemplateEmitsCanonicalMetadataKeys.
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
	// error_count derives from status_code — 4xx/5xx count as 1, else 0.
	// Backend's COUNT aggregation on error_count sums this deterministically
	// so scenario assertions on Error Requests bill the true 4xx/5xx count.
	errorCount := 0
	if statusCode >= 400 {
		errorCount = 1
	}
	// request_count is a fixed 1 per event — one API call event = one call.
	// Kept explicit so the descriptor's COUNT aggregation on request_count
	// has a numeric field to read rather than needing the null/absent-field
	// COUNT fallback path.
	return map[string]any{
		"endpoint":       genEndpoint(rng),
		"method":         defaultMethodPicker.Pick(rng),
		"status_code":    statusCode,
		"latency_ms":     genLatencyMs(rng),
		"request_bytes":  genRequestBytes(rng),
		"response_bytes": genResponseBytes(rng),
		"request_count":  1,
		"user_id":        genUserID(rng),
		"error_count":    errorCount,
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
	// Capability decides whether this event ALSO counts as a knowledge query
	// or a task completion — the descriptor's KB Queries and Tasks Completed
	// COUNT aggregations then bill the semantic share correctly rather than
	// treating every event as a knowledge/task hit.
	kbQuery := 0
	if capability == "search_knowledge" || capability == "answer_question" {
		kbQuery = 1
	}
	taskCompleted := 0
	if status == "success" && (capability == "generate_response" ||
		capability == "translate_document" || capability == "summarize_email" ||
		capability == "plan_actions" || capability == "execute_tool") {
		taskCompleted = 1
	}
	// GPU-heavy capabilities burn measurable GPU time; light classification
	// tasks still tick a small share of a GPU-second (embedding lookups,
	// tokenization). Must be > 0 — GPU Hours is SUM-aggregated and
	// resolveQuantity would over-bill light-capability events with a
	// zero value (fallback to 1 full GPU-hour on the wire, ~4 orders of
	// magnitude off).
	//
	// Tool calls carried in this event count against the Agent Tool Calls
	// COUNT metric — some capabilities don't call tools.
	var gpuHours float64
	if capability == "generate_response" || capability == "search_knowledge" ||
		capability == "translate_document" || capability == "summarize_email" {
		gpuHours = genGPUHours(rng)
	} else {
		// Light-capability floor: 0.5-5 GPU-milliseconds ≈ 0.0000001 - 0.000001h
		gpuHours = 0.0000001 + rng.Float64()*0.0000009
	}
	toolCallCount := 0
	if capability == "execute_tool" || capability == "search_knowledge" ||
		capability == "plan_actions" {
		toolCallCount = 1 + rng.Intn(4)
	}
	return map[string]any{
		// camelCase keys — extractAiAgentFields reads these exact spellings.
		"agentId":             genAgentID(rng, 32),
		"executionStatus":     status,
		"executionDurationMs": dur,
		// snake_case keys — extractAiAgentFields reads capability_name
		// exactly. session_id / trace_id / model / input_tokens /
		// output_tokens are not extracted by the AI_AGENT path; they land
		// on Envelope.Metadata as JSONB where the analytics MV reads them
		// by descriptor-defined source paths (metadata.model_name etc.).
		"capability_name": capability,
		"model":           defaultModelPicker.Pick(rng),
		"input_tokens":    in,
		"output_tokens":   out,
		// Session grouping — bucketed cardinality so many events share the
		// same traceId + sessionId, which is how usage-ingestor's
		// SessionAggregatorService groups an agentic session (routes to
		// aforo.agentic.sessions Kafka topic only when event.getTraceId()
		// is non-null; per-event random ids silently degraded to 96 rows
		// of 1 event each and Total Sessions: 0 on the dashboard).
		// generator.produce() lifts these to Envelope.TraceID +
		// Envelope.SessionID at the top of the ingest body so
		// ProductTypeEventExtractor.extractTrace reads them (extractTrace
		// only consults body top-level + traceparent/x-trace-id headers,
		// NEVER metadata.trace_id — leaving them here in metadata for
		// analytics MV parity but the top-level copy is what routes).
		"trace_id":        genTraceID(rng, defaultTraceIDCardinality),
		"session_id":      genSessionID(rng, defaultSessionIDCardinality),
		// Descriptor `key` fields that were previously absent — each is
		// consumed by exactly one AI_AGENT metric per descriptors/ai_agent.json.
		"session_count":          1,
		"step_count":             genStepCount(rng),
		"total_tokens":           in + out,
		"tool_call_count":        toolCallCount,
		"execution_minutes":      genExecutionMinutes(rng),
		"gpu_hours":              gpuHours,
		"kb_query_count":         kbQuery,
		"task_completed_count":   taskCompleted,
		"concurrent_agents":      genConcurrentGauge(rng, 1, 32),
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
	// LLM-adjacent tools consume real prompts + completions; read-only /
	// deterministic tools still emit MCP protocol overhead tokens (JSON-RPC
	// envelope + tool metadata + result payload — typically 20-60 tokens).
	// This has to be > 0 for SUM-aggregation MCP Input/Output Tokens metrics:
	// resolveQuantity's SUM branch treats 0 as "field absent" and falls back
	// to Quantity=1, which would over-bill read-only events by 1 token
	// each. Emitting realistic protocol overhead keeps SUM math honest.
	var inTokens, outTokens int
	if tool == "summarize" || tool == "extract_entities" || tool == "embed_text" ||
		tool == "classify" || tool == "translate" || tool == "moderate" {
		inTokens, outTokens = genTokens(rng)
	} else {
		// Protocol overhead — 20-60 in, 10-40 out.
		inTokens = 20 + rng.Intn(41)
		outTokens = 10 + rng.Intn(31)
	}
	// error_count derives from execution_status — success → 0, everything
	// else (error / timeout / unauthorized / rate_limited / validation_error)
	// counts as one error toward the MCP Errors COUNT metric.
	errorCount := 0
	if status != "success" {
		errorCount = 1
	}
	// MCP Session Duration is minutes-scaled; derived from ms so the two
	// aggregations agree per event within rounding.
	durationMinutes := dur / 60_000
	if durationMinutes < 1 && dur > 0 {
		durationMinutes = 1
	}
	return map[string]any{
		"agent_id":              genAgentID(rng, 32),
		"tool_name":             tool,
		"execution_status":      status,
		"execution_duration_ms": dur,
		// MCP grouping is session-driven (McpSessionManager keys on
		// session_id, not traceId) — cardinality matches AI_AGENT so the
		// two agentic surfaces produce a similar events-per-session shape.
		// generator.produce() lifts session_id → Envelope.SessionID.
		"session_id":            genSessionID(rng, defaultSessionIDCardinality),
		"transport":             mcpTransportPicker.Pick(rng),
		// Descriptor `key` fields absent from the historical template —
		// each is the sole event-source for its MCP metric per
		// descriptors/mcp_server.json.
		"tool_call_count":     1,
		"duration_minutes":    durationMinutes,
		"concurrent_sessions": genConcurrentGauge(rng, 1, 512),
		"input_tokens":        inTokens,
		"output_tokens":       outTokens,
		"error_count":         errorCount,
		"response_time_ms":    genPercentileLatencyMs(rng),
	}
}

func agenticAPITemplate(rng *rand.Rand) map[string]any {
	in, out := genTokens(rng)
	// Agentic API events run 1-N tool calls per HTTP request; realistic
	// distribution has 1 tool call being most common, occasional deep
	// chains. `agent_step_count` is the SUM-key for Agentic Steps; each
	// step usually calls exactly one tool.
	toolCalls := 1 + rng.Intn(3)
	if rng.Float64() < 0.05 {
		toolCalls += 3 + rng.Intn(8)
	}
	return map[string]any{
		// Session grouping — same bucketed contract as aiAgentTemplate.
		// generator.produce() lifts trace_id → Envelope.TraceID so
		// SessionAggregatorService's agentic-sessions Kafka listener sees
		// a non-null traceId and creates an agentic_sessions row instead
		// of early-returning at line 80-82.
		"trace_id":         genTraceID(rng, defaultTraceIDCardinality),
		"agent_id":         genAgentID(rng, 32),
		"endpoint":         genEndpoint(rng),
		"latency_ms":       genLatencyMs(rng),
		// Descriptor `key` fields — every AGENTIC_API metric per
		// descriptors/agentic_api.json now has a source in the payload.
		"request_count":      1,
		"agent_step_count":   toolCalls,
		"tool_call_count":    toolCalls,
		"input_tokens":       in,
		"output_tokens":      out,
		"response_bytes":     genResponseBytes(rng),
		"user_id":            genUserID(rng),
		// endpoint_path drives per-endpoint dimension-pricing (see the
		// P0-4 fix in aforo-nextgen-billing-service) and per-endpoint
		// PreBillAnomalyScorer baseline (P0-3 in aforo-nextgen-usage-
		// ingestor-service). Emitting it here lets the loadgen contract
		// test assert one of the two dimension-key surfaces per product
		// type (MCP: tool_name; AGENTIC_API: endpoint_path).
		"endpoint_path":      genEndpoint(rng),
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
	CustomerID     string    `json:"customerId"`
	MetricName     string    `json:"metricName"`
	Quantity       float64   `json:"quantity"`
	OccurredAt     time.Time `json:"occurredAt"`
	IdempotencyKey string    `json:"idempotencyKey"`
	ProductType    string    `json:"productType,omitempty"`
	// TraceID is the top-level body field ProductTypeEventExtractor.extractTrace
	// reads to populate UsageEvent.traceId when the request carries no
	// traceparent / x-trace-id header (loadgen's REST path does not).
	// Non-empty on AI_AGENT + AGENTIC_API events so AgenticEventRouter
	// routes to aforo.agentic.sessions Kafka topic (routeLegacy line 172
	// early-returns for null traceId and skips the session track — that
	// is the exact "Total Sessions: 0 despite 96 events" root cause).
	// Bucketed via defaultTraceIDCardinality so many events share the
	// same id and SessionAggregatorService produces an actual aggregated
	// session row instead of one-event-per-row.
	// Populated by generator.produce() from metadata["trace_id"] — the
	// metadata copy stays as an analytics-MV parity duplicate.
	TraceID string `json:"traceId,omitempty"`
	// SessionID is the top-level body field for AI_AGENT session grouping
	// (per ProductTypeEventExtractor Javadoc: "AI_AGENT: traceId +
	// sessionId"). Lifted from metadata["session_id"] by
	// generator.produce() for the same reason as TraceID.
	SessionID string         `json:"sessionId,omitempty"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}
