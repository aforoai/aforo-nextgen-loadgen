package generator

import (
	"fmt"
	"math/rand"
)

// Field-generation helpers shared by all product templates. We aim for
// realism (cardinality close to a real platform's distribution) while
// staying deterministic for a given rng. All helpers take the rng as the
// last argument; callers thread the same rng through one event's lifetime.

// Common HTTP method distribution — favors GET/POST as in real APIs.
var defaultMethodPicker = NewWeightedPicker(map[string]float64{
	"GET":     0.55,
	"POST":    0.25,
	"PUT":     0.08,
	"PATCH":   0.05,
	"DELETE":  0.04,
	"HEAD":    0.02,
	"OPTIONS": 0.01,
})

// Status code distribution — overwhelmingly 2xx, occasional 4xx, rare 5xx.
// This is what real production traffic looks like at steady state.
var defaultStatusPicker = NewWeightedPicker(map[string]float64{
	"200": 0.78,
	"201": 0.06,
	"204": 0.02,
	"301": 0.005,
	"304": 0.02,
	"400": 0.03,
	"401": 0.02,
	"403": 0.015,
	"404": 0.02,
	"409": 0.005,
	"422": 0.01,
	"429": 0.005,
	"500": 0.008,
	"502": 0.004,
	"503": 0.003,
	"504": 0.005,
})

// AI model registry — covers Anthropic + OpenAI families. Used by AI_AGENT
// and AGENTIC_API templates.
var defaultModelPicker = NewWeightedPicker(map[string]float64{
	"claude-opus-4-7":           0.18,
	"claude-sonnet-4-6":         0.42,
	"claude-haiku-4-5-20251001": 0.20,
	"gpt-4o":                    0.10,
	"gpt-4o-mini":               0.06,
	"o3":                        0.02,
	"o3-mini":                   0.02,
})

// MCP tool name registry — names mirror common MCP server registries.
var defaultMCPTools = []string{
	"search_web", "search_docs", "fetch_url", "list_files",
	"read_file", "write_file", "execute_query", "create_record",
	"update_record", "delete_record", "send_email", "list_calendar",
	"create_event", "vector_search", "embed_text", "summarize",
	"extract_entities", "translate", "classify", "moderate",
}

// AI_AGENT capability registry — names mirror the canonical capability set
// used across AI agent frameworks (LangChain / CrewAI / AutoGen). Emitted as
// metadata.capability_name (snake_case), which
// ProductTypeEventExtractor.extractAiAgentFields at
// usage-ingestor bridges to event.toolName so the descriptor-driven
// per-capability dimension pricing + anomaly baseline paths get exercised.
//
// Rotation is deterministic per rng — matches how mcpServerTemplate picks
// from defaultMCPTools.
var defaultAgentCapabilities = []string{
	"summarize_email", "translate_document", "answer_question",
	"classify_intent", "extract_entities", "generate_response",
	"search_knowledge", "plan_actions", "execute_tool", "verify_output",
}

// MCP transports — matches what gateway plugins detect.
var mcpTransportPicker = NewWeightedPicker(map[string]float64{
	"http":      0.65,
	"stdio":     0.15,
	"sse":       0.15,
	"websocket": 0.05,
})

// MCP execution status — biased to success but with realistic error mix.
var mcpExecStatusPicker = NewWeightedPicker(map[string]float64{
	"success":          0.92,
	"error":            0.04,
	"timeout":          0.02,
	"unauthorized":     0.01,
	"rate_limited":     0.005,
	"validation_error": 0.005,
})

// Endpoint generator — deterministic per-tenant cardinality. We model a
// tenant having ~100 distinct API endpoints in production. Format:
// "/api/v1/<noun>/<verb>" with stable noun/verb sets.
var endpointNouns = []string{
	"users", "orders", "products", "invoices", "subscriptions",
	"customers", "events", "metrics", "rate-plans", "offerings",
	"wallets", "transactions", "credits", "discounts", "agents",
	"sessions", "tools", "messages", "files", "tasks",
}

var endpointVerbs = []string{
	"", "list", "search", "summary", "export",
	"import", "stats", "history", "audit", "preview",
}

// genEndpoint picks an endpoint with cardinality controlled by rng.
// Returns one of ~100 distinct strings.
func genEndpoint(rng *rand.Rand) string {
	noun := endpointNouns[rng.Intn(len(endpointNouns))]
	verb := endpointVerbs[rng.Intn(len(endpointVerbs))]
	if verb == "" {
		// Half the time we hit the resource root, half a UUID-like detail.
		if rng.Intn(2) == 0 {
			return "/api/v1/" + noun
		}
		return fmt.Sprintf("/api/v1/%s/%s", noun, genResourceID(rng))
	}
	return fmt.Sprintf("/api/v1/%s/%s", noun, verb)
}

// genResourceID returns a 12-hex-char id-shaped string. Cardinality is
// effectively unbounded but rng-driven so tests can assert determinism.
func genResourceID(rng *rand.Rand) string {
	b := make([]byte, 12)
	const hex = "0123456789abcdef"
	for i := range b {
		b[i] = hex[rng.Intn(16)]
	}
	return string(b)
}

// genAgentID returns a deterministic agent id of shape "agent_<8hex>".
// Cardinality matches AI deployments — typically 5-50 agents per tenant.
// Caller passes the cardinality cap to keep per-tenant agent count realistic.
func genAgentID(rng *rand.Rand, cardinality int) string {
	if cardinality < 1 {
		cardinality = 16
	}
	idx := rng.Intn(cardinality)
	return fmt.Sprintf("agent_%08x", idx)
}

