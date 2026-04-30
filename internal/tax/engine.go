// Package tax computes the tax line item the platform's TaxCalculationService
// is expected to attach to an invoice, in three flavors:
//
//	mock     — deterministic local computation from scenario.tax.jurisdictions.
//	         Default in CI; never makes a network call.
//
//	avalara  — real Avalara AvaTax v2 client (uses AVATAX_ACCOUNT_ID +
//	         AVATAX_LICENSE_KEY env vars; falls back to mock if unset).
//
//	vertex   — real Vertex O Series client (uses VERTEX_USER + VERTEX_PWD;
//	         falls back to mock if unset).
//
// Engines all satisfy the Engine interface so the validator's tax check
// (Check 13) can call .Calculate(invoice) and compare against the platform's
// recorded tax_amount + jurisdiction_code.
//
// Floating-point math: tax is computed at 6dp, callers compare with the
// scenario.tax.tolerance_usd absolute tolerance (default $0.01 = one cent).
package tax

import (
	"context"
	"fmt"
	"math"
	"strings"
)

// Request is everything the engine needs to compute a tax line. Matches the
// shape the platform's TaxCalculationService.calculate(...) accepts: an
// invoice subtotal in a currency, plus context (customer's jurisdiction,
// product type, line items if available).
type Request struct {
	InvoiceID    string
	TenantID     string
	CustomerID   string
	SubtotalUSD  float64 // ALWAYS in USD — caller converts via fx package first
	Currency     string  // billing currency (USD, EUR, GBP) — for jurisdiction lookup
	Jurisdiction string  // explicit jurisdiction (e.g. "US-CA"); empty → derive from currency
	ProductType  string  // optional — some tax engines need this
}

// Response is the engine's verdict for one invoice.
type Response struct {
	JurisdictionCode string  // resolved jurisdiction (e.g. "US-CA", "EU-DE", "UK-LON")
	Rate             float64 // [0..1]
	TaxAmountUSD     float64 // = SubtotalUSD × Rate, 6dp
	Engine           string  // "mock" / "avalara" / "vertex" — for logs + report
	Note             string  // human reason when fallback fired (e.g. "avalara key missing → mock")
}

// Engine is the interface every tax engine satisfies.
type Engine interface {
	Calculate(ctx context.Context, req Request) (Response, error)
	Name() string
}

// Resolve turns a (currency, explicit jurisdiction, default, byCurrency map)
// into a single jurisdiction string. Returns empty when nothing matches.
//
// Order of precedence:
//
//  1. req.Jurisdiction if non-empty
//  2. byCurrency[req.Currency]
//  3. defaultJurisdiction
func Resolve(req Request, byCurrency map[string]string, defaultJurisdiction string) string {
	if strings.TrimSpace(req.Jurisdiction) != "" {
		return req.Jurisdiction
	}
	if c := strings.ToUpper(req.Currency); c != "" {
		if j, ok := byCurrency[c]; ok && j != "" {
			return j
		}
	}
	return defaultJurisdiction
}

// MultiplyAndRound applies rate to amount, rounding to 6 decimal places.
// Used by every engine — extracted so they can't accidentally diverge on
// rounding rules.
func MultiplyAndRound(amount, rate float64) float64 {
	if amount <= 0 || rate <= 0 {
		return 0
	}
	return math.Round(amount*rate*1e6) / 1e6
}

// validateRequest is the common entry-validator every engine runs first.
func validateRequest(r Request) error {
	if r.InvoiceID == "" {
		return fmt.Errorf("tax: invoice_id is required")
	}
	if r.SubtotalUSD < 0 {
		return fmt.Errorf("tax: subtotal_usd %v must be >= 0", r.SubtotalUSD)
	}
	return nil
}
