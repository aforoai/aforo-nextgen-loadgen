package tax

import (
	"fmt"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// Build returns the Engine selected by scenario.tax.engine. The engine name
// flag (--tax-engine) overrides scenario.tax.engine when non-empty — the CLI
// applies this before calling Build.
//
// "" → mock by default.
func Build(t scenario.Tax) (Engine, error) {
	switch t.Engine {
	case "", scenario.TaxMock:
		return NewMockEngine(t), nil
	case scenario.TaxAvalara:
		return NewAvalaraEngine(t), nil
	case scenario.TaxVertex:
		return NewVertexEngine(t), nil
	default:
		return nil, fmt.Errorf("tax: unknown engine %q (want mock|avalara|vertex)", t.Engine)
	}
}
