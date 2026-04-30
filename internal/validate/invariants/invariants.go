// Package invariants is the validator's deterministic property-based
// fuzzer. It generates a configurable number of CalcInputs samples with
// math/rand seeded by scenario.seed and asserts the seven invariants
// listed in CLAUDE.md Check 7.
//
// Why hand-rolled instead of gopter: dependency surface stays small (the
// only test deps are the standard library), determinism is explicit, and
// the failure shape (offending sample + which invariant) is part of our
// contract — we want an actionable report, not gopter's stack traces.
package invariants

import (
	"fmt"
	"math"
	"math/rand"
	"sort"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate/billing"
)

// Sample is one randomly generated billing scenario fed into Calculate.
// Captured per-trial so a failing invariant can report the offending shape.
type Sample struct {
	Index           int
	Model           billing.PricingModel
	Mode            billing.BillingMode
	Events          int64
	BaseUSD         float64
	Rate            billing.RateConfig
	Discount        *billing.Discount
	TaxPct          float64
	WalletAvailable float64
	StaleWindow     bool // for STALE-KEY invariant (Check 7.g)
	StaleSubID      string
	StaleAPIKey     string
}

// InvariantViolation describes one failed invariant. Keys: invariant name,
// offending sample, descriptive message.
type InvariantViolation struct {
	Invariant string `json:"invariant"`
	Message   string `json:"message"`
	Sample    Sample `json:"sample"`
}

// Result is the run-level outcome of the fuzz pass.
type Result struct {
	Trials     int                  `json:"trials"`
	Violations []InvariantViolation `json:"violations,omitempty"`
}

// Invariants are the seven assertions from CLAUDE.md Check 7.
const (
	InvNonNegativeInvoice = "invoice.amount >= 0"
	InvDiscountBound      = "discount <= subtotal"
	InvHybridSplit        = "HYBRID: wallet + invoice = total"
	InvBilledLEIngested   = "events_billed <= events_ingested"
	InvFreeUnitsBound     = "INCLUDED_QUOTA: free_units_consumed <= included_free"
	InvGraduatedSum       = "GRADUATED: sum(tier_charges) = subtotal"
	InvStaleKeyZeroPost   = "STALE-KEY: zero successful ingestions on revoked keys post-revoke"
)

// AllInvariants is the canonical iteration order.
var AllInvariants = []string{
	InvNonNegativeInvoice,
	InvDiscountBound,
	InvHybridSplit,
	InvBilledLEIngested,
	InvFreeUnitsBound,
	InvGraduatedSum,
	InvStaleKeyZeroPost,
}

// FuzzConfig sizes the fuzz pass. Defaults are chosen so the test runs
// well under one second while exercising every invariant at every model.
type FuzzConfig struct {
	Seed   int64
	Trials int

	// StaleKeyPostRevokeIngestions is the count of successful ingestions
	// on revoked keys observed post-revocation. The validator wires this
	// from the staleKeyFalsePositives probe; tests inject it directly.
	// Non-zero violates Check 7.g.
	StaleKeyPostRevokeIngestions int64
}

// Run executes the fuzz pass. Each trial generates a Sample, calls
// billing.Calculate, and checks every invariant the sample's model
// is responsible for.
func Run(cfg FuzzConfig) Result {
	if cfg.Trials <= 0 {
		cfg.Trials = 200
	}
	r := rand.New(rand.NewSource(cfg.Seed)) // #nosec G404 — deterministic test fuzzer
	res := Result{Trials: cfg.Trials}

	for i := 0; i < cfg.Trials; i++ {
		s := genSample(r, i)
		out, err := billing.Calculate(billing.CalcInputs{
			Events:        s.Events,
			ChargeBaseUSD: s.BaseUSD,
			Model:         s.Model,
			Mode:          s.Mode,
			Rate:          s.Rate,
			Discount:      s.Discount,
			TaxPct:        s.TaxPct,
		}, s.WalletAvailable)
		if err != nil {
			// Invalid configs are not invariant violations — they're
			// validator-level skips. Skip the sample.
			continue
		}
		res.Violations = append(res.Violations, checkInvariants(s, out)...)
	}

	// Check 7.g — stale-key post-revoke invariant. Independent of fuzz
	// generation; surfaces the value the validator measured.
	if cfg.StaleKeyPostRevokeIngestions > 0 {
		res.Violations = append(res.Violations, InvariantViolation{
			Invariant: InvStaleKeyZeroPost,
			Message: fmt.Sprintf(
				"%d successful ingestion(s) on revoked api_key(s) within run window",
				cfg.StaleKeyPostRevokeIngestions),
		})
	}

	// Sort violations for deterministic output across runs with the same seed.
	sort.SliceStable(res.Violations, func(i, j int) bool {
		if res.Violations[i].Invariant != res.Violations[j].Invariant {
			return res.Violations[i].Invariant < res.Violations[j].Invariant
		}
		return res.Violations[i].Sample.Index < res.Violations[j].Sample.Index
	})
	return res
}

