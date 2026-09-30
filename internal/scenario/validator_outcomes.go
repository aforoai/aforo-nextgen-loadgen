package scenario

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// Validation for OUTCOME_BASED weights, STAIRCASE bands and the generator's
// outcome_mix. The rules mirror pricing-service so a scenario that passes
// here doesn't 400 at seed time:
//
//	outcome_weights (RatePlanServiceImpl.validateOutcomeWeights):
//	  required + non-empty, keys are canonical statuses (any case), no two
//	  keys that differ only in case, each weight finite and in [0, 1],
//	  at least one weight > 0.
//	staircase_bands (RatePlanServiceImpl.validateStaircaseTiers):
//	  at least one band, first band starts at 0 or 1, every price > 0,
//	  only the last band may be open-ended (to_units 0), to >= from, and
//	  bands are contiguous (from == previous to + 1).

// checkOutcomeWeights validates an OUTCOME_BASED weight map at path.
func (v *validator) checkOutcomeWeights(path []string, weights map[string]float64) {
	if len(weights) == 0 {
		v.addAt(path, "pricing_model=OUTCOME_BASED requires outcome_weights (status → weight in [0, 1])")
		return
	}
	seen := make(map[string]string, len(weights))
	anyPositive := false
	for _, k := range sortedKeys(weights) {
		w := weights[k]
		p := append(append([]string{}, path...), k)
		norm := NormalizeExecutionStatus(k)
		if !IsExecutionStatus(k) {
			v.addAt(p, fmt.Sprintf("outcome_weights key %q is not a canonical execution status (%s)",
				k, strings.Join(ExecutionStatuses, ", ")))
		} else if prior, dup := seen[norm]; dup {
			v.addAt(p, fmt.Sprintf("outcome_weights keys %q and %q name the same status", prior, k))
		} else {
			seen[norm] = k
		}
		if math.IsNaN(w) || math.IsInf(w, 0) || w < 0 || w > 1 {
			v.addAt(p, fmt.Sprintf("outcome_weights[%s] = %v must be in [0, 1]", k, w))
			continue
		}
		if w > 0 {
			anyPositive = true
		}
	}
	if !anyPositive {
		v.addAt(path, "outcome_weights must have at least one weight > 0 — all-zero weights bill nothing")
	}
}

// checkStaircaseBands validates STAIRCASE bands at path.
func (v *validator) checkStaircaseBands(path []string, bands []StaircaseBand) {
	if len(bands) == 0 {
		v.addAt(path, "pricing_model=STAIRCASE requires at least one band in staircase_bands")
		return
	}
	// Pricing sorts the steps by tierStart before checking them (and
	// PricingCalculator sorts before matching), so bands may be written in
	// any order; check them in ascending from_units order and report each
	// problem at the band's own index.
	order := make([]int, len(bands))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(x, y int) bool { return bands[order[x]].FromUnits < bands[order[y]].FromUnits })
	for pos, i := range order {
		b := bands[i]
		p := append(append([]string{}, path...), indexSeg(i))
		last := pos == len(order)-1
		if math.IsNaN(b.PriceUSD) || math.IsInf(b.PriceUSD, 0) || b.PriceUSD <= 0 {
			v.addAt(append(p, "price_usd"),
				fmt.Sprintf("price_usd %v must be > 0 — a $0 step bills nothing for that band", b.PriceUSD))
		}
		if b.FromUnits < 0 {
			v.addAt(append(p, "from_units"), fmt.Sprintf("from_units %d must be >= 0", b.FromUnits))
		}
		if pos == 0 && b.FromUnits > 1 {
			v.addAt(append(p, "from_units"),
				fmt.Sprintf("the lowest band must start at 0 or 1 (got %d) — lower usage would match no band", b.FromUnits))
		}
		if b.ToUnits == 0 {
			if !last {
				v.addAt(append(p, "to_units"),
					"to_units=0 (open-ended) is only valid on the highest band")
			}
		} else if b.ToUnits < b.FromUnits {
			v.addAt(append(p, "to_units"),
				fmt.Sprintf("to_units %d is below from_units %d", b.ToUnits, b.FromUnits))
		}
		if pos > 0 {
			prev := bands[order[pos-1]]
			if prev.ToUnits != 0 && b.FromUnits != prev.ToUnits+1 {
				v.addAt(append(p, "from_units"),
					fmt.Sprintf("bands must be contiguous: from_units %d must be %d (previous to_units + 1)",
						b.FromUnits, prev.ToUnits+1))
			}
		}
	}
}

