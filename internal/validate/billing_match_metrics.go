package validate

import (
	"math"
	"sort"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate/billing"
)

// metricExpect is one metric's line of a per-metric expectation, reported
// so a mismatch can be traced to the metric that caused it.
type metricExpect struct {
	Metric       string  `json:"metric"`
	PricingModel string  `json:"pricing_model"`
	Aggregation  string  `json:"aggregation,omitempty"`
	Units        float64 `json:"units"`
	ChargeUSD    float64 `json:"charge_usd"`
	// Approximate is set when the metric's unit cannot be derived from the
	// run result (MAX is taken tenant-wide; COUNT_DISTINCT / percentile
	// aggregations fall back to the event count).
	Approximate bool `json:"approximate,omitempty"`
}

// expectedFor is the billing expectation for one customer.
//
// When the run result carries per-metric usage (RunResult.PerTenantMetric)
// and the customer's rate card resolves in the scenario, every metric of
// the plan is priced with its own config — allowance, block size, min fee,
// tiers, bands, weights — and the lines are summed (lines != nil).
// Otherwise — older run.json files, catalog-mode manifests, hand-built
// buckets — it falls back to computeExpected: the customer's estimated
// event count priced once with the plan-level config (lines == nil).
func expectedFor(bucket *archBucket, cust seed.ManifestCustomer, events int64, run *runner.RunResult, sc *scenario.Scenario) (billing.CalcResult, []metricExpect, error) {
	if in, approx, ok := perMetricInputs(bucket, cust, run, sc); ok {
		plan := bucket.planFor(cust)
		res, charges, err := billing.CalculateMetrics(in, plan.rate.WalletInitialBalanceUSD)
		if err != nil {
			return billing.CalcResult{}, nil, err
		}
		lines := make([]metricExpect, len(charges))
		for i, c := range charges {
			lines[i] = metricExpect{
				Metric:       c.Name,
				PricingModel: string(c.Model),
				Aggregation:  in.Metrics[i].Aggregation,
				Units:        c.Units,
				ChargeUSD:    c.Charge,
				Approximate:  approx[i],
			}
		}
		return res, lines, nil
	}
	res, err := computeExpected(bucket, cust, events, sc)
	return res, nil, err
}

// perMetricInputs builds the per-metric billing inputs for one customer.
// ok is false when the per-metric path does not apply (see expectedFor).
//
// The run result records usage per tenant, not per customer, so a
// customer's units are the tenant's times the customer's traffic share
// (bucketTenant.weights) — exact in expectation, since the generator picks
// uniformly among the tenant's usable keys.
func perMetricInputs(bucket *archBucket, cust seed.ManifestCustomer, run *runner.RunResult, sc *scenario.Scenario) (billing.MetricsInputs, []bool, bool) {
	if run == nil || run.PerTenantMetric == nil {
		return billing.MetricsInputs{}, nil, false
	}
	tenantID, ok := bucket.customerTenant[cust.CustomerID]
	if !ok {
		return billing.MetricsInputs{}, nil, false
	}
	bt := bucket.tenants[tenantID]
	if bt == nil || len(bt.metrics) == 0 {
		return billing.MetricsInputs{}, nil, false
	}
	plan := bucket.planFor(cust)
	arch, card, ok := resolveCard(sc, bucket.archetype, plan.name)
	if !ok {
		return billing.MetricsInputs{}, nil, false
	}

	// The plan's own model + rate come from the manifest (what was seeded);
	// the scenario card contributes the per-metric overrides.
	pricing := seed.ResolveMetricPricing(scenario.TenantArchetype{
		Name:          bucket.archetype,
		PricingModel:  plan.pricingModel,
		RateConfig:    plan.rate,
		MetricConfigs: seed.CardArchetype(arch, card).MetricConfigs,
	}, bt.metrics)

	usage := run.PerTenantMetric[tenantID]
	weight, total := float64(bt.weights[cust.CustomerID]), float64(bt.totalWeight)
	share := func(x float64) float64 {
		if total <= 0 || weight <= 0 {
			return 0
		}
		return round6(x * weight / total)
	}
	chargeBase := plan.rate.ChargeBasePerEventUSD
	if chargeBase <= 0 {
		chargeBase = 1.0
	}

	in := billing.MetricsInputs{
		Discount: convertDiscount(cust.Discount),
		TaxPct:   resolveTaxPct(sc, cust),
		Mode:     billing.Mode(plan.billingMode),
	}
	approx := make([]bool, 0, len(pricing))
	for _, mp := range pricing {
		u := usage[mp.MetricName]
		agg := strings.ToUpper(strings.TrimSpace(mp.AggregationType))
		events := share(float64(u.Events))
		mi := billing.MetricInput{
			Name:        mp.MetricName,
			Aggregation: agg,
			Model:       billing.PricingModel(mp.Model),
			Rate:        billingRate(mp.Model, mp.Rate),
		}
		isApprox := false
		switch agg {
		case "", "SUM":
			mi.Units = share(u.Quantity)
		case "COUNT":
			mi.Units = events
		case "AVG":
			if u.Events > 0 && events > 0 {
				mi.Units = round6(u.Quantity / float64(u.Events))
			}
		case "MAX":
			// The tenant-wide maximum stands in for each active customer's.
			if events > 0 {
				mi.Units = u.MaxQuantity
			}
			isApprox = true
		default:
			// COUNT_DISTINCT, PERCENTILE_95, …: not derivable from counters.
			mi.Units = events
			isApprox = true
		}
		mi.ChargeBaseUSD = mi.Units * chargeBase
		if mp.Model == scenario.PricingOutcomeBased && mi.Units > 0 {
			mi.Outcomes = outcomeRows(u, share)
			if len(mi.Outcomes) == 0 {
				// No per-status split recorded: spread the units by the
				// outcome mix of the card's product type.
				mix, _ := sc.EffectiveOutcomeMix(bucket.outcomeProductType(plan, sc))
				mi.Outcomes = expectedOutcomeUnits(mi.Units, mix)
			}
		}
		in.Metrics = append(in.Metrics, mi)
		approx = append(approx, isApprox)
	}
	return in, approx, true
}

