package generator

import (
	"math/rand"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// TestTemplatesEmitEveryDescriptorKey — the ingestion-coverage guarantee.
// Each of the 4 GA product-type templates MUST emit every `key` field
// its descriptor names in metrics[].key, so the runtime's
// resolveQuantity(metadata, metric) always finds a numeric value for
// SUM / MAX / PERCENTILE_95 aggregations. Otherwise those metrics silently
// collapse to COUNT(events) because Envelope.Quantity falls back to 1.
//
// The descriptor keys are hard-coded here (rather than parsed from JSON at
// runtime) because we want the test to fail loudly if a descriptor adds a
// new metric that the loadgen template forgot about — you can't parse the
// descriptor because it's in a separate repo (aforo-nextgen-common).
// Cross-check: aforo-nextgen-common/src/main/resources/descriptors/*.json
// metrics[*].key vs the maps below.
func TestTemplatesEmitEveryDescriptorKey(t *testing.T) {
	expected := map[scenario.ProductType][]string{
		scenario.ProductAPI: {
			"request_count",
			"response_bytes",
			"user_id",
			"latency_ms",
			"error_count",
		},
		scenario.ProductAIAgent: {
			"session_count",
			"step_count",
			"total_tokens",
			"tool_call_count",
			"execution_minutes",
			"gpu_hours",
			"kb_query_count",
			"task_completed_count",
			"concurrent_agents",
		},
		scenario.ProductMCPServer: {
			"tool_call_count",
			"duration_minutes",
			"concurrent_sessions",
			"agent_id",
			"input_tokens",
			"output_tokens",
			"error_count",
			"response_time_ms",
		},
		scenario.ProductAgenticAPI: {
			"request_count",
			"agent_step_count",
			"tool_call_count",
			"input_tokens",
			"output_tokens",
			"latency_ms",
			"response_bytes",
			"user_id",
		},
	}

	rng := rand.New(rand.NewSource(42))
	for pt, keys := range expected {
		body := TemplateForProductType(pt)(rng)
		for _, k := range keys {
			if _, ok := body[k]; !ok {
				t.Errorf("%s template missing descriptor key %q", pt, k)
			}
		}
	}
}

// TestResolveQuantity_ReadsSumEventField — the Quantity resolver picks up
// the numeric value under EventField for SUM aggregations. If this
// silently returns 1, every SUM metric bills COUNT(events) instead of
// SUM(field), which was the exact bug we're closing.
func TestResolveQuantity_ReadsSumEventField(t *testing.T) {
	body := map[string]any{
		"response_bytes": 4096,
		"latency_ms":     150,
	}
	m := seed.ManifestMetric{
		ID:              "metric-1",
		Name:            "Data Transfer",
		EventField:      "response_bytes",
		AggregationType: "SUM",
	}
	got := resolveQuantity(body, m)
	if got != 4096.0 {
		t.Fatalf("resolveQuantity SUM/response_bytes = %v, want 4096", got)
	}
}

// TestResolveQuantity_MaxAndPercentile — MAX and PERCENTILE_95 also read
// the numeric value under EventField (backend takes max / percentile
// across events for those metrics).
func TestResolveQuantity_MaxAndPercentile(t *testing.T) {
	body := map[string]any{"concurrent_sessions": 128, "response_time_ms": 275}
	max := seed.ManifestMetric{Name: "MCP Active Sessions", EventField: "concurrent_sessions", AggregationType: "MAX"}
	p95 := seed.ManifestMetric{Name: "MCP P95 Latency", EventField: "response_time_ms", AggregationType: "PERCENTILE_95"}
	if got := resolveQuantity(body, max); got != 128.0 {
		t.Errorf("MAX resolveQuantity = %v, want 128", got)
	}
	if got := resolveQuantity(body, p95); got != 275.0 {
		t.Errorf("PERCENTILE_95 resolveQuantity = %v, want 275", got)
	}
}

// TestResolveQuantity_CountReturnsOne — COUNT and COUNT_DISTINCT
// aggregations don't read Quantity at all; the resolver returns 1 so the
// backend @Positive constraint is satisfied.
func TestResolveQuantity_CountReturnsOne(t *testing.T) {
	body := map[string]any{"request_count": 99, "user_id": "user_abc"}
	count := seed.ManifestMetric{Name: "API Calls", EventField: "request_count", AggregationType: "COUNT"}
	distinct := seed.ManifestMetric{Name: "Active Users", EventField: "user_id", AggregationType: "COUNT_DISTINCT"}
	if got := resolveQuantity(body, count); got != 1.0 {
		t.Errorf("COUNT resolveQuantity = %v, want 1", got)
	}
	if got := resolveQuantity(body, distinct); got != 1.0 {
		t.Errorf("COUNT_DISTINCT resolveQuantity = %v, want 1", got)
	}
}

// TestResolveQuantity_FallsBackOnMissingField — when the descriptor field
// isn't in the payload (older manifest, template drift, or a metric on a
// product type that hasn't been widened yet), Quantity falls back to 1 so
// the event still passes @Positive validation.
func TestResolveQuantity_FallsBackOnMissingField(t *testing.T) {
	body := map[string]any{"other_field": 42}
	m := seed.ManifestMetric{Name: "Data Transfer", EventField: "response_bytes", AggregationType: "SUM"}
	got := resolveQuantity(body, m)
	if got != 1.0 {
		t.Fatalf("missing field fallback = %v, want 1", got)
	}
}

// TestResolveQuantity_FallsBackOnEmptyMetadata — when metadata itself is
// missing (older manifest or generator-side null propagation), Quantity
// still lands at 1.
func TestResolveQuantity_FallsBackOnEmptyMetadata(t *testing.T) {
	m := seed.ManifestMetric{Name: "Data Transfer", EventField: "response_bytes", AggregationType: "SUM"}
	if got := resolveQuantity(nil, m); got != 1.0 {
		t.Fatalf("nil metadata fallback = %v, want 1", got)
	}
}

// TestResolveQuantity_FallsBackOnManifestWithoutDescriptorFields —
// backward compat: manifests written before EventField/AggregationType
// were plumbed through the seed pipeline leave those blank; resolver
// safely defaults to 1.
func TestResolveQuantity_FallsBackOnManifestWithoutDescriptorFields(t *testing.T) {
	body := map[string]any{"response_bytes": 4096}
	m := seed.ManifestMetric{Name: "Data Transfer"} // EventField + Aggregation both empty
	if got := resolveQuantity(body, m); got != 1.0 {
		t.Fatalf("old-manifest fallback = %v, want 1", got)
	}
}

// TestResolveQuantity_CoerceNumeric — payload floats, ints, int64, uint,
// etc. are all recognized as numeric. Strings/bools/nil/maps/slices are
// not (they degenerate to Quantity=1 by design).
func TestResolveQuantity_CoerceNumeric(t *testing.T) {
	cases := []struct {
		desc     string
		v        any
		want     float64
		wantOK   bool
	}{
		{"float64", float64(42.5), 42.5, true},
		{"float32", float32(5.5), 5.5, true},
		{"int", int(100), 100.0, true},
		{"int32", int32(1_000_000), 1_000_000.0, true},
		{"int64", int64(2_000_000_000), 2_000_000_000.0, true},
		{"uint", uint(50), 50.0, true},
		{"uint32", uint32(100), 100.0, true},
		{"uint64", uint64(9_000_000_000), 9_000_000_000.0, true},
		{"string-rejected", "not-a-number", 0, false},
		{"bool-rejected", true, 0, false},
		{"nil-rejected", nil, 0, false},
		{"map-rejected", map[string]any{"k": 1}, 0, false},
	}
	for _, tc := range cases {
		got, ok := coerceNumeric(tc.v)
		if ok != tc.wantOK {
			t.Errorf("%s: coerceNumeric ok = %v, want %v", tc.desc, ok, tc.wantOK)
		}
		if ok && got != tc.want {
			t.Errorf("%s: coerceNumeric v = %v, want %v", tc.desc, got, tc.want)
		}
	}
}