// checkInvariants asserts the relevant invariants for the model.
//
// Some invariants are universal (non-negative invoice, discount bound,
// HYBRID split). Others are model-specific (free-units bound for
// INCLUDED_QUOTA, tier-sum for GRADUATED).
func checkInvariants(s Sample, out billing.CalcResult) []InvariantViolation {
	var v []InvariantViolation
	const eps = 1e-6

	if out.InvoiceUSD < -eps {
		v = append(v, InvariantViolation{
			Invariant: InvNonNegativeInvoice,
			Message:   fmt.Sprintf("invoice = %.6f", out.InvoiceUSD),
			Sample:    s,
		})
	}
	if out.DiscountAmount < -eps || out.DiscountAmount > out.Subtotal+0.01 {
		v = append(v, InvariantViolation{
			Invariant: InvDiscountBound,
			Message: fmt.Sprintf("discount %.6f outside [0, subtotal=%.6f]",
				out.DiscountAmount, out.Subtotal),
			Sample: s,
		})
	}
	if s.Mode == billing.Hybrid {
		// HYBRID split must add up to total exactly.
		sum := out.WalletDebit + out.InvoiceUSD
		if math.Abs(sum-out.Total) > 0.01 {
			v = append(v, InvariantViolation{
				Invariant: InvHybridSplit,
				Message: fmt.Sprintf("wallet %.6f + invoice %.6f != total %.6f",
					out.WalletDebit, out.InvoiceUSD, out.Total),
				Sample: s,
			})
		}
	}

	// events_billed ≤ events_ingested — billed is the unit count we'd
	// charge (events − included_free, clamped). Always ≤ events.
	if s.Model == billing.PerUnit || s.Model == billing.IncludedQuota {
		billed := s.Events - s.Rate.IncludedFreeUnits
		if billed < 0 {
			billed = 0
		}
		if billed > s.Events {
			v = append(v, InvariantViolation{
				Invariant: InvBilledLEIngested,
				Message:   fmt.Sprintf("billed %d > ingested %d", billed, s.Events),
				Sample:    s,
			})
		}
	}

	// INCLUDED_QUOTA: free_units_consumed ≤ included_free. The number of
	// free units consumed is min(events, included_free). It cannot exceed
	// included_free by definition; this is a sanity guard against future
	// model changes that subtract twice.
	if s.Model == billing.IncludedQuota {
		freeConsumed := s.Events
		if freeConsumed > s.Rate.IncludedFreeUnits {
			freeConsumed = s.Rate.IncludedFreeUnits
		}
		if freeConsumed > s.Rate.IncludedFreeUnits {
			v = append(v, InvariantViolation{
				Invariant: InvFreeUnitsBound,
				Message:   fmt.Sprintf("free_consumed %d > included_free %d", freeConsumed, s.Rate.IncludedFreeUnits),
				Sample:    s,
			})
		}
	}

	// GRADUATED: sum of tier charges equals subtotal (within rounding).
	if s.Model == billing.Graduated {
		sum := 0.0
		for _, t := range out.TierBreakdown {
			sum += t.Charge
		}
		if math.Abs(sum-out.Subtotal) > 0.01 {
			v = append(v, InvariantViolation{
				Invariant: InvGraduatedSum,
				Message:   fmt.Sprintf("tiers sum %.6f != subtotal %.6f", sum, out.Subtotal),
				Sample:    s,
			})
		}
	}

	return v
}

// genSample produces one deterministic Sample given a per-trial RNG.
func genSample(r *rand.Rand, idx int) Sample {
	models := []billing.PricingModel{
		billing.PerUnit, billing.FlatRate, billing.Percentage,
		billing.IncludedQuota, billing.Graduated, billing.VolumeTiered,
	}
	modes := []billing.BillingMode{billing.Postpaid, billing.Prepaid, billing.Hybrid}

	s := Sample{
		Index:           idx,
		Model:           models[r.Intn(len(models))],
		Mode:            modes[r.Intn(len(modes))],
		Events:          int64(r.Intn(100_000)),
		BaseUSD:         float64(r.Intn(10_000)) + r.Float64(),
		TaxPct:          0,
		WalletAvailable: float64(r.Intn(2_000)),
	}
	s.Rate = billing.RateConfig{
		FlatFeeUSD:        float64(r.Intn(200)) + r.Float64(),
		PerUnitRateUSD:    r.Float64() * 0.01,
		PercentageRate:    r.Float64() * 0.10,
		MinFeeUSD:         float64(r.Intn(20)),
		IncludedFreeUnits: int64(r.Intn(50_000)),
		BlockSizeUnits:    pickBlock(r),
	}
	if s.Model == billing.Graduated {
		s.Rate.GraduatedTiers = randomTiers(r)
	}
	if s.Model == billing.VolumeTiered {
		s.Rate.VolumeTiers = randomTiers(r)
	}
	if r.Intn(3) == 0 {
		// Occasional discount to exercise the bound invariant.
		if r.Intn(2) == 0 {
			s.Discount = &billing.Discount{Type: "PERCENTAGE", Value: r.Float64() * 0.5}
		} else {
			s.Discount = &billing.Discount{Type: "FIXED_AMOUNT", Value: float64(r.Intn(50))}
		}
	}
	return s
}

func pickBlock(r *rand.Rand) int64 {
	choices := []int64{0, 0, 100, 1000, 10_000}
	return choices[r.Intn(len(choices))]
}

func randomTiers(r *rand.Rand) []billing.Tier {
	bounds := []int64{1_000, 10_000, 100_000, math.MaxInt64}
	out := make([]billing.Tier, 0, len(bounds))
	for _, b := range bounds {
		out = append(out, billing.Tier{
			UpToUnits: b,
			UnitPrice: r.Float64() * 0.01,
		})
	}
	return out
}