// outcomeRows turns a metric's recorded per-status usage into billing's
// by-outcome rows for one customer, sorted by status.
func outcomeRows(u runner.MetricUsage, share func(float64) float64) []billing.OutcomeUsage {
	if len(u.ByStatus) == 0 {
		return nil
	}
	statuses := make([]string, 0, len(u.ByStatus))
	for s := range u.ByStatus {
		statuses = append(statuses, s)
	}
	sort.Strings(statuses)
	out := make([]billing.OutcomeUsage, 0, len(statuses))
	for _, s := range statuses {
		row := u.ByStatus[s]
		out = append(out, billing.OutcomeUsage{
			Status:     s,
			Quantity:   share(row.Quantity),
			EventCount: share(float64(row.Events)),
		})
	}
	return out
}

// expectedOutcomeUnits splits units across an outcome mix. Both Quantity
// and EventCount carry the split so it weights the same for SUM and COUNT.
func expectedOutcomeUnits(units float64, mix map[string]float64) []billing.OutcomeUsage {
	const scale = 1_000_000
	rows := billing.ExpectedOutcomes(scale, mix)
	for i := range rows {
		rows[i].Quantity = rows[i].Quantity / scale * units
		rows[i].EventCount = rows[i].EventCount / scale * units
	}
	return rows
}

// resolveCard finds the scenario archetype and rate card a manifest rate
// plan was seeded from. The card is matched by the plan name the seeder
// gives it (seed.CardRatePlanName); an unnamed plan (archetype fallback in
// planFor) or a single-card archetype resolves to the first card. ok is
// false when the scenario does not define the archetype — catalog-mode
// manifests — or a named plan matches none of several cards.
func resolveCard(sc *scenario.Scenario, archetype, planName string) (scenario.TenantArchetype, scenario.RateCardSpec, bool) {
	if sc == nil || archetype == "" {
		return scenario.TenantArchetype{}, scenario.RateCardSpec{}, false
	}
	for _, a := range sc.Tenants.Archetypes {
		if a.Name != archetype {
			continue
		}
		cards := a.RateCards
		if len(cards) == 0 {
			// Same backfill the seeder applies to a bare archetype.
			cards = []scenario.RateCardSpec{{
				Name:          "default",
				PricingModel:  a.PricingModel,
				BillingMode:   a.BillingMode,
				RateConfig:    a.RateConfig,
				MetricConfigs: a.MetricConfigs,
			}}
		}
		for _, c := range cards {
			if planName != "" && seed.CardRatePlanName(a.Name, c.Name) == planName {
				return a, c, true
			}
		}
		if planName == "" || len(cards) == 1 {
			return a, cards[0], true
		}
		return scenario.TenantArchetype{}, scenario.RateCardSpec{}, false
	}
	return scenario.TenantArchetype{}, scenario.RateCardSpec{}, false
}

// outcomeProductType is the product type whose outcome mix an OUTCOME_BASED
// estimate uses for a customer: the first entry of their rate card's
// product_filter when the card sets one, else the archetype's first product
// type. (It used to be the archetype's first product type for every card,
// so a card bound to AI_AGENT products on an API-first archetype was
// estimated with the API mix.)
func (b *archBucket) outcomeProductType(plan customerPlan, sc *scenario.Scenario) scenario.ProductType {
	if _, card, ok := resolveCard(sc, b.archetype, plan.name); ok && len(card.ProductFilter) > 0 {
		return card.ProductFilter[0]
	}
	return b.productType
}

// round6 rounds half-even to 6 decimal places (billing's unit scale), which
// also absorbs float noise from splitting a tenant total across customers.
func round6(x float64) float64 {
	return math.RoundToEven(x*1e6) / 1e6
}
