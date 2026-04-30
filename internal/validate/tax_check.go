package validate

import (
	"context"
	"math"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/tax"
)

// runTaxMath is Check 13.
//
// Asserts (independent of the live platform): for every (currency, jurisdiction)
// pair in the scenario, the tax engine's result equals subtotal × rate within
// scenario.tax.tolerance_usd.
//
// This is a SHAPE check on the engine the loadgen is configured to use. It
// does NOT compare against a platform-recorded tax_amount — that requires
// running the validator with --include-billing + a live BackendClient. We
// nevertheless surface the engine name + per-pair computed numbers as
// details so a human can spot drift between mock and real engines.
func (v *Validator) runTaxMath(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckTaxMath)
	t := v.in.Scenario.Tax
	if t.Engine == "" || t.Engine == scenario.TaxMock {
		// always-runnable path
	}
	if len(t.Jurisdictions) == 0 && len(t.JurisdictionByCurrency) == 0 && t.DefaultJurisdiction == "" {
		return res.Skip("no jurisdictions configured in scenario.tax")
	}
	engine, err := tax.Build(t)
	if err != nil {
		return res.Fail("build tax engine: %v", err)
	}
	tol := t.ToleranceUSD
	if tol <= 0 {
		tol = 0.01
	}
	res.Set("engine", engine.Name())
	res.Set("tolerance_usd", tol)

	type pair struct {
		Currency     string  `json:"currency"`
		Jurisdiction string  `json:"jurisdiction"`
		Subtotal     float64 `json:"subtotal"`
		ExpectedRate float64 `json:"expected_rate"`
		ComputedTax  float64 `json:"computed_tax"`
		ExpectedTax  float64 `json:"expected_tax"`
		Drift        float64 `json:"drift"`
		Match        bool    `json:"match"`
	}
	rows := []pair{}
	for currency, jur := range t.JurisdictionByCurrency {
		rate, ok := t.Jurisdictions[jur]
		if !ok {
			continue
		}
		req := tax.Request{
			InvoiceID: "validate-probe", SubtotalUSD: 100, Currency: currency,
		}
		resp, err := engine.Calculate(ctx, req)
		expectedTax := tax.MultiplyAndRound(100, rate)
		row := pair{
			Currency: currency, Jurisdiction: jur, Subtotal: 100,
			ExpectedRate: rate, ExpectedTax: expectedTax,
		}
		if err != nil {
			row.Match = false
		} else {
			row.ComputedTax = resp.TaxAmountUSD
			row.Drift = math.Abs(resp.TaxAmountUSD - expectedTax)
			row.Match = row.Drift <= tol
		}
		rows = append(rows, row)
	}
	res.Set("rows", rows)
	mismatch := 0
	for _, r := range rows {
		if !r.Match {
			mismatch++
		}
	}
	if mismatch > 0 {
		return res.Fail("%d/%d (currency, jurisdiction) pairs disagreed with engine compute", mismatch, len(rows))
	}
	if len(rows) == 0 {
		return res.Skip("no scenario.tax.jurisdiction_by_currency entries to verify")
	}
	return res.Pass()
}
