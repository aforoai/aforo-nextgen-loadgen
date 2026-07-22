package seed

import (
	"math/rand"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestAllocateTenants_LargestResidual(t *testing.T) {
	s := &scenario.Scenario{
		Tenants: scenario.Tenants{
			Count: 10,
			Archetypes: []scenario.TenantArchetype{
				{Name: "a", Weight: 0.34},
				{Name: "b", Weight: 0.33},
				{Name: "c", Weight: 0.33},
			},
		},
	}
	allocs := AllocateTenants(s)
	total := 0
	for _, a := range allocs {
		total += a.Count
	}
	if total != 10 {
		t.Fatalf("sum of allocations = %d, want 10", total)
	}
	// Sorted by name → a, b, c.
	if allocs[0].Archetype.Name != "a" || allocs[1].Archetype.Name != "b" || allocs[2].Archetype.Name != "c" {
		t.Errorf("not sorted: %v", []string{allocs[0].Archetype.Name, allocs[1].Archetype.Name, allocs[2].Archetype.Name})
	}
	// Highest weight gets the residual.
	if allocs[0].Count != 4 {
		t.Errorf("largest residual not assigned: a=%d (want 4)", allocs[0].Count)
	}
}

func TestAllocateTenants_30ArchetypesMatchesScenario(t *testing.T) {
	// Smoke test against matrix-billing's 30-archetype shape: weights of
	// 0.033 and 0.034 (10x at 0.034 + 20x at 0.033 = 1.000), N=90.
	arches := make([]scenario.TenantArchetype, 30)
	for i := 0; i < 30; i++ {
		w := 0.033
		if i < 10 {
			w = 0.034
		}
		arches[i] = scenario.TenantArchetype{Name: archetypeName(i), Weight: w}
	}
	s := &scenario.Scenario{Tenants: scenario.Tenants{Count: 90, Archetypes: arches}}
	allocs := AllocateTenants(s)
	total := 0
	for _, a := range allocs {
		total += a.Count
	}
	if total != 90 {
		t.Fatalf("sum = %d, want 90", total)
	}
	for _, a := range allocs {
		if a.Count != 3 {
			t.Errorf("archetype %s: count=%d (want 3)", a.Archetype.Name, a.Count)
		}
	}
}

func TestFilterArchetypes(t *testing.T) {
	allocs := []ArchetypeAllocation{
		{Archetype: scenario.TenantArchetype{Name: "a"}, Count: 5},
		{Archetype: scenario.TenantArchetype{Name: "b"}, Count: 3},
		{Archetype: scenario.TenantArchetype{Name: "c"}, Count: 2},
	}
	got := FilterArchetypes(allocs, []string{"a", "c"})
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	if got[0].Archetype.Name != "a" || got[1].Archetype.Name != "c" {
		t.Errorf("got %v", got)
	}

	// Empty include → unchanged.
	if len(FilterArchetypes(allocs, nil)) != 3 {
		t.Errorf("empty filter should be unchanged")
	}
}

func TestPlanArchetype_DistributesCustomers(t *testing.T) {
	a := scenario.TenantArchetype{
		CustomerCount: 20,
		CurrencyMix:   map[string]float64{"USD": 0.6, "EUR": 0.4},
		SubscriptionStateMix: map[scenario.SubscriptionState]float64{
			scenario.StateActive:    0.5,
			scenario.StateCancelled: 0.3,
			scenario.StateExpired:   0.2,
		},
	}
	rng := rand.New(rand.NewSource(42))
	plan := planArchetype(a, rng)
	if len(plan.Customers) != 20 {
		t.Fatalf("got %d plans, want 20", len(plan.Customers))
	}

	// Currency tally should match the exact rounding (12 USD + 8 EUR for n=20).
	var usd, eur int
	var active, cancelled, expired int
	for _, p := range plan.Customers {
		switch p.Currency {
		case "USD":
			usd++
		case "EUR":
			eur++
		}
		switch p.State {
		case scenario.StateActive:
			active++
		case scenario.StateCancelled:
			cancelled++
		case scenario.StateExpired:
			expired++
		}
	}
	if usd != 12 || eur != 8 {
		t.Errorf("currency split: USD=%d EUR=%d, want 12/8", usd, eur)
	}
	// State counts should match exact rounding (10/6/4).
	if active != 10 || cancelled != 6 || expired != 4 {
		t.Errorf("state split: ACTIVE=%d CANCELLED=%d EXPIRED=%d, want 10/6/4",
			active, cancelled, expired)
	}
}

// TestPlanArchetype_DistributesAcrossRateCards asserts that the v2
// RateCards path assigns customers to cards proportional to CustomerShare.
// Exact rounding at n=100 with shares 0.6/0.3/0.1 → 60/30/10 buckets.
func TestPlanArchetype_DistributesAcrossRateCards(t *testing.T) {
	a := scenario.TenantArchetype{
		CustomerCount: 100,
		CurrencyMix:   map[string]float64{"USD": 1.0},
		SubscriptionStateMix: map[scenario.SubscriptionState]float64{
			scenario.StateActive: 1.0,
		},
		RateCards: []scenario.RateCardSpec{
			{Name: "starter", CustomerShare: 0.6, PricingModel: scenario.PricingPerUnit},
			{Name: "pro", CustomerShare: 0.3, PricingModel: scenario.PricingPerUnit},
			{Name: "enterprise", CustomerShare: 0.1, PricingModel: scenario.PricingFlatRate},
		},
	}
	rng := rand.New(rand.NewSource(42))
	plan := planArchetype(a, rng)
	counts := map[int]int{}
	for _, cp := range plan.Customers {
		counts[cp.CardIndex]++
	}
	if counts[0] != 60 || counts[1] != 30 || counts[2] != 10 {
		t.Errorf("card distribution: starter=%d pro=%d enterprise=%d, want 60/30/10",
			counts[0], counts[1], counts[2])
	}
}

// TestPlanArchetype_SingleCard_AllZero asserts the v1 back-compat path:
// when RateCards has exactly one entry (backfilled from legacy scalars),
// every customer gets CardIndex=0. This is the invariant seeder.go's
// customer loop assumes when it reads a.RateCards[cp.CardIndex].
func TestPlanArchetype_SingleCard_AllZero(t *testing.T) {
	a := scenario.TenantArchetype{
		CustomerCount:        7,
		CurrencyMix:          map[string]float64{"USD": 1.0},
		SubscriptionStateMix: map[scenario.SubscriptionState]float64{scenario.StateActive: 1.0},
		RateCards: []scenario.RateCardSpec{
			{Name: "default", CustomerShare: 1.0, PricingModel: scenario.PricingPerUnit},
		},
	}
	rng := rand.New(rand.NewSource(1))
	plan := planArchetype(a, rng)
	for i, cp := range plan.Customers {
		if cp.CardIndex != 0 {
			t.Errorf("customer[%d].CardIndex = %d, want 0 (single-card back-compat)", i, cp.CardIndex)
		}
	}
}

func TestExpectedBillingFormula(t *testing.T) {
	tests := []struct {
		name string
		a    scenario.TenantArchetype
		want string
	}{
		{
			name: "FLAT_RATE",
			a: scenario.TenantArchetype{
				PricingModel: scenario.PricingFlatRate,
				RateConfig:   scenario.RateConfig{FlatFeeUSD: 99.0},
			},
			want: "flat 99.00 USD per period",
		},
		{
			name: "PER_UNIT",
			a: scenario.TenantArchetype{
				PricingModel: scenario.PricingPerUnit,
				RateConfig:   scenario.RateConfig{PerUnitRateUSD: 0.001},
			},
			want: "units * 0.001000 USD",
		},
		{
			name: "INCLUDED_QUOTA with block size",
			a: scenario.TenantArchetype{
				PricingModel: scenario.PricingIncludedQuota,
				RateConfig:   scenario.RateConfig{IncludedFreeUnits: 10000, BlockSizeUnits: 100, PerUnitRateUSD: 0.001},
			},
			want: "max(0, ceil((units - 10000) / 100)) * 0.001000 USD",
		},
		{
			name: "GRADUATED",
			a: scenario.TenantArchetype{
				PricingModel: scenario.PricingGraduated,
				RateConfig: scenario.RateConfig{
					GraduatedTiers: []scenario.TierBand{{}, {}, {}},
				},
			},
			want: "graduated tiers (3 bands) — staircase pricing",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := expectedBillingFormula(tc.a)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func archetypeName(i int) string {
	names := "abcdefghijklmnopqrstuvwxyzABCD"
	if i < 0 || i >= len(names) {
		return "x"
	}
	return string(names[i])
}
