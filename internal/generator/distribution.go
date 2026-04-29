// Package generator builds the event stream a load run sends to the platform.
//
// Distribution sampling is the foundation: tenant-traffic shaping (Pareto,
// Zipf, Uniform), product-mix selection, ingestion-path selection, and
// payload-size selection all share the same weighted-categorical machinery.
//
// All samplers take an explicit *rand.Rand so the generator stays deterministic
// for a given scenario seed — the entire run replays identically given the
// same scenario + manifest + seed.
package generator

import (
	"math"
	"math/rand"
	"sort"
)

// PicksFromWeights builds a categorical sampler from a weight map.
// Keys are sorted before normalization so the same map produces the same
// CDF regardless of Go's randomized map iteration order.
//
// Empty input returns a zero-keys picker; Pick returns "" in that case.
type WeightedPicker struct {
	keys []string
	cum  []float64 // cumulative distribution, last entry == 1.0
}

// NewWeightedPicker constructs a sampler from string-keyed weights.
// Negative weights are treated as zero. Total weight 0 → uniform fallback.
func NewWeightedPicker(weights map[string]float64) WeightedPicker {
	if len(weights) == 0 {
		return WeightedPicker{}
	}
	keys := make([]string, 0, len(weights))
	for k := range weights {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	total := 0.0
	for _, k := range keys {
		w := weights[k]
		if w < 0 {
			w = 0
		}
		total += w
	}
	cum := make([]float64, len(keys))
	if total <= 0 {
		// Uniform fallback so Pick returns something deterministic.
		step := 1.0 / float64(len(keys))
		for i := range cum {
			cum[i] = step * float64(i+1)
		}
		return WeightedPicker{keys: keys, cum: cum}
	}
	running := 0.0
	for i, k := range keys {
		w := weights[k]
		if w < 0 {
			w = 0
		}
		running += w / total
		cum[i] = running
	}
	// Force last entry to exactly 1.0 — guards against FP drift on long runs.
	cum[len(cum)-1] = 1.0
	return WeightedPicker{keys: keys, cum: cum}
}

// Pick samples one key. Empty picker returns "".
func (p WeightedPicker) Pick(rng *rand.Rand) string {
	if len(p.keys) == 0 {
		return ""
	}
	r := rng.Float64()
	// Binary search the CDF — O(log N) per pick keeps ProductMix and
	// IngestionPaths cheap when traffic is shaped across many channels.
	idx := sort.SearchFloat64s(p.cum, r)
	if idx >= len(p.keys) {
		idx = len(p.keys) - 1
	}
	return p.keys[idx]
}

// Len reports the number of keys in the picker.
func (p WeightedPicker) Len() int { return len(p.keys) }

// Pareto80_20Weights returns a deterministic weight array for N ranks so
// that the FIRST 20% of indices claim ~80% of total mass — the classic
// Pareto principle.
//
// Implementation: weights are w_i = 1/(rank+1)^alpha, with alpha chosen by
// numerical bisection per N so the top-20% mass is exactly 0.80 ± 1e-3.
// A fixed alpha (e.g. 1.16, the continuous-Pareto inverse) drifts off
// 80/20 by 5–10 pp at small N — bisection makes it tight at every N.
//
// For N ≤ 4 the rule is meaningless (top 20% is one index). We fall back
// to uniform weights — the caller's tests should also handle that case.
//
// Returns the per-index weight array, normalized to sum to 1.0. The
// CALLER assigns the permutation: which tenant goes in which rank.
func Pareto80_20Weights(n int) []float64 {
	if n <= 0 {
		return nil
	}
	if n < 5 {
		// Too few items for a meaningful 80/20 split.
		return UniformWeights(n)
	}
	top := n / 5
	if top < 1 {
		top = 1
	}
	const targetMass = 0.80
	lo, hi := 0.0, 6.0
	for iter := 0; iter < 60; iter++ {
		mid := (lo + hi) / 2
		mass := paretoTopMass(n, top, mid)
		if mass < targetMass {
			lo = mid // need more concentration → higher alpha
		} else {
			hi = mid
		}
		if hi-lo < 1e-7 {
			break
		}
	}
	alpha := (lo + hi) / 2
	return paretoWeights(n, alpha)
}

// paretoWeights builds normalized w_i = 1/(rank+1)^alpha for N ranks.
func paretoWeights(n int, alpha float64) []float64 {
	w := make([]float64, n)
	total := 0.0
	for i := 0; i < n; i++ {
		w[i] = 1.0 / math.Pow(float64(i+1), alpha)
		total += w[i]
	}
	if total <= 0 {
		// Degenerate — fall back to uniform.
		return UniformWeights(n)
	}
	for i := range w {
		w[i] /= total
	}
	// Pin the last entry so cumulative sums are exact.
	return w
}

// paretoTopMass returns the share held by the first `top` ranks under the
// Pareto distribution with exponent alpha. Used by the bisection above.
func paretoTopMass(n, top int, alpha float64) float64 {
	total := 0.0
	topSum := 0.0
	for i := 0; i < n; i++ {
		w := 1.0 / math.Pow(float64(i+1), alpha)
		total += w
		if i < top {
			topSum += w
		}
	}
	if total <= 0 {
		return 0
	}
	return topSum / total
}

// ZipfWeights returns Zipfian weights for N ranks: w_i = 1/(rank+1)^s.
// s=1.0 is classic Zipf (frequency proportional to 1/rank).
func ZipfWeights(n int, s float64) []float64 {
	if n <= 0 {
		return nil
	}
	if s <= 0 {
		s = 1.0
	}
	w := make([]float64, n)
	total := 0.0
	for i := 0; i < n; i++ {
		w[i] = 1.0 / math.Pow(float64(i+1), s)
		total += w[i]
	}
	for i := range w {
		w[i] /= total
	}
	return w
}

// UniformWeights returns 1/N for each of N ranks.
func UniformWeights(n int) []float64 {
	if n <= 0 {
		return nil
	}
	w := make([]float64, n)
	step := 1.0 / float64(n)
	for i := range w {
		w[i] = step
	}
	return w
}

// IndexPicker samples integer indices [0,N) by a precomputed weight array.
// This avoids the string-key allocation of WeightedPicker for hot paths
// like per-event tenant selection.
type IndexPicker struct {
	cum []float64
}

// NewIndexPicker builds a picker over weights. Negative weights → 0.
// Total 0 → uniform fallback.
func NewIndexPicker(weights []float64) IndexPicker {
	if len(weights) == 0 {
		return IndexPicker{}
	}
	clean := make([]float64, len(weights))
	total := 0.0
	for i, w := range weights {
		if w < 0 {
			w = 0
		}
		clean[i] = w
		total += w
	}
	cum := make([]float64, len(clean))
	if total <= 0 {
		step := 1.0 / float64(len(clean))
		for i := range cum {
			cum[i] = step * float64(i+1)
		}
		return IndexPicker{cum: cum}
	}
	running := 0.0
	for i, w := range clean {
		running += w / total
		cum[i] = running
	}
	cum[len(cum)-1] = 1.0
	return IndexPicker{cum: cum}
}

// Pick samples one index. Empty picker returns -1.
func (p IndexPicker) Pick(rng *rand.Rand) int {
	if len(p.cum) == 0 {
		return -1
	}
	r := rng.Float64()
	idx := sort.SearchFloat64s(p.cum, r)
	if idx >= len(p.cum) {
		idx = len(p.cum) - 1
	}
	return idx
}

// Len reports the number of weighted slots.
func (p IndexPicker) Len() int { return len(p.cum) }
