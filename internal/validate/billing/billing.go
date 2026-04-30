// Package billing is the validator's pure-function model of Aforo's
// PricingCalculatorService + RateStage + DiscountStage + TaxStage +
// RouteStage. Every public function is deterministic, side-effect-free,
// and depends only on its inputs.
//
// The model is intentionally a re-implementation, not a wrapper. Bug-for-bug
// parity with the production service would defeat the purpose: the validator
// must catch regressions in the service. The prompts in CLAUDE.md document
// the contract per pricing model in plain English; this package implements
// that contract as Go and is unit-tested with golden cases that match the
// pricing-service's own PricingCalculatorServiceTest suite.
package billing

import (
	"errors"
	"math"
	"sort"
)

// PricingModel mirrors scenario.PricingModel — duplicated here so the
// package is self-contained and unit tests don't pull the scenario tree.
type PricingModel string

const (
	PerUnit       PricingModel = "PER_UNIT"
	FlatRate      PricingModel = "FLAT_RATE"
	Percentage    PricingModel = "PERCENTAGE"
	IncludedQuota PricingModel = "INCLUDED_QUOTA"
	Graduated     PricingModel = "GRADUATED"
	VolumeTiered  PricingModel = "VOLUME_TIERED"
)

// BillingMode mirrors scenario.BillingMode.
type BillingMode string

const (
	Postpaid BillingMode = "POSTPAID"
	Prepaid  BillingMode = "PREPAID"
	Hybrid   BillingMode = "HYBRID"
)

// Tier is one band in a GRADUATED or VOLUME_TIERED rate. UpToUnits is the
// upper bound of the band (inclusive). Use math.MaxInt64 for an open-ended
// final tier.
type Tier struct {
	UpToUnits  int64
	UnitPrice  float64
	FlatFee    float64
}

// RateConfig is the per-archetype config that drives the math. Mirrors
// scenario.RateConfig — only the fields the validator needs are here.
type RateConfig struct {
	FlatFeeUSD        float64
	PerUnitRateUSD    float64
	PercentageRate    float64 // e.g. 0.025 for 2.5%
	MinFeeUSD         float64
	IncludedFreeUnits int64
	BlockSizeUnits    int64
	GraduatedTiers    []Tier
	VolumeTiers       []Tier
}

// Discount captures a single applied discount. Type is "PERCENTAGE" or
// "FIXED_AMOUNT". Value is interpreted by Type — 0.10 for 10% off, or
// $5.00 for a fixed five-dollar discount.
type Discount struct {
	Type  string
	Value float64
}

// CalcInputs is the set of facts about a single (customer × archetype)
// billing window. Events is the count consumed; ChargeBaseUSD is the raw
// amount the customer paid (PERCENTAGE only); the rest are config.
type CalcInputs struct {
	Events        int64
	ChargeBaseUSD float64 // for PERCENTAGE
	Model         PricingModel
	Rate          RateConfig
	Discount      *Discount
	TaxPct        float64 // 0.0 = no tax (mock or not configured)
	Mode          BillingMode
}

// CalcResult is the staged output: (Subtotal, Discount, Taxable, Tax, Total)
// + the routing split between wallet and invoice. The validator compares
// Total against the actual invoice + wallet debit from the platform within
// scenario tolerance.
type CalcResult struct {
	Subtotal       float64
	DiscountAmount float64
	Taxable        float64
	TaxAmount      float64
	Total          float64

	// Route split — POSTPAID: invoice = Total, wallet = 0.
	// PREPAID:   wallet = Total, invoice = 0.
	// HYBRID:    wallet = min(Total, walletAvailable), invoice = remainder.
	WalletDebit float64
	InvoiceUSD  float64

	// For diagnostic detail in the validation report.
	TierBreakdown []TierCharge
	StageOrder    []string
}

// TierCharge is one row of a graduated/volume tier breakdown.
type TierCharge struct {
	From      int64
	To        int64
	Units     int64
	UnitPrice float64
	FlatFee   float64
	Charge    float64
}

// ErrInvalidConfig is returned when an archetype's RateConfig fails the
// minimum-required-fields check for its pricing model. Surfaced at
// validation time so the validator can SKIP the row (rather than emit a
// nonsense expected number).
var ErrInvalidConfig = errors.New("billing: invalid rate config for pricing model")

