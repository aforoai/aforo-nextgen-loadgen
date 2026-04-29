package generator

import (
	"math/rand"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// TestEachProductTemplate — every product type emits a body matching the
// MetricTemplateRegistry shape per CLAUDE.md.
func TestEachProductTemplate(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	api := apiTemplate(rng)
	for _, k := range []string{"endpoint", "method", "status_code", "latency_ms", "request_bytes", "response_bytes"} {
		if _, ok := api[k]; !ok {
			t.Errorf("API template missing %q", k)
		}
	}

	ai := aiAgentTemplate(rng)
	for _, k := range []string{"agent_id", "model", "input_tokens", "output_tokens", "trace_id", "session_id"} {
		if _, ok := ai[k]; !ok {
			t.Errorf("AI_AGENT template missing %q", k)
		}
	}

	mcp := mcpServerTemplate(rng)
	for _, k := range []string{"agent_id", "tool_name", "execution_status", "execution_duration_ms", "session_id", "transport"} {
		if _, ok := mcp[k]; !ok {
			t.Errorf("MCP_SERVER template missing %q", k)
		}
	}

	ag := agenticAPITemplate(rng)
	for _, k := range []string{"trace_id", "agent_id", "endpoint", "latency_ms"} {
		if _, ok := ag[k]; !ok {
			t.Errorf("AGENTIC_API template missing %q", k)
		}
	}
}

// TestTemplateForProductTypeSelectsCorrectShape — dispatcher picks the
// right template by product type string.
func TestTemplateForProductTypeSelectsCorrectShape(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	body := TemplateForProductType(scenario.ProductMCPServer)(rng)
	if _, ok := body["tool_name"]; !ok {
		t.Errorf("MCP template via dispatcher missing tool_name")
	}
	body = TemplateForProductType(scenario.ProductAIAgent)(rng)
	if _, ok := body["model"]; !ok {
		t.Errorf("AI_AGENT template via dispatcher missing model")
	}
	body = TemplateForProductType(scenario.ProductAgenticAPI)(rng)
	if _, ok := body["trace_id"]; !ok {
		t.Errorf("AGENTIC_API template via dispatcher missing trace_id")
	}
	body = TemplateForProductType(scenario.ProductType("UNKNOWN"))(rng)
	if _, ok := body["endpoint"]; !ok {
		t.Errorf("Unknown product type should fall back to API template")
	}
}

// TestTokenCountsBounded — input/output token counts stay within sane
// ranges. Output capped at 32k per template.
func TestTokenCountsBounded(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 1000; i++ {
		in, out := genTokens(rng)
		if in <= 0 || in > 31000 {
			t.Errorf("input tokens out of range: %d", in)
		}
		if out <= 0 || out > 32000 {
			t.Errorf("output tokens out of range: %d", out)
		}
	}
}
