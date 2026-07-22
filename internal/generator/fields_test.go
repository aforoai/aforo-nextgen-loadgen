package generator

import (
	"math/rand"
	"strings"
	"testing"
)

// TestTraceIDIsBucketed asserts genTraceID produces IDs from a bounded
// cardinality space so events naturally group into synthetic sessions.
//
// This is the load-bearing invariant behind the 2026-07-22 tester Issue A7
// fix ("Total Sessions: 0 despite 96 events"). SessionAggregatorService
// (usage-ingestor's agentic-sessions Kafka consumer) creates a row per
// distinct (tenantId, traceId) pair. If genTraceID mints a fresh 16-hex per
// event — the pre-fix shape — every AI_AGENT/AGENTIC_API event lands in its
// own single-event session, which surfaces to the operator as "no sessions".
//
// The regression proof: 10k IDs from cardinality=256 must produce ≤ 256
// distinct strings. If a future refactor drops the bucketing (e.g. reverts
// to the pure-random hex shape), this test fails immediately.
func TestTraceIDIsBucketed(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const iterations = 10_000
	const cardinality = 256
	distinct := make(map[string]struct{}, cardinality)
	for i := 0; i < iterations; i++ {
		id := genTraceID(rng, cardinality)
		if id == "" {
			t.Fatalf("iter %d: genTraceID returned empty string", i)
		}
		if !strings.HasPrefix(id, "trace_") {
			t.Errorf("iter %d: genTraceID id %q missing trace_ prefix — do NOT change format without updating the SessionAggregatorService contract note above the function", i, id)
		}
		distinct[id] = struct{}{}
	}
	if len(distinct) > cardinality {
		t.Errorf("genTraceID produced %d distinct IDs from cardinality=%d — bucketing broken; every event will land in its own single-event session (tester Issue A7)", len(distinct), cardinality)
	}
	// Also assert we saw meaningful diversity — a cardinality of 256 over
	// 10k iterations should cover most buckets. If we see < 10% coverage
	// something is deeply wrong (e.g. rng.Intn returning 0 always).
	if len(distinct) < cardinality/10 {
		t.Errorf("genTraceID coverage suspiciously low: %d/%d buckets over 10k iterations — bucketing rng may be stuck", len(distinct), cardinality)
	}
}

// TestTraceIDFallbackCardinality asserts the cardinality < 1 branch falls
// back to a sane default (256) rather than producing empty / panicking IDs.
func TestTraceIDFallbackCardinality(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for _, bad := range []int{0, -1, -256} {
		id := genTraceID(rng, bad)
		if id == "" {
			t.Errorf("cardinality=%d: fell through to empty string", bad)
		}
	}
}

// TestAIAgentTemplateEmitsTraceID — every AI_AGENT event MUST carry a
// non-empty trace_id in metadata (the source generator.produce() lifts to
// Envelope.TraceID). This is what SessionAggregatorService requires to
// create an agentic_sessions row (see line 79-82 of that class:
//
//	String traceId = event.containsKey("traceId") ? String.valueOf(event.get("traceId")) : null;
//	if (traceId == null || "null".equals(traceId) || traceId.isBlank()) {
//	    return;  // <-- silent drop, no analytics
//	}
//
// A regression that drops trace_id from the AI_AGENT template silently
// disables the agentic-sessions surface fleet-wide. Locking this down.
func TestAIAgentTemplateEmitsTraceID(t *testing.T) {
	rng := rand.New(rand.NewSource(2026_07_22))
	const iterations = 200
	for i := 0; i < iterations; i++ {
		ev := aiAgentTemplate(rng)
		trace, ok := ev["trace_id"].(string)
		if !ok {
			t.Fatalf("iter %d: AI_AGENT event missing trace_id or non-string type: %T", i, ev["trace_id"])
		}
		if trace == "" {
			t.Fatalf("iter %d: AI_AGENT event trace_id is empty — SessionAggregatorService.onAgenticSessionEvent would early-return (Issue A7)", i)
		}
	}
}

