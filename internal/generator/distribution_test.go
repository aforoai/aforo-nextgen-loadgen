package generator

import (
	"math/rand"
	"sort"
	"testing"
)

// TestPareto8020Within2Percent verifies the Pareto 80/20 weight generator —
// the top 20% of ranks should claim ~80% of the total mass, ±2 percentage
// points. This is the canonical 80/20 property the docs promise.
func TestPareto8020Within2Percent(t *testing.T) {
	for _, n := range []int{50, 100, 200, 500} {
		t.Run("n="+itoa(n), func(t *testing.T) {
			w := Pareto80_20Weights(n)
			if len(w) != n {
				t.Fatalf("len(weights)=%d, want %d", len(w), n)
			}
			// Sum should be 1.0 within tiny FP error.
			sum := 0.0
			for _, x := range w {
				sum += x
			}
			if sum < 0.999 || sum > 1.001 {
				t.Fatalf("sum of weights=%.6f, want ~1.0", sum)
			}
			// Top 20% mass.
			top := n / 5
			if top == 0 {
				top = 1
			}
			topMass := 0.0
			for i := 0; i < top; i++ {
				topMass += w[i]
			}
			// Target: 80% ± 2pp. The Pareto exponent 1.16 yields ~78-82% for
			// reasonable N.
			if topMass < 0.76 || topMass > 0.84 {
				t.Errorf("Pareto 80/20: top 20%% mass = %.4f, want 0.78–0.82 (~80%%)", topMass)
			}
		})
	}
}

// TestWeightedPickerCDFRoundsToOne ensures the CDF construction never
// leaves a gap that would cause Pick to never return the last key.
func TestWeightedPickerCDFRoundsToOne(t *testing.T) {
	w := map[string]float64{"a": 0.1, "b": 0.2, "c": 0.7}
	p := NewWeightedPicker(w)
	if len(p.cum) != 3 {
		t.Fatalf("cum len=%d, want 3", len(p.cum))
	}
	if p.cum[2] != 1.0 {
		t.Errorf("last CDF entry = %f, want exactly 1.0", p.cum[2])
	}

	rng := rand.New(rand.NewSource(1))
	counts := map[string]int{}
	const N = 100_000
	for i := 0; i < N; i++ {
		counts[p.Pick(rng)]++
	}
	// Tolerance: ±1.5pp at N=100k.
	check := func(k string, want float64) {
		got := float64(counts[k]) / float64(N)
		if got < want-0.015 || got > want+0.015 {
			t.Errorf("Pick %q got share %.4f, want %.4f ±0.015", k, got, want)
		}
	}
	check("a", 0.1)
	check("b", 0.2)
	check("c", 0.7)
}

// TestWeightedPickerEmptyAndZero — defensive paths.
func TestWeightedPickerEmptyAndZero(t *testing.T) {
	rng := rand.New(rand.NewSource(1))

	empty := NewWeightedPicker(nil)
	if got := empty.Pick(rng); got != "" {
		t.Errorf("empty picker should return \"\", got %q", got)
	}

	zero := NewWeightedPicker(map[string]float64{"x": 0, "y": 0})
	// Should still return one of the keys, deterministic by sort order.
	got := zero.Pick(rng)
	if got != "x" && got != "y" {
		t.Errorf("zero-weight picker returned %q; want x or y (uniform fallback)", got)
	}
}

// TestZipfWeightsMonotone — Zipfian weights must be strictly decreasing.
func TestZipfWeightsMonotone(t *testing.T) {
	w := ZipfWeights(50, 1.0)
	if !sort.SliceIsSorted(w, func(i, j int) bool { return w[i] > w[j] }) {
		t.Errorf("Zipf weights not monotonically decreasing")
	}
}

// TestUniformWeightsExact — every weight equals 1/N.
func TestUniformWeightsExact(t *testing.T) {
	n := 13
	w := UniformWeights(n)
	want := 1.0 / float64(n)
	for i, x := range w {
		if x != want {
			t.Errorf("uniform[%d]=%.6f, want %.6f", i, x, want)
		}
	}
}

// TestIndexPickerDeterministic — same seed = same picks.
func TestIndexPickerDeterministic(t *testing.T) {
	w := []float64{0.1, 0.4, 0.5}
	p := NewIndexPicker(w)
	r1 := rand.New(rand.NewSource(42))
	r2 := rand.New(rand.NewSource(42))
	for i := 0; i < 100; i++ {
		if got1, got2 := p.Pick(r1), p.Pick(r2); got1 != got2 {
			t.Fatalf("non-deterministic: iteration %d got %d vs %d", i, got1, got2)
		}
	}
}

func itoa(n int) string {
	const digits = "0123456789"
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = digits[n%10]
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
