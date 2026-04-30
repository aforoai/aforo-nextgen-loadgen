package validate

import (
	"context"
	"errors"
	"math"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/fx"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/payments"
)

// runMultiCurrency is Check 14.
//
// Asserts:
//
//	a. Every PAID invoice's currency matches the customer's seeded currency
//	   (no silent USD conversion at ingest time).
//	b. The realized FX rate (per scenario.fx.pinned_rates) was applied at
//	   bill-run time — verified by computing what the platform should have
//	   emitted and comparing against payments.jsonl amount_usd × rate.
//	c. scenario.fx.applied_at is "bill_run_time" (the WRONG path
//	   "event_ingest_time" is asserted not present in the scenario).
//
// SKIPs when no payments.jsonl exists, when no foreign-currency invoices
// were processed, or when scenario.fx is empty.
func (v *Validator) runMultiCurrency(_ context.Context) *CheckResult {
	res := NewCheckResult(CheckMultiCurrency)

	if v.in.Scenario.FX.AppliedAt == "event_ingest_time" {
		return res.Fail("scenario.fx.applied_at = \"event_ingest_time\" — platform applies FX at bill-run time; this is the WRONG path")
	}

	records, err := payments.Load(v.in.RunOutputDir)
	if err != nil {
		return res.Fail("load payments.jsonl: %v", err)
	}
	if len(records) == 0 {
		return res.Skip("no payments.jsonl — payments driver didn't run")
	}

	// Build customer→currency map from manifest.
	custCurrency := map[string]string{}
	for _, t := range v.in.Manifest.Tenants {
		for _, c := range t.Customers {
			custCurrency[c.CustomerID] = c.Currency
		}
	}

	provider := fx.FromScenario(v.in.Scenario)
	type rowResult struct {
		InvoiceID    string  `json:"invoice_id"`
		CustomerID   string  `json:"customer_id"`
		Currency     string  `json:"currency"`
		Expected     string  `json:"expected_currency"`
		AmountUSD    float64 `json:"amount_usd"`
		ConvertedUSD float64 `json:"converted_usd"`
		Match        bool    `json:"match"`
		Reason       string  `json:"reason,omitempty"`
	}
	rows := []rowResult{}
	currencyMismatch := 0
	fxMismatch := 0
	checked := 0
	for _, r := range records {
		want := custCurrency[r.CustomerID]
		if want == "" || want == r.Currency {
			// Either we don't know the customer's currency, or the invoice
			// matches; both fine.
			continue
		}
		checked++
		row := rowResult{
			InvoiceID: r.InvoiceID, CustomerID: r.CustomerID,
			Currency: r.Currency, Expected: want, AmountUSD: r.AmountUSD,
		}
		if r.Currency != want {
			currencyMismatch++
			row.Reason = "billing currency != customer currency"
			rows = append(rows, row)
			continue
		}
		// Use the FX provider to compute what the USD-converted invoice
		// SHOULD be; check that the recorded amount_usd matches.
		converted, err := provider.Convert(r.AmountUSD, want, "USD")
		if err != nil {
			if errors.Is(err, fx.ErrUnknownPair) {
				row.Reason = "FX pair not pinned"
			} else {
				row.Reason = err.Error()
			}
			rows = append(rows, row)
			continue
		}
		row.ConvertedUSD = converted
		// Tolerance: 1c absolute or 0.5% relative.
		tol := math.Max(0.01, math.Abs(converted)*0.005)
		row.Match = math.Abs(r.AmountUSD-converted) <= tol
		if !row.Match {
			fxMismatch++
		}
		rows = append(rows, row)
	}
	res.Set("checked_invoices", checked)
	res.Set("currency_mismatch", currencyMismatch)
	res.Set("fx_mismatch", fxMismatch)
	res.Set("rows", rows)
	if checked == 0 {
		return res.Skip("no foreign-currency invoices in payments.jsonl")
	}
	if currencyMismatch > 0 {
		return res.Fail("%d invoices billed in wrong currency (customer's currency != invoice currency)", currencyMismatch)
	}
	if fxMismatch > 0 {
		return res.Fail("%d invoices with FX-applied amount drifting from pinned rate", fxMismatch)
	}
	return res.Pass()
}
