package seed

import (
	"fmt"
	"math/rand"
	"sort"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// ArchetypeAllocation expands the scenario's archetype list into per-archetype
// tenant counts, deterministic given a seed. Sum of counts == scenario.tenants.count
// (largest-residual rounding).
type ArchetypeAllocation struct {
	Archetype scenario.TenantArchetype
	Count     int
}

// AllocateTenants computes how many tenants each archetype gets. The scenario
// validator already enforces that weights sum to 1.0 ± 0.001; we apply
// largest-residual to absorb the rounding floor.
//
// Sort key is archetype name → reproducible across runs.
func AllocateTenants(s *scenario.Scenario) []ArchetypeAllocation {
	if s == nil || s.Tenants.Count <= 0 || len(s.Tenants.Archetypes) == 0 {
		return nil
	}
	n := s.Tenants.Count
	arches := append([]scenario.TenantArchetype(nil), s.Tenants.Archetypes...)
	sort.Slice(arches, func(i, j int) bool { return arches[i].Name < arches[j].Name })

	type carrier struct {
		idx       int
		archetype scenario.TenantArchetype
		raw       float64
		floor     int
		residual  float64
	}
	carriers := make([]carrier, len(arches))
	totalWeight := 0.0
	for _, a := range arches {
		totalWeight += a.Weight
	}
	if totalWeight <= 0 {
		// Validator shouldn't allow this, but guard so we don't /0.
		return nil
	}

	allocated := 0
	for i, a := range arches {
		raw := (a.Weight / totalWeight) * float64(n)
		fl := int(raw)
		carriers[i] = carrier{i, a, raw, fl, raw - float64(fl)}
		allocated += fl
	}

	// Distribute the rounding remainder by descending residual, breaking ties
	// by archetype name (already in sorted order).
	if allocated < n {
		ordered := append([]carrier(nil), carriers...)
		sort.SliceStable(ordered, func(i, j int) bool {
			if ordered[i].residual != ordered[j].residual {
				return ordered[i].residual > ordered[j].residual
			}
			return ordered[i].archetype.Name < ordered[j].archetype.Name
		})
		for k := 0; allocated < n; k++ {
			ordered[k%len(ordered)].floor++
			allocated++
		}
		// Mutate carriers in original order using the (possibly bumped) floors.
		// Build map from idx → bumped floor.
		bump := make(map[int]int, len(ordered))
		for _, c := range ordered {
			bump[c.idx] = c.floor
		}
		for i := range carriers {
			carriers[i].floor = bump[i]
		}
	}

	out := make([]ArchetypeAllocation, len(carriers))
	for i, c := range carriers {
		out[i] = ArchetypeAllocation{Archetype: c.archetype, Count: c.floor}
	}
	return out
}

// FilterArchetypes returns only the allocations whose archetype name is in
// the include set. If include is empty, allocs is returned unchanged.
func FilterArchetypes(allocs []ArchetypeAllocation, include []string) []ArchetypeAllocation {
	if len(include) == 0 {
		return allocs
	}
	allow := map[string]struct{}{}
	for _, n := range include {
		allow[n] = struct{}{}
	}
	out := make([]ArchetypeAllocation, 0, len(allocs))
	for _, a := range allocs {
		if _, ok := allow[a.Archetype.Name]; ok {
			out = append(out, a)
		}
	}
	return out
}

// archetypePlan is the per-tenant plan derived from an archetype: which
// state to put each customer's subscription into, which currency to use,
// which discount to apply. Computed up front so seeding is deterministic.
type archetypePlan struct {
	Customers []customerPlan
}

type customerPlan struct {
	Currency string
	Discount *ManifestDiscount
	State    scenario.SubscriptionState
	// RNG-derived suffix for unique external IDs.
	Seq int
	// CardIndex is the index into the archetype's RateCards slice that
	// this customer subscribes to. Populated by planArchetype using the
	// per-card CustomerShare weights. Always in [0, len(RateCards)).
	CardIndex int
}

// planArchetype expands one archetype into per-customer plans. customer_count
// is treated as the number of customers per tenant (NOT the number of tenants).
//
// State distribution is exact (proportional rounding); currency is exact;
// discount is sampled per-customer (the validator doesn't constrain discount
// distribution to be exact, so per-draw sampling is acceptable).
func planArchetype(a scenario.TenantArchetype, rng *rand.Rand) archetypePlan {
	n := a.CustomerCount
	if n <= 0 {
		return archetypePlan{}
	}
	currencies := distributeCurrencies(a.CurrencyMix, n)
	states := distributeStates(a.SubscriptionStateMix, n)
	// v2 (2026-07-22): distribute customers across the archetype's RateCards
	// using each card's CustomerShare weight. When RateCards has exactly one
	// card (backfilled from legacy scalars), every customer gets CardIndex=0
	// — preserving v1 behavior.
	cardIdxs := distributeCardIndices(a.RateCards, n)

	// Shuffle currencies, states, and cardIdxs independently so
	// currency=EUR isn't always paired with state=ACTIVE or with card #0.
	// All shuffles use the same rng, so runs with the same seed produce the
	// same pairing.
	rng.Shuffle(len(currencies), func(i, j int) {
		currencies[i], currencies[j] = currencies[j], currencies[i]
	})
	rng.Shuffle(len(states), func(i, j int) {
		states[i], states[j] = states[j], states[i]
	})
	rng.Shuffle(len(cardIdxs), func(i, j int) {
		cardIdxs[i], cardIdxs[j] = cardIdxs[j], cardIdxs[i]
	})

	plans := make([]customerPlan, n)
	for i := 0; i < n; i++ {
		plans[i] = customerPlan{
			Currency:  currencies[i],
			State:     states[i],
			Discount:  drawDiscount(a.DiscountMix, rng),
			Seq:       i + 1,
			CardIndex: cardIdxs[i],
		}
	}
	return archetypePlan{Customers: plans}
}

// distributeCardIndices returns a slice of length n where each element is
// the index of the RateCardSpec that customer i subscribes to. Card counts
// are proportional to each card's CustomerShare with largest-residual
// rounding, so the exact distribution is deterministic given the same
// (RateCards, n) tuple. When RateCards is empty (shouldn't happen after
// applyDefaults but guarded for safety), returns all-zeros.
func distributeCardIndices(cards []scenario.RateCardSpec, n int) []int {
	if len(cards) == 0 || n <= 0 {
		return make([]int, n)
	}
	if len(cards) == 1 {
		return make([]int, n) // all zeros
	}
	type slot struct {
		idx      int
		raw      float64
		floor    int
		residual float64
	}
	slots := make([]slot, len(cards))
	allocated := 0
	for i, c := range cards {
		raw := c.CustomerShare * float64(n)
		fl := int(raw)
		slots[i] = slot{i, raw, fl, raw - float64(fl)}
		allocated += fl
	}
	// Distribute rounding remainder by descending residual.
	if allocated < n {
		ordered := append([]slot(nil), slots...)
		sort.SliceStable(ordered, func(i, j int) bool {
			if ordered[i].residual != ordered[j].residual {
				return ordered[i].residual > ordered[j].residual
			}
			return ordered[i].idx < ordered[j].idx
		})
		for k := 0; allocated < n; k++ {
			ordered[k%len(ordered)].floor++
			allocated++
		}
		bump := make(map[int]int, len(ordered))
		for _, s := range ordered {
			bump[s.idx] = s.floor
		}
		for i := range slots {
			slots[i].floor = bump[i]
		}
	}
	out := make([]int, 0, n)
	for _, s := range slots {
		for k := 0; k < s.floor; k++ {
			out = append(out, s.idx)
		}
	}
	// Guard against edge cases where floor+residual rounding produced
	// fewer than n entries (shouldn't happen post-remainder, but be
	// defensive).
	for len(out) < n {
		out = append(out, 0)
	}
	return out[:n]
}

// expectedBillingFormula renders a one-line description of how the platform
// should bill a subscription, given the archetype's pricing model and rate
// config. The string is recorded in the manifest and read by Session 4's
// per-archetype billing assertion.
func expectedBillingFormula(a scenario.TenantArchetype) string {
	rc := a.RateConfig
	switch a.PricingModel {
	case scenario.PricingFlatRate:
		return fmt.Sprintf("flat %.2f USD per period", rc.FlatFeeUSD)
	case scenario.PricingPerUnit:
		if rc.IncludedFreeUnits > 0 {
			return fmt.Sprintf("max(0, units - %d) * %.6f USD", rc.IncludedFreeUnits, rc.PerUnitRateUSD)
		}
		return fmt.Sprintf("units * %.6f USD", rc.PerUnitRateUSD)
	case scenario.PricingPercentage:
		if rc.MinFeeUSD > 0 {
			return fmt.Sprintf("max(%.2f, raw_units * %.4f) USD", rc.MinFeeUSD, rc.PercentageRate)
		}
		return fmt.Sprintf("raw_units * %.4f USD", rc.PercentageRate)
	case scenario.PricingIncludedQuota:
		if rc.BlockSizeUnits > 0 {
			return fmt.Sprintf("max(0, ceil((units - %d) / %d)) * %.6f USD",
				rc.IncludedFreeUnits, rc.BlockSizeUnits, rc.PerUnitRateUSD)
		}
		return fmt.Sprintf("max(0, units - %d) * %.6f USD", rc.IncludedFreeUnits, rc.PerUnitRateUSD)
	case scenario.PricingGraduated:
		return fmt.Sprintf("graduated tiers (%d bands) — staircase pricing", len(rc.GraduatedTiers))
	case scenario.PricingVolumeTiered:
		return fmt.Sprintf("volume tiers (%d bands) — qualifying-band rate * total volume", len(rc.VolumeTiers))
	}
	return string(a.PricingModel)
}
