package generator

import (
	"encoding/json"
	"math/rand"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// TestPayloadSizePickerMatchesScenarioPercentages — sample 10k payloads
// against a 50/30/20 mix; observed shares must be within ±2pp.
func TestPayloadSizePickerMatchesScenarioPercentages(t *testing.T) {
	cfg := scenario.PayloadVariation{SmallPct: 0.5, MediumPct: 0.3, LargePct: 0.2}
	p := NewPayloadSizePicker(cfg)

	rng := rand.New(rand.NewSource(7))
	counts := map[PayloadSize]int{}
	const N = 10_000
	for i := 0; i < N; i++ {
		counts[p.Pick(rng)]++
	}
	check := func(s PayloadSize, want float64) {
		got := float64(counts[s]) / float64(N)
		if got < want-0.02 || got > want+0.02 {
			t.Errorf("payload %s share %.4f, want %.4f ±0.02", s, got, want)
		}
	}
	check(PayloadSmall, 0.5)
	check(PayloadMedium, 0.3)
	check(PayloadLarge, 0.2)
}

// TestApplyPayloadSizeBytes — small unchanged, medium ~2KB, large ~20KB
// when JSON-encoded. Bounds are loose because the templates contribute
// their own bytes; we only check the padder's contribution lands in range.
func TestApplyPayloadSizeBytes(t *testing.T) {
	rng := rand.New(rand.NewSource(7))

	for _, tc := range []struct {
		size    PayloadSize
		minSize int
		maxSize int
	}{
		{PayloadSmall, 0, 500},
		{PayloadMedium, 1500, 3000},
		{PayloadLarge, 17_000, 25_000},
	} {
		body := apiTemplate(rng)
		ApplyPayloadSize(body, tc.size, rng)
		buf, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		if len(buf) < tc.minSize || len(buf) > tc.maxSize {
			t.Errorf("size=%s: encoded %d bytes, want [%d, %d]", tc.size, len(buf), tc.minSize, tc.maxSize)
		}
	}
}

// TestPayloadSizeDefaultsToSmall — empty config yields all-small.
func TestPayloadSizeDefaultsToSmall(t *testing.T) {
	p := NewPayloadSizePicker(scenario.PayloadVariation{})
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 100; i++ {
		if got := p.Pick(rng); got != PayloadSmall {
			t.Fatalf("default picker returned %q at iteration %d, want small", got, i)
		}
	}
}
