package scenario

// Execution-status (outcome) distribution helpers shared by the generator
// (which statuses to emit) and the billing validator (what OUTCOME_BASED
// should bill when only an event count is known).

// defaultAgentOutcomeMix is the built-in status distribution for the agentic
// templates (AI_AGENT, MCP_SERVER, AGENTIC_API). It is the historical
// lowercase mix (success .92 / error .04 / timeout .02 / unauthorized .01 /
// rate_limited .005 / validation_error .005) mapped onto canonical statuses:
// unauthorized + rate_limited → BLOCKED (the gateways' 401/403/429 mapping),
// validation_error → VALIDATION_FAILED.
var defaultAgentOutcomeMix = map[string]float64{
	"SUCCESS":           0.92,
	"ERROR":             0.04,
	"TIMEOUT":           0.02,
	"BLOCKED":           0.015,
	"VALIDATION_FAILED": 0.005,
}

// defaultAPIOutcomeMixRaw is the expected status share of the API template,
// which derives execution_status from its status_code with the gateway
// default table (2xx/3xx → SUCCESS, 408/504 → TIMEOUT, 499 → CANCELLED,
// 400/422 → VALIDATION_FAILED, 401/403/429 → BLOCKED, other 4xx/5xx → ERROR).
// Values are the generator's status-code weights summed per outcome; they
// total 1.01 (the status picker normalises), so DefaultOutcomeMix normalises
// too. Locked against the generator's status picker by
// TestAPIDefaultOutcomeMixMatchesStatusCodes.
var defaultAPIOutcomeMixRaw = map[string]float64{
	"SUCCESS":           0.885, // 200 .78 + 201 .06 + 204 .02 + 301 .005 + 304 .02
	"VALIDATION_FAILED": 0.04,  // 400 .03 + 422 .01
	"BLOCKED":           0.04,  // 401 .02 + 403 .015 + 429 .005
	"TIMEOUT":           0.005, // 504 .005
	"ERROR":             0.04,  // 404 .02 + 409 .005 + 500 .008 + 502 .004 + 503 .003
}

// DefaultOutcomeMix returns the built-in status distribution for a product
// type, normalised to sum to 1.0. The returned map is a fresh copy.
func DefaultOutcomeMix(pt ProductType) map[string]float64 {
	if pt == ProductAPI {
		return normalizeOutcomeMix(defaultAPIOutcomeMixRaw)
	}
	return normalizeOutcomeMix(defaultAgentOutcomeMix)
}

// EffectiveOutcomeMix resolves the status distribution for a product type:
// outcome_mix_by_product[pt] wins, then outcome_mix, then the built-in
// default. configured is false only for the built-in default — the generator
// uses that to keep the API template's status tied to its status_code.
// The returned map has canonical upper-case keys and sums to 1.0.
func (s *Scenario) EffectiveOutcomeMix(pt ProductType) (mix map[string]float64, configured bool) {
	if s != nil {
		if m, ok := s.OutcomeMixByProduct[pt]; ok && len(m) > 0 {
			return normalizeOutcomeMix(m), true
		}
		if len(s.OutcomeMix) > 0 {
			return normalizeOutcomeMix(s.OutcomeMix), true
		}
	}
	return DefaultOutcomeMix(pt), false
}

// normalizeOutcomeMix upper-cases + trims keys (merging keys that differ only
// in case), drops non-positive weights, and scales the shares to sum to 1.0.
// An all-zero mix returns an empty map.
func normalizeOutcomeMix(in map[string]float64) map[string]float64 {
	out := make(map[string]float64, len(in))
	total := 0.0
	for k, v := range in {
		if v <= 0 {
			continue
		}
		key := NormalizeExecutionStatus(k)
		if key == "" {
			continue
		}
		out[key] += v
		total += v
	}
	if total <= 0 {
		return map[string]float64{}
	}
	for k, v := range out {
		out[k] = v / total
	}
	return out
}
