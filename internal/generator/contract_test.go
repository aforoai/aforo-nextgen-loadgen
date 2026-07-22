package generator

import (
	"math/rand"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// Contract tests — assert every generated event carries the fields its
// corresponding downstream usage-ingestor consumer needs. Read as a
// PRODUCER contract against these specific server-side consumers:
//
//	AI_AGENT     — usage-ingestor SessionAggregatorService (agentic-sessions
//	               Kafka listener) requires event.traceId; AgenticEventRouter
//	               .routeLegacy line 172 early-returns for null; the sessions
//	               table stays empty and Total Sessions dashboard reads 0.
//	             — usage-ingestor ProductTypeEventExtractor.extractAiAgentFields
//	               reads camelCase agentId + executionStatus + executionDurationMs
//	               from either top-level fields OR metadata (fallback). The
//	               loadgen Envelope currently only carries traceId + sessionId
//	               top-level; the rest ride in Metadata as camelCase.
//	MCP_SERVER   — usage-ingestor extractor reads metadata.tool_name (dimension
//	               key for per-tool billing multipliers, P2 dimension-pricing
//	               parity). Missing tool_name → all MCP events bill at base
//	               rate regardless of configured multipliers.
//	             — session_id drives McpSessionManager (session-based grouping,
//	               not traceId-based).
//	AGENTIC_API  — SessionAggregatorService gate (same as AI_AGENT).
//	             — metadata.endpoint_path drives per-endpoint anomaly baseline
//	               (P0-3, PreBillAnomalyScorer per-endpoint scope) and per-
//	               endpoint dimension pricing (P0-4). Missing endpoint_path →
//	               scorer falls back to metric-level baseline; billing
//	               ignores per-endpoint multipliers.
//	API          — endpoint / method / status_code drive standard REST metering.
//
// This test is the producer-side lock. When one of the referenced consumer
// classes above adds a new required field, add it here first — a failing
// contract test forces the loadgen author to grep the consumer source and
// keeps producer and consumer from drifting apart silently.

type contractCheck struct {
	// key is the metadata/template map key to look up.
	key string
	// mustBeString set to true if the field must specifically be a string
	// (not just present with any type). Used for identity fields the
	// extractor stringifies directly.
	mustBeString bool
	// mustBeNonEmpty set to true if the field must be non-blank when
	// stringified — matches the server-side check
	// "if (traceId == null || 'null'.equals(traceId) || traceId.isBlank())"
	// in SessionAggregatorService line 79-82.
	mustBeNonEmpty bool
	// consumer is a short description of the downstream reader; on failure
	// the test message points at it so the next Aforo engineer knows
	// exactly which server-side class started 4xx/silently-dropping on
	// their new field addition.
	consumer string
}

var productTypeContracts = map[scenario.ProductType][]contractCheck{
	scenario.ProductAPI: {
		{key: "endpoint", mustBeString: true, mustBeNonEmpty: true, consumer: "standard REST metering"},
		{key: "method", mustBeString: true, mustBeNonEmpty: true, consumer: "standard REST metering"},
		{key: "status_code", mustBeNonEmpty: true, consumer: "standard REST metering (error_count derivation)"},
	},
	scenario.ProductAIAgent: {
		// The SessionAggregatorService gate — non-null traceId is required.
		{key: "trace_id", mustBeString: true, mustBeNonEmpty: true, consumer: "SessionAggregatorService.onAgenticSessionEvent (line 79-82 traceId gate)"},
		// Session grouping companion for AI_AGENT.
		{key: "session_id", mustBeString: true, mustBeNonEmpty: true, consumer: "ProductTypeEventExtractor Javadoc (AI_AGENT: traceId + sessionId)"},
		// extractAiAgentFields identity + status keys — camelCase per the
		// 2026-07-11 correction (snake_case keys silently drop at the extractor).
		{key: "agentId", mustBeString: true, mustBeNonEmpty: true, consumer: "ProductTypeEventExtractor.extractAiAgentFields (agentId camelCase)"},
		{key: "executionStatus", mustBeString: true, mustBeNonEmpty: true, consumer: "ProductTypeEventExtractor.extractAiAgentFields (executionStatus camelCase)"},
		{key: "executionDurationMs", mustBeNonEmpty: true, consumer: "ProductTypeEventExtractor.extractAiAgentFields (executionDurationMs camelCase)"},
		// capability_name is snake_case — bridged to event.toolName for
		// per-capability dimension pricing parity with MCP tool_name.
		{key: "capability_name", mustBeString: true, mustBeNonEmpty: true, consumer: "ProductTypeEventExtractor.extractAiAgentFields (capability_name → toolName bridge, per-capability dimension pricing)"},
	},
	scenario.ProductMCPServer: {
		// McpSessionManager keys on session_id — grouping surface for MCP.
		{key: "session_id", mustBeString: true, mustBeNonEmpty: true, consumer: "McpSessionManager (session-based grouping)"},
		// Dimension key for per-tool billing multipliers (P2 dimension-
		// pricing parity, 2026-07-10).
		{key: "tool_name", mustBeString: true, mustBeNonEmpty: true, consumer: "billing-service AggregateStage.enrichWithDimensionData (per-tool multipliers)"},
		// Extractor identity + status fields for MCP.
		{key: "agent_id", mustBeString: true, mustBeNonEmpty: true, consumer: "ProductTypeEventExtractor MCP path (agent_id)"},
		{key: "execution_status", mustBeString: true, mustBeNonEmpty: true, consumer: "ProductTypeEventExtractor MCP path (execution_status)"},
		{key: "execution_duration_ms", mustBeNonEmpty: true, consumer: "ProductTypeEventExtractor MCP path (execution_duration_ms)"},
	},
	scenario.ProductAgenticAPI: {
		// SessionAggregatorService gate — same as AI_AGENT.
		{key: "trace_id", mustBeString: true, mustBeNonEmpty: true, consumer: "SessionAggregatorService.onAgenticSessionEvent (line 79-82 traceId gate)"},
		// Per-endpoint anomaly baseline (P0-3) + per-endpoint dimension
		// pricing (P0-4). Without endpoint_path both surfaces silently
		// degrade to metric-level.
		{key: "endpoint_path", mustBeString: true, mustBeNonEmpty: true, consumer: "PreBillAnomalyScorer per-endpoint baseline (P0-3) + billing-service AggregateStage per-endpoint multipliers (P0-4)"},
		{key: "agent_id", mustBeString: true, mustBeNonEmpty: true, consumer: "AGENTIC_API extractor (agent_id)"},
	},
}

// TestProductTemplateConsumerContract — iterates every GA product type and
// asserts the emitted template metadata carries every field the downstream
// consumer requires. Fails loudly with a consumer pointer on any missing
// field so the next Aforo engineer knows the exact class to grep.
//
// This is the regression lock for tester Issue A7 root cause: without it,
// a template refactor could drop trace_id and silently disable the entire
// agentic-sessions surface again.
func TestProductTemplateConsumerContract(t *testing.T) {
	rng := rand.New(rand.NewSource(2026_07_22))
	const iterationsPerProduct = 50

	for productType, checks := range productTypeContracts {
		productType, checks := productType, checks
		t.Run(string(productType), func(t *testing.T) {
			template := TemplateForProductType(productType)
			if template == nil {
				t.Fatalf("no template registered for product type %q", productType)
			}
			for i := 0; i < iterationsPerProduct; i++ {
				ev := template(rng)
				for _, check := range checks {
					v, present := ev[check.key]
					if !present {
						t.Errorf("iter %d: %s template missing field %q — required by %s", i, productType, check.key, check.consumer)
						continue
					}
					if check.mustBeString {
						s, ok := v.(string)
						if !ok {
							t.Errorf("iter %d: %s template field %q must be string, got %T — required by %s", i, productType, check.key, v, check.consumer)
							continue
						}
						if check.mustBeNonEmpty && s == "" {
							t.Errorf("iter %d: %s template field %q is empty — required non-empty by %s", i, productType, check.key, check.consumer)
						}
						continue
					}
					if check.mustBeNonEmpty {
						// Numeric or other type — verify it's not a literal
						// nil. Zero values for status_code / durations are
						// OK for standard API templates (e.g. status 200 is
						// zero-tagged when parsed to int(0) is impossible;
						// but 0 in reality means "no status" which is bad).
						// Keep this gate light — actual value semantics
						// belong in per-template tests.
						if v == nil {
							t.Errorf("iter %d: %s template field %q is nil — required by %s", i, productType, check.key, check.consumer)
						}
					}
				}
			}
		})
	}
}

// TestProductTemplateConsumerContractCoverage — makes sure every GA product
// type has a contract row. New product types must land in
// productTypeContracts so the drift lock covers them.
func TestProductTemplateConsumerContractCoverage(t *testing.T) {
	// The canonical list of GA product types the platform bills. Keep in
	// sync with scenario.ProductType constants + descriptors/*.json.
	gaProductTypes := []scenario.ProductType{
		scenario.ProductAPI,
		scenario.ProductAIAgent,
		scenario.ProductMCPServer,
		scenario.ProductAgenticAPI,
	}
	for _, pt := range gaProductTypes {
		if _, ok := productTypeContracts[pt]; !ok {
			t.Errorf("GA product type %q has no contract in productTypeContracts — add one and cite the downstream consumer file:class the requirement flows from", pt)
		}
	}
}
