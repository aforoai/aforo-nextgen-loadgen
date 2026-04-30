package tax

import (
	"context"
	"fmt"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// MockEngine computes tax deterministically from a scenario's tax block —
// no network calls, no env vars, no flakes. Default in CI.
//
// Resolution: caller-supplied req.Jurisdiction wins; otherwise scenario's
// JurisdictionByCurrency lookup; otherwise scenario.DefaultJurisdiction.
// If still empty, returns Response{Rate: 0, TaxAmountUSD: 0, Note:
// "no jurisdiction matched"} — the validator treats zero as "not asserted".
type MockEngine struct {
	jurisdictions          map[string]float64
	jurisdictionByCurrency map[string]string
	defaultJurisdiction    string
}

// NewMockEngine constructs a MockEngine from a scenario.Tax block.
func NewMockEngine(t scenario.Tax) *MockEngine {
	jurs := make(map[string]float64, len(t.Jurisdictions))
	for k, v := range t.Jurisdictions {
		jurs[k] = v
	}
	byCur := make(map[string]string, len(t.JurisdictionByCurrency))
	for k, v := range t.JurisdictionByCurrency {
		byCur[k] = v
	}
	return &MockEngine{
		jurisdictions:          jurs,
		jurisdictionByCurrency: byCur,
		defaultJurisdiction:    t.DefaultJurisdiction,
	}
}

// Name returns the engine's id — "mock". Stable string used in logs.
func (m *MockEngine) Name() string { return "mock" }

// Calculate returns the deterministic tax line.
func (m *MockEngine) Calculate(_ context.Context, req Request) (Response, error) {
	if err := validateRequest(req); err != nil {
		return Response{}, err
	}
	jurisdiction := Resolve(req, m.jurisdictionByCurrency, m.defaultJurisdiction)
	if jurisdiction == "" {
		return Response{Engine: m.Name(), Note: "no jurisdiction matched"}, nil
	}
	rate, ok := m.jurisdictions[jurisdiction]
	if !ok {
		return Response{Engine: m.Name(), JurisdictionCode: jurisdiction,
			Note: fmt.Sprintf("jurisdiction %q absent from tax.jurisdictions", jurisdiction),
		}, nil
	}
	return Response{
		JurisdictionCode: jurisdiction,
		Rate:             rate,
		TaxAmountUSD:     MultiplyAndRound(req.SubtotalUSD, rate),
		Engine:           m.Name(),
	}, nil
}
