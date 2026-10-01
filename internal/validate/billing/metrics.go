package billing

import "math"

// MetricInput is one billable unit of a rate plan with the usage a customer
// ran up on it in the billing window. A plan is billed as the sum of its
// metrics, each rated with its own model and config — its own free
// allowance, block size, min fee, tiers and bands.
type MetricInput struct {
	Name string
	// Units is the metric's aggregated usage: the event count for a COUNT
	// metric, the summed quantity for a SUM metric. Zero (with no Outcomes
	// usage) means the customer never used the metric.
	Units float64
	// ChargeBaseUSD is the processed amount PERCENTAGE bills a share of.
	ChargeBaseUSD float64
	// Outcomes / Aggregation drive OUTCOME_BASED — see CalcInputs.
	Outcomes    []OutcomeUsage
	Aggregation string
	Model       PricingModel
	Rate        RateConfig
}

// MetricsInputs is CalcInputs for a multi-metric plan.
type MetricsInputs struct {
	Metrics  []MetricInput
	Discount *Discount
	TaxPct   float64
	Mode     Mode
}

// MetricCharge is one metric's line of a CalculateMetrics result.
type MetricCharge struct {
	Name   string
	Model  PricingModel
	Units  float64
	Charge float64
}

// CalculateMetrics rates every metric with its own config, sums the lines
// into the subtotal, then runs the same Discount → Tax → Route stages as
// Calculate on the total.
//
// A metric without usage produces no line — billing only synthesises a
// line for FLAT_RATE, which bills its fee every period. So a min fee, a
// staircase band or a percentage floor on an unused metric costs nothing.
func CalculateMetrics(in MetricsInputs, walletAvailable float64) (CalcResult, []MetricCharge, error) {
	lines := make([]MetricCharge, 0, len(in.Metrics))
	var subtotal float64
	var breakdown []TierCharge
	for _, m := range in.Metrics {
		charge, br, err := rateMetric(m)
		if err != nil {
			return CalcResult{}, nil, err
		}
		charge = roundCents(charge)
		subtotal += charge
		breakdown = append(breakdown, br...)
		lines = append(lines, MetricCharge{Name: m.Name, Model: m.Model, Units: m.Units, Charge: charge})
	}
	return finish(subtotal, breakdown, in.Discount, in.TaxPct, in.Mode, walletAvailable), lines, nil
}

// rateMetric prices one metric's usage with its own config.
func rateMetric(m MetricInput) (float64, []TierCharge, error) {
	if m.Model == FlatRate {
		return flatRateCharge(m.Rate), nil, nil
	}
	if m.Units <= 0 && !hasOutcomeUsage(m.Outcomes) {
		switch m.Model {
		case PerUnit, OutcomeBased, Staircase, Percentage, IncludedQuota, Graduated, VolumeTiered:
			return 0, nil, nil
		default:
			return 0, nil, ErrInvalidConfig
		}
	}
	// Tiered models are defined on whole units; a fractional estimate
	// (a tenant total split across customers) is rounded to the nearest.
	whole := int64(math.Round(m.Units))
	switch m.Model {
	case PerUnit:
		return linearCharge(m.Units, m.Rate), nil, nil
	case OutcomeBased:
		units := m.Units
		if w, ok := WeightOutcomes(m.Outcomes, m.Rate.OutcomeWeights, m.Aggregation); ok {
			units = w.Weighted
		}
		return linearCharge(units, m.Rate), nil, nil
	case Staircase:
		amt, br := staircaseCharge(m.Units, m.Rate.IncludedFreeUnits, m.Rate.StaircaseBands)
		return amt, br, nil
	case Percentage:
		return percentageCharge(m.ChargeBaseUSD, m.Rate), nil, nil
	case IncludedQuota:
		return includedQuotaCharge(whole, m.Rate), nil, nil
	case Graduated:
		amt, br := graduatedCharge(whole, m.Rate.GraduatedTiers)
		return amt, br, nil
	case VolumeTiered:
		amt, br := volumeTieredCharge(whole, m.Rate.VolumeTiers)
		return amt, br, nil
	default:
		return 0, nil, ErrInvalidConfig
	}
}
