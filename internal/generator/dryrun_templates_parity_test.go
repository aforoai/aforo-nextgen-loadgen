package generator

import (
	"math/rand"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// TestDryRunStubMatchesRuntimeTemplates locks the invariant that the
// hardcoded per-product-type template list used by seed.fetchMetricTemplates
// in DryRun mode (via seed.DryRunTemplatesForProductType) MUST cover the
// exact same set of EventFields the runtime generator emits. Two separate
// hardcoded descriptor copies drift silently unless something asserts they
// agree — this test is that assertion.
//
// If this fails after adding a new metric to a descriptor, update BOTH:
//   - internal/generator/templates.go — the runtime emitter
//   - internal/seed/metrics.go dryRunTemplatesForProductType — the dry-run stub
//
// The runtime-side widening is separately locked by
// TestTemplatesEmitEveryDescriptorKey.
func TestDryRunStubMatchesRuntimeTemplates(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	for _, pt := range []scenario.ProductType{
		scenario.ProductAPI,
		scenario.ProductAIAgent,
		scenario.ProductMCPServer,
		scenario.ProductAgenticAPI,
	} {
		body := TemplateForProductType(pt)(rng)
		stubs := seed.DryRunTemplatesForProductType(pt)

		// Every EventField the dry-run stub declares MUST exist in the
		// runtime template's emitted map. Otherwise, dry-run manifests
		// name descriptor keys that the runtime templates don't emit —
		// resolveQuantity would fall back to 1 for those metrics at
		// real seed time, silently over-billing.
		for _, s := range stubs {
			if s.EventField == "" {
				continue // COUNT-only template entries omit EventField (rare).
			}
			if _, ok := body[s.EventField]; !ok {
				t.Errorf("%s dry-run stub declares EventField=%q for metric %q, "+
					"but runtime template does not emit that field. "+
					"Update internal/generator/templates.go OR internal/seed/metrics.go "+
					"dryRunTemplatesForProductType to bring them into agreement.",
					pt, s.EventField, s.Name)
			}
		}

		// Reverse direction: not every runtime field needs to appear in
		// the dry-run stub (some fields are metadata-only, not
		// descriptor-named metrics — model, endpoint, method, trace_id,
		// session_id). But every DESCRIPTOR-named field the stub uses
		// SHOULD have a runtime emitter — checked by the forward pass
		// above. No test on the reverse direction.
	}
}