// genTraceID returns a 16-hex-char id (W3C-trace-context style).
func genTraceID(rng *rand.Rand) string {
	b := make([]byte, 16)
	const hex = "0123456789abcdef"
	for i := range b {
		b[i] = hex[rng.Intn(16)]
	}
	return string(b)
}

// genSessionID returns a session id with bounded cardinality, simulating
// "active sessions" — typically 100-1000 per tenant at a time.
func genSessionID(rng *rand.Rand, cardinality int) string {
	if cardinality < 1 {
		cardinality = 256
	}
	return fmt.Sprintf("sess_%08x", rng.Intn(cardinality))
}

// genLatencyMs returns a realistic latency in milliseconds.
// Distribution: 80% in [10, 80], 15% in [80, 250], 4% in [250, 1500],
// 1% slow tail in [1500, 8000]. Mimics a real p99 long-tail.
func genLatencyMs(rng *rand.Rand) int {
	r := rng.Float64()
	switch {
	case r < 0.80:
		return 10 + rng.Intn(70)
	case r < 0.95:
		return 80 + rng.Intn(170)
	case r < 0.99:
		return 250 + rng.Intn(1250)
	default:
		return 1500 + rng.Intn(6500)
	}
}

// genTokens returns a (input, output) pair for AI events.
// Modeled as truncated lognormal-ish: most prompts are small, some long.
func genTokens(rng *rand.Rand) (input, output int) {
	// Input: median ~600, mean ~1500, p99 ~12000.
	r := rng.Float64()
	switch {
	case r < 0.50:
		input = 50 + rng.Intn(600)
	case r < 0.85:
		input = 600 + rng.Intn(2000)
	case r < 0.99:
		input = 2600 + rng.Intn(8000)
	default:
		input = 10600 + rng.Intn(20000)
	}
	// Output: typically 1.5-3x input but capped.
	mult := 1.5 + rng.Float64()*1.5
	output = int(float64(input) * mult)
	if output > 32000 {
		output = 32000
	}
	return input, output
}

// genRequestBytes returns a realistic request body size.
// Most API requests are tiny (GET, no body) or small POST/PUT (~1KB).
func genRequestBytes(rng *rand.Rand) int {
	r := rng.Float64()
	switch {
	case r < 0.55:
		return 0 // GET / DELETE / HEAD
	case r < 0.85:
		return 200 + rng.Intn(2000)
	case r < 0.98:
		return 2200 + rng.Intn(20000)
	default:
		return 22200 + rng.Intn(500_000)
	}
}

// genResponseBytes returns a realistic response body size.
// Skews larger than request because of list responses, payloads, etc.
func genResponseBytes(rng *rand.Rand) int {
	r := rng.Float64()
	switch {
	case r < 0.40:
		return 100 + rng.Intn(2000)
	case r < 0.85:
		return 2100 + rng.Intn(20000)
	case r < 0.98:
		return 22100 + rng.Intn(200_000)
	default:
		return 222100 + rng.Intn(1_000_000)
	}
}

// genUserID returns a synthetic end-user id with bounded cardinality — used
// by every descriptor whose Active-Users metric aggregates COUNT_DISTINCT
// on `user_id`. Cardinality ~5000/tenant matches a mid-sized SaaS's active
// user population; adjust upstream if a scenario needs a heavier tail.
func genUserID(rng *rand.Rand) string {
	return fmt.Sprintf("user_%06x", rng.Intn(5000))
}

// genGPUHours returns fractional GPU-hours consumed for an AI_AGENT step.
// Modeled as a small typical value (0.001-0.05 h ≈ 3.6s-180s per event)
// with a heavy tail for long training / batch generation calls.
func genGPUHours(rng *rand.Rand) float64 {
	r := rng.Float64()
	switch {
	case r < 0.85:
		return 0.001 + rng.Float64()*0.049
	case r < 0.98:
		return 0.05 + rng.Float64()*0.5
	default:
		return 0.55 + rng.Float64()*3.5
	}
}

// genExecutionMinutes returns whole-minute execution durations for
// AI_AGENT tasks. Most tasks complete in <5m; the tail models long-running
// agents.
func genExecutionMinutes(rng *rand.Rand) int {
	r := rng.Float64()
	switch {
	case r < 0.70:
		return 1 + rng.Intn(5)
	case r < 0.95:
		return 5 + rng.Intn(15)
	default:
		return 20 + rng.Intn(40)
	}
}

// genStepCount returns the number of agent steps taken in one event.
// Small for simple tasks, larger for multi-tool orchestrations.
func genStepCount(rng *rand.Rand) int {
	r := rng.Float64()
	switch {
	case r < 0.55:
		return 1
	case r < 0.85:
		return 2 + rng.Intn(3)
	case r < 0.98:
		return 5 + rng.Intn(10)
	default:
		return 15 + rng.Intn(25)
	}
}

// genConcurrentGauge returns a MAX-aggregated gauge value between low and
// high. Used for concurrent_agents / concurrent_sessions — the platform's
// MAX aggregation takes the highest quantity in the period, so a realistic
// distribution biases toward the middle of the range.
func genConcurrentGauge(rng *rand.Rand, low, high int) int {
	if low >= high {
		return low
	}
	// Triangular distribution centered ~30% between low and high.
	base := float64(low) + rng.Float64()*float64(high-low)
	skew := rng.Float64() * 0.5 * float64(high-low)
	v := int(base + skew)
	if v > high {
		v = high
	}
	if v < low {
		v = low
	}
	return v
}

// genPercentileLatencyMs is a Quantity value for PERCENTILE_95 metrics
// (MCP P95 Latency). It mirrors genLatencyMs so per-event latency stamped
// as Quantity feeds the percentile aggregator correctly.
func genPercentileLatencyMs(rng *rand.Rand) int {
	return genLatencyMs(rng)
}
