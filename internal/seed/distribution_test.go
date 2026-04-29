package seed

import (
	"math/rand"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestDistributeStates_ExactProportionalRounding(t *testing.T) {
	tests := []struct {
		name string
		mix  map[scenario.SubscriptionState]float64
		n    int
		want map[scenario.SubscriptionState]int
	}{
		{
			name: "single state at 1.0",
			mix:  map[scenario.SubscriptionState]float64{scenario.StateActive: 1.0},
			n:    20,
			want: map[scenario.SubscriptionState]int{scenario.StateActive: 20},
		},
		{
			name: "60/25/15 across active/cancelled/expired",
			mix: map[scenario.SubscriptionState]float64{
				scenario.StateActive:    0.60,
				scenario.StateCancelled: 0.25,
				scenario.StateExpired:   0.15,
			},
			n: 20,
			want: map[scenario.SubscriptionState]int{
				scenario.StateActive:    12,
				scenario.StateCancelled: 5,
				scenario.StateExpired:   3,
			},
		},
		{
			name: "uneven N — residual to highest weight",
			mix: map[scenario.SubscriptionState]float64{
				scenario.StateActive:   0.5,
				scenario.StateTrialing: 0.3,
				scenario.StatePaused:   0.2,
			},
			n: 7,
			want: map[scenario.SubscriptionState]int{
				scenario.StateActive:   4, // 3 + residual
				scenario.StateTrialing: 2,
				scenario.StatePaused:   1,
			},
		},
		{
			name: "empty mix defaults all to ACTIVE",
			mix:  nil,
			n:    5,
			want: map[scenario.SubscriptionState]int{scenario.StateActive: 5},
		},
		{
			name: "n=0 returns empty",
			mix:  map[scenario.SubscriptionState]float64{scenario.StateActive: 1.0},
			n:    0,
			want: map[scenario.SubscriptionState]int{},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := distributeStates(tc.mix, tc.n)
			counts := map[scenario.SubscriptionState]int{}
			for _, s := range got {
				counts[s]++
			}
			if len(got) != tc.n {
				t.Fatalf("got %d states, want %d", len(got), tc.n)
			}
			for state, want := range tc.want {
				if counts[state] != want {
					t.Errorf("state %s: got %d, want %d (full=%v)", state, counts[state], want, counts)
				}
			}
		})
	}
}

func TestDistributeStates_StaleStatesPresent(t *testing.T) {
	// Critical for stale_keys flow — if scenario asks for 30% CANCELLED and
	// 20% EXPIRED, distribution must yield non-zero counts for both.
	mix := map[scenario.SubscriptionState]float64{
		scenario.StateActive:    0.5,
		scenario.StateCancelled: 0.3,
		scenario.StateExpired:   0.2,
	}
	got := distributeStates(mix, 10)
	counts := map[scenario.SubscriptionState]int{}
	for _, s := range got {
		counts[s]++
	}
	if counts[scenario.StateCancelled] == 0 {
		t.Errorf("CANCELLED state missing — stale_keys flow would have nothing to test")
	}
	if counts[scenario.StateExpired] == 0 {
		t.Errorf("EXPIRED state missing")
	}
	if counts[scenario.StateActive] == 0 {
		t.Errorf("ACTIVE state missing")
	}
}

func TestDistributeStates_DeterministicOrder(t *testing.T) {
	// Same input → same output ordering. Critical for reproducible runs.
	mix := map[scenario.SubscriptionState]float64{
		scenario.StateActive:    0.5,
		scenario.StateCancelled: 0.3,
		scenario.StateExpired:   0.2,
	}
	a := distributeStates(mix, 10)
	b := distributeStates(mix, 10)
	if len(a) != len(b) {
		t.Fatalf("lengths differ: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Errorf("position %d: %s vs %s", i, a[i], b[i])
		}
	}
}

func TestDistributeCurrencies_HandlesEmpty(t *testing.T) {
	got := distributeCurrencies(nil, 5)
	if len(got) != 5 {
		t.Fatalf("got %d, want 5", len(got))
	}
	for _, c := range got {
		if c != "USD" {
			t.Errorf("default currency should be USD, got %s", c)
		}
	}
}

func TestDistributeCurrencies_MultiCurrency(t *testing.T) {
	mix := map[string]float64{"USD": 0.5, "EUR": 0.3, "GBP": 0.2}
	got := distributeCurrencies(mix, 100)
	counts := map[string]int{}
	for _, c := range got {
		counts[c]++
	}
	// Tolerance is exact for n=100 with these weights: 50/30/20.
	if counts["USD"] != 50 || counts["EUR"] != 30 || counts["GBP"] != 20 {
		t.Errorf("got %v, want USD=50 EUR=30 GBP=20", counts)
	}
}

func TestParseDiscountLabel(t *testing.T) {
	tests := []struct {
		in   string
		want *ManifestDiscount
	}{
		{"none", nil},
		{"", nil},
		{"unknown", nil},
		{"pct_10", &ManifestDiscount{Type: "PERCENTAGE", Value: 10}},
		{"pct_25.5", &ManifestDiscount{Type: "PERCENTAGE", Value: 25.5}},
		{"fixed_50", &ManifestDiscount{Type: "FIXED_AMOUNT", Value: 50}},
		{"pct_0", nil},
		{"pct_abc", nil},
	}
	for _, tc := range tests {
		got := parseDiscountLabel(tc.in)
		if !discountEqual(got, tc.want) {
			t.Errorf("%q: got %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestWeightedDraw_DeterministicWithSeed(t *testing.T) {
	mix := map[string]float64{"a": 0.5, "b": 0.3, "c": 0.2}
	rng1 := rand.New(rand.NewSource(42))
	rng2 := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		a := weightedDraw(mix, rng1)
		b := weightedDraw(mix, rng2)
		if a != b {
			t.Fatalf("seed mismatch at iter %d: %s vs %s", i, a, b)
		}
	}
}

func discountEqual(a, b *ManifestDiscount) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Type == b.Type && a.Value == b.Value
}
