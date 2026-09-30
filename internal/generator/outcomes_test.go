package generator

import (
	"encoding/json"
	"math"
	"math/rand"
	"strconv"
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// pickerShares turns a WeightedPicker's CDF back into key → probability.
func pickerShares(p WeightedPicker) map[string]float64 {
	out := make(map[string]float64, len(p.keys))
	prev := 0.0
	for i, k := range p.keys {
		out[k] = p.cum[i] - prev
		prev = p.cum[i]
	}
	return out
}

// TestAPIDefaultOutcomeMixMatchesStatusCodes locks scenario.DefaultOutcomeMix(API)
// (what the billing validator assumes) to what the API template actually
// emits: the status-code distribution mapped through outcomeForStatusCode.
func TestAPIDefaultOutcomeMixMatchesStatusCodes(t *testing.T) {
	derived := map[string]float64{}
	for code, share := range pickerShares(defaultStatusPicker) {
		n, _ := strconv.Atoi(code)
		derived[outcomeForStatusCode(n)] += share
	}
	want := scenario.DefaultOutcomeMix(scenario.ProductAPI)
	if len(derived) != len(want) {
		t.Fatalf("derived %v vs default %v", derived, want)
	}
	for k, v := range want {
		if math.Abs(derived[k]-v) > 1e-9 {
			t.Errorf("%s: status codes give %.6f, DefaultOutcomeMix says %.6f", k, derived[k], v)
		}
	}
}

func TestOutcomeForStatusCode(t *testing.T) {
	cases := map[int]string{
		200: "SUCCESS", 204: "SUCCESS", 304: "SUCCESS",
		408: "TIMEOUT", 504: "TIMEOUT", 499: "CANCELLED",
		400: "VALIDATION_FAILED", 422: "VALIDATION_FAILED",
		401: "BLOCKED", 403: "BLOCKED", 429: "BLOCKED",
		404: "ERROR", 409: "ERROR", 500: "ERROR", 503: "ERROR",
		0: "", 102: "",
	}
	for code, want := range cases {
		if got := outcomeForStatusCode(code); got != want {
			t.Errorf("%d → %q; want %q", code, got, want)
		}
	}
}

// Every template emits only canonical upper-case statuses by default.
func TestTemplatesEmitCanonicalStatuses(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, pt := range scenario.AllProductTypes {
		tmpl := TemplateForProductType(pt)
		for i := 0; i < 500; i++ {
			st := executionStatusOf(tmpl(rng))
			if st == "" {
				t.Fatalf("%s: template emitted no execution status", pt)
			}
			if !scenario.IsExecutionStatus(st) || st != strings.ToUpper(st) {
				t.Fatalf("%s: non-canonical status %q", pt, st)
			}
		}
	}
}

// API: with no configured mix, the status follows status_code.
func TestAPITemplateStatusFollowsStatusCode(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for i := 0; i < 500; i++ {
		body := apiTemplate(rng, nil)
		code, _ := body["status_code"].(int)
		if got, want := body["execution_status"], outcomeForStatusCode(code); got != want {
			t.Fatalf("status_code %d gave %v; want %s", code, got, want)
		}
	}
}

// A configured outcome_mix drives the observed distribution (±2pp over 20k).
func TestConfiguredOutcomeMixDistribution(t *testing.T) {
	sc := &scenario.Scenario{
		OutcomeMix: map[string]float64{"success": 0.9, "PARTIAL": 0.05, "HITL_REQUIRED": 0.05},
	}
	tmpls := templatesFor(sc)
	rng := rand.New(rand.NewSource(3))
	for _, pt := range scenario.AllProductTypes {
		counts := map[string]int{}
		const n = 20000
		for i := 0; i < n; i++ {
			counts[executionStatusOf(tmpls[pt](rng))]++
		}
		for status, want := range map[string]float64{"SUCCESS": 0.9, "PARTIAL": 0.05, "HITL_REQUIRED": 0.05} {
			got := float64(counts[status]) / n
			if math.Abs(got-want) > 0.02 {
				t.Errorf("%s %s: observed %.3f, want %.2f", pt, status, got, want)
			}
		}
		if len(counts) != 3 {
			t.Errorf("%s: unexpected statuses %v", pt, counts)
		}
	}
}

// Non-error outcomes (PARTIAL / HITL_REQUIRED) must not count toward
// error_count; error outcomes must.
func TestErrorCountFollowsOutcome(t *testing.T) {
	for status, wantErr := range map[string]int{"SUCCESS": 0, "PARTIAL": 0, "HITL_REQUIRED": 0, "PENDING": 0,
		"ERROR": 1, "TIMEOUT": 1, "BLOCKED": 1, "VALIDATION_FAILED": 1, "FAILED": 1, "FAILURE": 1, "CANCELLED": 1} {
		p := NewWeightedPicker(map[string]float64{status: 1})
		body := mcpServerTemplate(rand.New(rand.NewSource(1)), p)
		if body["error_count"] != wantErr {
			t.Errorf("%s: error_count %v; want %d", status, body["error_count"], wantErr)
		}
	}
}

// The generator lifts the status to the top-level executionStatus field —
// the only status field usage-ingestor reads for every product type.
func TestGeneratorLiftsExecutionStatus(t *testing.T) {
	scn := miniScenario()
	scn.OutcomeMix = map[string]float64{"PARTIAL": 1.0}
	g, err := NewGenerator(Config{Scenario: scn, Manifest: miniManifest(), Now: fixedNow})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range collectN(t, g, 20) {
		if e.Envelope.ExecutionStatus != "PARTIAL" {
			t.Fatalf("envelope executionStatus %q; want PARTIAL", e.Envelope.ExecutionStatus)
		}
		raw, _ := json.Marshal(e.Envelope)
		if !strings.Contains(string(raw), `"executionStatus":"PARTIAL"`) {
			t.Fatalf("serialized envelope lacks executionStatus: %s", raw)
		}
	}
}