// checkOutcomeMix validates a status → share map (outcome_mix and each
// outcome_mix_by_product entry): canonical keys, no case-duplicates, shares
// finite and in [0, 1], summing to 1.0.
func (v *validator) checkOutcomeMix(path []string, mix map[string]float64) {
	if len(mix) == 0 {
		return
	}
	seen := make(map[string]string, len(mix))
	sum := 0.0
	for _, k := range sortedKeys(mix) {
		share := mix[k]
		p := append(append([]string{}, path...), k)
		norm := NormalizeExecutionStatus(k)
		if !IsExecutionStatus(k) {
			v.addAt(p, fmt.Sprintf("outcome_mix key %q is not a canonical execution status (%s)",
				k, strings.Join(ExecutionStatuses, ", ")))
		} else if prior, dup := seen[norm]; dup {
			v.addAt(p, fmt.Sprintf("outcome_mix keys %q and %q name the same status", prior, k))
		} else {
			seen[norm] = k
		}
		if math.IsNaN(share) || math.IsInf(share, 0) || share < 0 || share > 1 {
			v.addAt(p, fmt.Sprintf("outcome_mix[%s] = %v must be in [0, 1]", k, share))
			continue
		}
		sum += share
	}
	if !weightsApproxOne(sum) {
		v.addAt(path, fmt.Sprintf("outcome_mix shares sum to %.4f; must be 1.0 ± %g", sum, weightTolerance))
	}
}

// checkOutcomeMixes validates the scenario-level outcome_mix and every
// outcome_mix_by_product entry.
func (v *validator) checkOutcomeMixes(s *Scenario) {
	v.checkOutcomeMix([]string{"outcome_mix"}, s.OutcomeMix)
	pts := make([]string, 0, len(s.OutcomeMixByProduct))
	for pt := range s.OutcomeMixByProduct {
		pts = append(pts, string(pt))
	}
	sort.Strings(pts)
	for _, pt := range pts {
		p := []string{"outcome_mix_by_product", pt}
		if !isValidProductType(ProductType(pt)) {
			v.addAt(p, fmt.Sprintf("product type %q is not one of: API, AI_AGENT, MCP_SERVER, AGENTIC_API", pt))
			continue
		}
		mix := s.OutcomeMixByProduct[ProductType(pt)]
		if len(mix) == 0 {
			v.addAt(p, "outcome_mix_by_product entry must not be empty")
			continue
		}
		v.checkOutcomeMix(p, mix)
	}
}

// checkModelSpecificConfig applies the OUTCOME_BASED / STAIRCASE minimums to
// a rate config at path (archetype rate_config or a rate card's). The other
// six models keep their existing inline checks.
func (v *validator) checkModelSpecificConfig(path []string, model PricingModel, rc RateConfig, prefix string) {
	switch model {
	case PricingOutcomeBased:
		if !(rc.PerUnitRateUSD > 0) {
			v.addAt(append(append([]string{}, path...), "rate_config", "per_unit_rate_usd"),
				prefix+"pricing_model=OUTCOME_BASED requires rate_config.per_unit_rate_usd > 0")
		}
		v.checkOutcomeWeights(append(append([]string{}, path...), "rate_config", "outcome_weights"), rc.OutcomeWeights)
	case PricingStaircase:
		v.checkStaircaseBands(append(append([]string{}, path...), "rate_config", "staircase_bands"), rc.StaircaseBands)
	}
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
