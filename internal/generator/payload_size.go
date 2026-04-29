package generator

import (
	"math/rand"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// PayloadSize labels the size class chosen for an event. Carried in the
// Event struct so metrics can be sliced by size class.
type PayloadSize string

const (
	PayloadSmall  PayloadSize = "small"  // ~200 bytes
	PayloadMedium PayloadSize = "medium" // ~2 KiB
	PayloadLarge  PayloadSize = "large"  // ~20 KiB nested
)

// PayloadSizePicker chooses one of the three size classes per scenario weights.
type PayloadSizePicker struct {
	picker WeightedPicker
}

// NewPayloadSizePicker constructs a picker from the scenario configuration.
// If all three weights are zero, defaults to 100% small.
func NewPayloadSizePicker(p scenario.PayloadVariation) PayloadSizePicker {
	weights := map[string]float64{
		string(PayloadSmall):  p.SmallPct,
		string(PayloadMedium): p.MediumPct,
		string(PayloadLarge):  p.LargePct,
	}
	if p.Sum() <= 0 {
		weights[string(PayloadSmall)] = 1.0
	}
	return PayloadSizePicker{picker: NewWeightedPicker(weights)}
}

// Pick returns one of small/medium/large.
func (p PayloadSizePicker) Pick(rng *rand.Rand) PayloadSize {
	v := p.picker.Pick(rng)
	if v == "" {
		return PayloadSmall
	}
	return PayloadSize(v)
}

// ApplyPayloadSize pads the event body with deterministic noise to bring its
// size up to the target class. Keeps the originally-generated fields intact;
// adds a `_pad` field with the right amount of bytes.
//
// Sizes are approximate to JSON-encoded bytes; the actual marshaled size will
// differ by a few percent due to encoding overhead.
func ApplyPayloadSize(body map[string]any, size PayloadSize, rng *rand.Rand) {
	if body == nil {
		return
	}
	switch size {
	case PayloadSmall:
		// No-op — natural template body is ~200B.
	case PayloadMedium:
		body["_pad"] = padString(rng, 1900)
	case PayloadLarge:
		body["_pad"] = padNested(rng, 18000)
	}
}

// padString returns an approximately-N-byte deterministic string. Random
// bytes from rng kept printable so JSON encoding doesn't blow up the size.
func padString(rng *rand.Rand, n int) string {
	if n <= 0 {
		return ""
	}
	const alphabet = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = alphabet[rng.Intn(len(alphabet))]
	}
	return string(b)
}

// padNested returns a nested map structure totaling approximately N bytes
// when JSON-encoded — closer to "real" oversized payloads with structure
// rather than a flat string. Used for the "large" size class.
func padNested(rng *rand.Rand, totalBytes int) map[string]any {
	if totalBytes <= 0 {
		return map[string]any{}
	}
	// Roughly 10 keys, each carrying its share of bytes.
	keys := 10
	per := totalBytes / keys
	out := make(map[string]any, keys)
	for i := 0; i < keys; i++ {
		out["k"+padString(rng, 4)] = padString(rng, per)
	}
	return out
}
