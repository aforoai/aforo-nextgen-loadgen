package seed

import (
	"math/rand"
	"sort"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// weightedDraw is a deterministic categorical draw from a string→weight map
// using rng. Empty input returns "" (caller's responsibility to default).
//
// Keys are sorted before sampling so the same scenario + same seed produces
// the same draw, even though Go map iteration is randomized.
func weightedDraw(weights map[string]float64, rng *rand.Rand) string {
	if len(weights) == 0 {
		return ""
	}
	keys := sortedKeys(weights)
	total := 0.0
	for _, k := range keys {
		total += weights[k]
	}
	if total <= 0 {
		return keys[0]
	}
	r := rng.Float64() * total
	cum := 0.0
	for _, k := range keys {
		cum += weights[k]
		if r <= cum {
			return k
		}
	}
	return keys[len(keys)-1]
}

func sortedKeys(m map[string]float64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// distributeStates expands a state-mix into N concrete states. Counts are
// derived from the weights and rounded; the residual goes to the highest-weight
// state to preserve sum=N.
//
// The returned slice is in deterministic order (sorted by state name) so that
// integration tests asserting "M out of N are CANCELLED" stay stable.
func distributeStates(mix map[scenario.SubscriptionState]float64, n int) []scenario.SubscriptionState {
	if n <= 0 {
		return nil
	}
	if len(mix) == 0 {
		out := make([]scenario.SubscriptionState, n)
		for i := range out {
			out[i] = scenario.StateActive
		}
		return out
	}

	// Convert to a sorted slice of (state, weight) so output is deterministic.
	type kv struct {
		state  scenario.SubscriptionState
		weight float64
	}
	pairs := make([]kv, 0, len(mix))
	for s, w := range mix {
		pairs = append(pairs, kv{s, w})
	}
	sort.Slice(pairs, func(i, j int) bool { return string(pairs[i].state) < string(pairs[j].state) })

	total := 0.0
	for _, p := range pairs {
		total += p.weight
	}
	if total <= 0 {
		out := make([]scenario.SubscriptionState, n)
		for i := range out {
			out[i] = scenario.StateActive
		}
		return out
	}

	counts := make([]int, len(pairs))
	allocated := 0
	for i, p := range pairs {
		counts[i] = int((p.weight / total) * float64(n))
		allocated += counts[i]
	}
	// Distribute the residual to the highest-weight state. If multiple share
	// the max, we hand the leftovers out left-to-right (sorted order, so
	// reproducible) until the residual is consumed.
	for allocated < n {
		maxIdx := 0
		for i, p := range pairs {
			if p.weight > pairs[maxIdx].weight {
				maxIdx = i
			}
		}
		counts[maxIdx]++
		allocated++
	}

	out := make([]scenario.SubscriptionState, 0, n)
	for i, p := range pairs {
		for j := 0; j < counts[i]; j++ {
			out = append(out, p.state)
		}
	}
	return out
}

// distributeCurrencies returns N currency codes drawn from the mix. Uses the
// same proportional-rounding as distributeStates so weighted shares are exact
// to within ±1 per category for any N.
func distributeCurrencies(mix map[string]float64, n int) []string {
	if n <= 0 {
		return nil
	}
	if len(mix) == 0 {
		out := make([]string, n)
		for i := range out {
			out[i] = "USD"
		}
		return out
	}
	keys := sortedKeys(mix)
	total := 0.0
	for _, k := range keys {
		total += mix[k]
	}
	counts := make(map[string]int, len(keys))
	allocated := 0
	for _, k := range keys {
		counts[k] = int((mix[k] / total) * float64(n))
		allocated += counts[k]
	}
	// Hand residual to the largest-weight key (deterministic via sortedKeys).
	for allocated < n {
		maxKey := keys[0]
		for _, k := range keys {
			if mix[k] > mix[maxKey] {
				maxKey = k
			}
		}
		counts[maxKey]++
		allocated++
	}

	out := make([]string, 0, n)
	for _, k := range keys {
		for j := 0; j < counts[k]; j++ {
			out = append(out, k)
		}
	}
	return out
}

// drawDiscount picks one of the discount labels in the mix. "none" maps to nil
// in the manifest. Labels follow scenario convention: "none", "pct_<N>",
// "fixed_<N>". Returns nil for unrecognized labels.
func drawDiscount(mix map[string]float64, rng *rand.Rand) *ManifestDiscount {
	if len(mix) == 0 {
		return nil
	}
	label := weightedDraw(mix, rng)
	return parseDiscountLabel(label)
}

// parseDiscountLabel decodes "pct_10" → {Type: PERCENTAGE, Value: 10},
// "fixed_50" → {Type: FIXED_AMOUNT, Value: 50}, anything else → nil.
func parseDiscountLabel(label string) *ManifestDiscount {
	switch {
	case label == "" || label == "none":
		return nil
	case strings.HasPrefix(label, "pct_"):
		v := parseFloatTrailing(label[len("pct_"):])
		if v <= 0 {
			return nil
		}
		return &ManifestDiscount{Type: "PERCENTAGE", Value: v}
	case strings.HasPrefix(label, "fixed_"):
		v := parseFloatTrailing(label[len("fixed_"):])
		if v <= 0 {
			return nil
		}
		return &ManifestDiscount{Type: "FIXED_AMOUNT", Value: v}
	default:
		return nil
	}
}

func parseFloatTrailing(s string) float64 {
	// Hand-rolled to avoid an import for a one-line job. Only "<digits>" or
	// "<digits>.<digits>" is recognized; anything else returns 0.
	v := 0.0
	frac := 0.0
	scale := 1.0
	dot := false
	for _, r := range s {
		if r == '.' && !dot {
			dot = true
			continue
		}
		if r < '0' || r > '9' {
			return 0
		}
		d := float64(r - '0')
		if dot {
			scale *= 10
			frac = frac*10 + d
		} else {
			v = v*10 + d
		}
	}
	return v + frac/scale
}