// TestAgenticAPITemplateEmitsTraceID — sibling assertion for AGENTIC_API.
// Same rationale as TestAIAgentTemplateEmitsTraceID: AgenticEventRouter
// .routeLegacy line 172 early-returns for null traceId and skips the
// aforo.agentic.sessions Kafka publish.
func TestAgenticAPITemplateEmitsTraceID(t *testing.T) {
	rng := rand.New(rand.NewSource(2026_07_22))
	const iterations = 200
	for i := 0; i < iterations; i++ {
		ev := agenticAPITemplate(rng)
		trace, ok := ev["trace_id"].(string)
		if !ok {
			t.Fatalf("iter %d: AGENTIC_API event missing trace_id or non-string type: %T", i, ev["trace_id"])
		}
		if trace == "" {
			t.Fatalf("iter %d: AGENTIC_API event trace_id is empty — AgenticEventRouter.routeLegacy would early-return (Issue A7)", i)
		}
	}
}

// TestAIAgentEventsGroupBySharedTraceID — across many events emitted by the
// same rng seed, some events MUST share a traceId. Bucketing is the whole
// point of the 2026-07-22 fix; without shared IDs each event lands in its
// own session and the dashboard's session count converges to event count.
//
// With cardinality=256 and 3000 iterations we should see collisions
// (~90%+ chance of a collision after ~19 iterations by the birthday bound;
// after 3000 iterations most buckets have multiple events).
func TestAIAgentEventsGroupBySharedTraceID(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const iterations = 3_000
	seen := make(map[string]int, defaultTraceIDCardinality)
	for i := 0; i < iterations; i++ {
		ev := aiAgentTemplate(rng)
		trace, _ := ev["trace_id"].(string)
		seen[trace]++
	}
	// Expect at least one bucket with >= 2 events (shared session grouping).
	sharedSessions := 0
	for _, count := range seen {
		if count >= 2 {
			sharedSessions++
		}
	}
	if sharedSessions == 0 {
		t.Errorf("expected at least one AI_AGENT traceId to be shared across events — got zero; bucketing broken. Distinct traceIds: %d / %d events", len(seen), iterations)
	}
	if len(seen) > defaultTraceIDCardinality {
		t.Errorf("AI_AGENT template produced %d distinct traceIds over %d events — exceeds defaultTraceIDCardinality=%d; genTraceID cardinality gate not being honored", len(seen), iterations, defaultTraceIDCardinality)
	}
}

// TestAgenticAPIEventsGroupBySharedTraceID — sibling test for AGENTIC_API.
func TestAgenticAPIEventsGroupBySharedTraceID(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	const iterations = 3_000
	seen := make(map[string]int, defaultTraceIDCardinality)
	for i := 0; i < iterations; i++ {
		ev := agenticAPITemplate(rng)
		trace, _ := ev["trace_id"].(string)
		seen[trace]++
	}
	sharedSessions := 0
	for _, count := range seen {
		if count >= 2 {
			sharedSessions++
		}
	}
	if sharedSessions == 0 {
		t.Errorf("expected at least one AGENTIC_API traceId to be shared across events — got zero. Distinct traceIds: %d / %d events", len(seen), iterations)
	}
	if len(seen) > defaultTraceIDCardinality {
		t.Errorf("AGENTIC_API template produced %d distinct traceIds over %d events — exceeds defaultTraceIDCardinality=%d", len(seen), iterations, defaultTraceIDCardinality)
	}
}

// TestAgenticAPITemplateEmitsEndpointPath — endpoint_path is the P0-3
// per-endpoint anomaly baseline key + the P0-4 per-endpoint dimension-
// pricing key (usage-ingestor + billing-service, 2026-07-12). Without it,
// AGENTIC_API events silently bill at base rate ignoring configured
// per-endpoint multipliers and the PreBillAnomalyScorer falls back to
// metric-level baselines.
//
// Regression-locking here so a future refactor can't drop it silently.
func TestAgenticAPITemplateEmitsEndpointPath(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	const iterations = 100
	for i := 0; i < iterations; i++ {
		ev := agenticAPITemplate(rng)
		ep, ok := ev["endpoint_path"].(string)
		if !ok {
			t.Fatalf("iter %d: AGENTIC_API event missing endpoint_path or non-string type: %T", i, ev["endpoint_path"])
		}
		if ep == "" {
			t.Fatalf("iter %d: AGENTIC_API event endpoint_path is empty", i)
		}
	}
}