// Calculate runs the full Rate → Discount → Tax → Route pipeline.
//
// All money is double — float64 dollars, rounded only at presentation. The
// validator's tolerance (default 0.001 = 0.1%) absorbs the rounding noise
// vs. the production service's BigDecimal scale. Tighten the tolerance to
// 0.0 when the platform exposes the BigDecimal-level invoice total.
func Calculate(in CalcInputs, walletAvailable float64) (CalcResult, error) {
	subtotal, breakdown, err := rate(in)
	if err != nil {
		return CalcResult{}, err
	}
	subtotal = roundCents(subtotal)

	discount := applyDiscount(subtotal, in.Discount)
	discount = roundCents(discount)
	taxable := subtotal - discount

	tax := taxable * in.TaxPct
	tax = roundCents(tax)
	total := taxable + tax

	wallet, invoice := route(total, in.Mode, walletAvailable)

	return CalcResult{
		Subtotal:       subtotal,
		DiscountAmount: discount,
		Taxable:        taxable,
		TaxAmount:      tax,
		Total:          total,
		WalletDebit:    wallet,
		InvoiceUSD:     invoice,
		TierBreakdown:  breakdown,
		StageOrder:     []string{"rate", "discount", "tax", "route"},
	}, nil
}

// rate dispatches to the per-model pure functions.
func rate(in CalcInputs) (float64, []TierCharge, error) {
	switch in.Model {
	case PerUnit:
		return perUnitCharge(in.Events, in.Rate), nil, nil
	case FlatRate:
		return flatRateCharge(in.Rate), nil, nil
	case Percentage:
		return percentageCharge(in.ChargeBaseUSD, in.Rate), nil, nil
	case IncludedQuota:
		return includedQuotaCharge(in.Events, in.Rate), nil, nil
	case Graduated:
		amt, br := graduatedCharge(in.Events, in.Rate.GraduatedTiers)
		return amt, br, nil
	case VolumeTiered:
		amt, br := volumeTieredCharge(in.Events, in.Rate.VolumeTiers)
		return amt, br, nil
	default:
		return 0, nil, ErrInvalidConfig
	}
}

// applyDiscount enforces "discount cannot exceed subtotal" (DiscountStage
// rule). Always returns a non-negative number ≤ subtotal.
func applyDiscount(subtotal float64, d *Discount) float64 {
	if d == nil || subtotal <= 0 {
		return 0
	}
	var amount float64
	switch d.Type {
	case "PERCENTAGE":
		amount = subtotal * d.Value
	case "FIXED_AMOUNT":
		amount = d.Value
	default:
		return 0
	}
	if amount < 0 {
		return 0
	}
	if amount > subtotal {
		return subtotal
	}
	return amount
}

// route mirrors Aforo's RouteStage:
//
//	POSTPAID → invoice everything
//	PREPAID  → wallet everything (assumes funded; over-spend handled elsewhere)
//	HYBRID   → wallet first up to available, remainder to invoice
//
// The validator returns the planned split. The platform may differ if the
// wallet is over-drawn; the validator's billing check compares the platform's
// reported split, not its derivation.
func route(total float64, mode BillingMode, walletAvailable float64) (wallet, invoice float64) {
	if total <= 0 {
		return 0, 0
	}
	switch mode {
	case Postpaid:
		return 0, total
	case Prepaid:
		return total, 0
	case Hybrid:
		w := math.Min(total, math.Max(walletAvailable, 0))
		return w, total - w
	default:
		return 0, total
	}
}

// roundCents is half-even rounding to the cent. Aforo's BigDecimal scale=2
// uses HALF_EVEN; matching that here is what makes the parity tests close.
func roundCents(x float64) float64 {
	return math.RoundToEven(x*100) / 100
}

// SortTiersAscending returns tiers sorted by UpToUnits. The validator's
// graduated/volume math assumes ascending input; this guards against
// scenario authors writing the bands out of order.
func SortTiersAscending(tiers []Tier) []Tier {
	out := make([]Tier, len(tiers))
	copy(out, tiers)
	sort.SliceStable(out, func(i, j int) bool { return out[i].UpToUnits < out[j].UpToUnits })
	return out
}
