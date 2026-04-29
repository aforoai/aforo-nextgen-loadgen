package scenario

import (
	"fmt"
	"math"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// weightTolerance is the absolute tolerance used to decide whether a set of
// weights "sums to 1.0". Floats accumulate small errors when authored by
// hand (e.g. 0.1 + 0.2 + 0.7 = 0.9999999999999999), so we accept anything in
// [1 - tol, 1 + tol]. 0.001 is loose enough that a typo of 0.85 instead of
// 0.85 won't slip through, but tight enough to catch missing 5% buckets.
const weightTolerance = 0.001

// kebabCase matches names like "ci-smoke", "walk-realistic-50t", "x".
// The pattern allows single-character names (lowercase or digit), and
// requires hyphens to be sandwiched between alphanumeric characters.
var kebabCase = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// ValidationError is one validation failure. Path is a dot-and-bracket
// notation: "tenants.archetypes[3].weight". File/Line/Column are filled in
// when the path resolves to a real node in the source YAML.
type ValidationError struct {
	File    string
	Line    int
	Column  int
	Path    string
	Message string
}

// Error renders the error in a tool-friendly format. When the source
// location is known, the form is "<file>:<line>:<col>: <path>: <msg>", which
// IDEs and editors recognize and turn into a clickable link.
func (e ValidationError) Error() string {
	loc := e.File
	if loc == "" {
		loc = "<scenario>"
	}
	if e.Line > 0 {
		return fmt.Sprintf("%s:%d:%d: %s: %s", loc, e.Line, e.Column, e.Path, e.Message)
	}
	if e.Path != "" {
		return fmt.Sprintf("%s: %s: %s", loc, e.Path, e.Message)
	}
	return fmt.Sprintf("%s: %s", loc, e.Message)
}

// ValidationErrors collects multiple validation failures. The validator
// reports as many as it can find in one pass so users can fix in batches.
type ValidationErrors []ValidationError

// Error joins all errors with newlines.
func (es ValidationErrors) Error() string {
	if len(es) == 0 {
		return ""
	}
	parts := make([]string, len(es))
	for i, e := range es {
		parts[i] = e.Error()
	}
	return strings.Join(parts, "\n")
}

// HasErrors reports whether any errors were collected.
func (es ValidationErrors) HasErrors() bool { return len(es) > 0 }

// Validate runs every check the schema enforces and returns all errors found.
// Returns nil when the document is fully valid.
//
// Validation is:
//   - structural — required fields, allowed enum values
//   - cross-field — weights sum to 1.0, PREPAID/HYBRID requires wallet,
//     stale_keys_pct > 0 requires CANCELLED or EXPIRED in some archetype
//   - bounds — all _pct fields in [0, 1], counts > 0 where required
func Validate(doc *Document) ValidationErrors {
	if doc == nil || doc.Scenario == nil {
		return ValidationErrors{{Message: "scenario document is nil"}}
	}
	v := &validator{doc: doc}
	v.run()
	// Sort by (line, column, path) so error output is deterministic across
	// runs and machines. Unresolvable paths (line=0) sort first.
	sort.SliceStable(v.errs, func(i, j int) bool {
		a, b := v.errs[i], v.errs[j]
		if a.Line != b.Line {
			return a.Line < b.Line
		}
		if a.Column != b.Column {
			return a.Column < b.Column
		}
		return a.Path < b.Path
	})
	if len(v.errs) == 0 {
		return nil
	}
	return v.errs
}

type validator struct {
	doc  *Document
	errs ValidationErrors
}

// addAt records a violation at the given path. The path is resolved against
// the source YAML to find a line/column for the error message.
func (v *validator) addAt(path []string, msg string) {
	pathStr := joinPath(path)
	line, col := 0, 0
	if v.doc.Root != nil {
		if n := findNode(v.doc.Root, path); n != nil {
			line = n.Line
			col = n.Column
		} else if len(path) > 0 {
			// Fall back to the parent so users at least know the right region.
			if parent := findNode(v.doc.Root, path[:len(path)-1]); parent != nil {
				line = parent.Line
				col = parent.Column
			}
		}
	}
	v.errs = append(v.errs, ValidationError{
		File:    v.doc.Path,
		Line:    line,
		Column:  col,
		Path:    pathStr,
		Message: msg,
	})
}

// addAtNode records a violation at the position of a known yaml.Node. Used
// when iterating over nodes whose logical path is hard to reconstruct.
func (v *validator) addAtNode(n *yaml.Node, path []string, msg string) {
	pathStr := joinPath(path)
	line, col := 0, 0
	if n != nil {
		line, col = n.Line, n.Column
	}
	v.errs = append(v.errs, ValidationError{
		File:    v.doc.Path,
		Line:    line,
		Column:  col,
		Path:    pathStr,
		Message: msg,
	})
}

// run is the top-level validation entry. Each section is independent so
// that a failure in one doesn't short-circuit the rest.
func (v *validator) run() {
	s := v.doc.Scenario

	v.checkSchemaVersion(s)
	v.checkIdentity(s)
	v.checkTraffic(s)
	v.checkTenants(s)
	v.checkProductMix(s)
	v.checkIngestionPaths(s)
	v.checkPayloadVariation(s)
	v.checkNegativePaths(s)
	v.checkLifecycle(s)
	v.checkPayments(s)
	v.checkTax(s)
	v.checkERP(s)
	v.checkCreditNotes(s)
	v.checkChaos(s)
	v.checkAssertions(s)

	v.checkCrossField(s)
}

// --- section checkers ---------------------------------------------------

func (v *validator) checkSchemaVersion(s *Scenario) {
	if err := Migrate(s); err != nil {
		v.addAt([]string{"schema_version"}, err.Error())
	}
}

func (v *validator) checkIdentity(s *Scenario) {
	if s.Name == "" {
		v.addAt([]string{"name"}, "name is required")
	} else if !kebabCase.MatchString(s.Name) {
		v.addAt([]string{"name"},
			fmt.Sprintf("name %q must be kebab-case (lowercase letters, digits, hyphens; no leading/trailing hyphen)", s.Name))
	}
}

func (v *validator) checkTraffic(s *Scenario) {
	if s.TargetTPS <= 0 {
		v.addAt([]string{"target_tps"}, "target_tps must be > 0")
	}
	if s.Duration.Std() <= 0 {
		v.addAt([]string{"duration"}, "duration must be > 0 (e.g. \"60s\", \"24h\")")
	}
	if s.Seed < 0 {
		v.addAt([]string{"seed"}, "seed must be >= 0")
	}
	switch s.TimePattern {
	case TimeConstant, TimeSine24h, TimeBursty:
		// ok
	default:
		v.addAt([]string{"time_pattern"},
			fmt.Sprintf("time_pattern %q is not one of: constant, sine_24h, bursty", s.TimePattern))
	}
}

func (v *validator) checkTenants(s *Scenario) {
	if s.Tenants.Count <= 0 {
		v.addAt([]string{"tenants", "count"}, "tenants.count must be > 0")
	}
	switch s.Tenants.Distribution {
	case DistUniform, DistPareto8020, DistZipf:
		// ok
	default:
		v.addAt([]string{"tenants", "distribution"},
			fmt.Sprintf("tenants.distribution %q is not one of: uniform, pareto_80_20, zipf", s.Tenants.Distribution))
	}

	if len(s.Tenants.Archetypes) == 0 {
		v.addAt([]string{"tenants", "archetypes"}, "tenants.archetypes must contain at least one archetype")
		return
	}

	seenNames := make(map[string]int, len(s.Tenants.Archetypes))
	var weightSum float64
	for i := range s.Tenants.Archetypes {
		a := &s.Tenants.Archetypes[i]
		basePath := []string{"tenants", "archetypes", indexSeg(i)}

		if a.Name == "" {
			v.addAt(append(basePath, "name"), "archetype name is required")
		} else if prior, ok := seenNames[a.Name]; ok {
			v.addAt(append(basePath, "name"),
				fmt.Sprintf("archetype name %q is also defined at archetype index %d", a.Name, prior))
		} else {
			seenNames[a.Name] = i
		}

		if a.Weight < 0 || a.Weight > 1 {
			v.addAt(append(basePath, "weight"),
				fmt.Sprintf("weight %v must be in [0, 1]", a.Weight))
		}
		weightSum += a.Weight

		if !isValidPricingModel(a.PricingModel) {
			v.addAt(append(basePath, "pricing_model"),
				fmt.Sprintf("pricing_model %q is not one of: %s",
					a.PricingModel, joinPricingModels()))
		}
		if !isValidBillingMode(a.BillingMode) {
			v.addAt(append(basePath, "billing_mode"),
				fmt.Sprintf("billing_mode %q is not one of: %s",
					a.BillingMode, joinBillingModes()))
		}
		if len(a.ProductTypes) == 0 {
			v.addAt(append(basePath, "product_types"),
				"product_types must list at least one of: API, AI_AGENT, MCP_SERVER, AGENTIC_API")
		} else {
			for j, pt := range a.ProductTypes {
				if !isValidProductType(pt) {
					v.addAt(append(basePath, "product_types", indexSeg(j)),
						fmt.Sprintf("product_type %q is not one of: API, AI_AGENT, MCP_SERVER, AGENTIC_API", pt))
				}
			}
		}
		if a.CustomerCount <= 0 {
			v.addAt(append(basePath, "customer_count"),
				"customer_count must be > 0")
		}

		v.checkWeightMap(append(basePath, "currency_mix"), a.CurrencyMix, false /* required */)
		v.checkSubscriptionStateMix(append(basePath, "subscription_state_mix"), a.SubscriptionStateMix)
		v.checkWeightMap(append(basePath, "discount_mix"), a.DiscountMix, false)

		// Cross-field on the archetype: PREPAID/HYBRID require an initial wallet balance.
		if a.BillingMode == BillingPrepaid || a.BillingMode == BillingHybrid {
			if a.RateConfig.WalletInitialBalanceUSD <= 0 {
				v.addAt(append(basePath, "rate_config", "wallet_initial_balance_usd"),
					fmt.Sprintf("billing_mode=%s requires rate_config.wallet_initial_balance_usd > 0",
						a.BillingMode))
			}
		}

		// Pricing-model-specific minimums. We require a positive number on
		// the dimension that drives that model's bill — so a misconfigured
		// FLAT_RATE archetype can't silently bill $0/month.
		switch a.PricingModel {
		case PricingFlatRate:
			if a.RateConfig.FlatFeeUSD <= 0 {
				v.addAt(append(basePath, "rate_config", "flat_fee_usd"),
					"pricing_model=FLAT_RATE requires rate_config.flat_fee_usd > 0")
			}
		case PricingPerUnit:
			if a.RateConfig.PerUnitRateUSD <= 0 {
				v.addAt(append(basePath, "rate_config", "per_unit_rate_usd"),
					"pricing_model=PER_UNIT requires rate_config.per_unit_rate_usd > 0")
			}
		case PricingPercentage:
			if a.RateConfig.PercentageRate <= 0 {
				v.addAt(append(basePath, "rate_config", "percentage_rate"),
					"pricing_model=PERCENTAGE requires rate_config.percentage_rate > 0")
			}
		case PricingIncludedQuota:
			if a.RateConfig.IncludedFreeUnits <= 0 || a.RateConfig.PerUnitRateUSD <= 0 {
				v.addAt(append(basePath, "rate_config"),
					"pricing_model=INCLUDED_QUOTA requires both included_free_units > 0 and per_unit_rate_usd > 0 (overage rate)")
			}
		case PricingGraduated:
			if len(a.RateConfig.GraduatedTiers) == 0 {
				v.addAt(append(basePath, "rate_config", "graduated_tiers"),
					"pricing_model=GRADUATED requires at least one tier in rate_config.graduated_tiers")
			}
		case PricingVolumeTiered:
			if len(a.RateConfig.VolumeTiers) == 0 {
				v.addAt(append(basePath, "rate_config", "volume_tiers"),
					"pricing_model=VOLUME_TIERED requires at least one tier in rate_config.volume_tiers")
			}
		}
	}

	if !weightsApproxOne(weightSum) {
		v.addAt([]string{"tenants", "archetypes"},
			fmt.Sprintf("archetype weights sum to %.4f; must be 1.0 ± %g", weightSum, weightTolerance))
	}
}

func (v *validator) checkSubscriptionStateMix(path []string, mix map[SubscriptionState]float64) {
	if len(mix) == 0 {
		// Optional — the seed harness will default to 100% ACTIVE.
		return
	}
	var sum float64
	for state, weight := range mix {
		if !isValidSubscriptionState(state) {
			v.addAt(append(append([]string{}, path...), string(state)),
				fmt.Sprintf("subscription state %q is not one of: %s", state, joinStates()))
		}
		if weight < 0 || weight > 1 {
			v.addAt(append(append([]string{}, path...), string(state)),
				fmt.Sprintf("weight for state %q is %v; must be in [0, 1]", state, weight))
		}
		sum += weight
	}
	if !weightsApproxOne(sum) {
		v.addAt(path, fmt.Sprintf("subscription_state_mix weights sum to %.4f; must be 1.0 ± %g", sum, weightTolerance))
	}
}

// checkWeightMap validates that all values are in [0, 1] and (if non-empty)
// sum to 1.0. Empty maps are allowed when required=false.
func (v *validator) checkWeightMap(path []string, mix map[string]float64, required bool) {
	if len(mix) == 0 {
		if required {
			v.addAt(path, "weight map must contain at least one entry summing to 1.0")
		}
		return
	}
	var sum float64
	for k, weight := range mix {
		if k == "" {
			v.addAt(path, "weight map keys must be non-empty")
		}
		if weight < 0 || weight > 1 {
			v.addAt(append(append([]string{}, path...), k),
				fmt.Sprintf("weight for %q is %v; must be in [0, 1]", k, weight))
		}
		sum += weight
	}
	if !weightsApproxOne(sum) {
		v.addAt(path, fmt.Sprintf("weights sum to %.4f; must be 1.0 ± %g", sum, weightTolerance))
	}
}

func (v *validator) checkProductMix(s *Scenario) {
	sum := s.ProductMix.Sum()
	if sum == 0 {
		// Optional — runner can default to all weight on the archetype's product types.
		return
	}
	if !weightsApproxOne(sum) {
		v.addAt([]string{"product_mix"},
			fmt.Sprintf("product_mix weights sum to %.4f; must be 1.0 ± %g (or omit for default)", sum, weightTolerance))
	}
	for label, w := range map[string]float64{
		"API": s.ProductMix.API, "AI_AGENT": s.ProductMix.AIAgent,
		"MCP_SERVER": s.ProductMix.MCPServer, "AGENTIC_API": s.ProductMix.AgenticAPI,
	} {
		if w < 0 || w > 1 {
			v.addAt([]string{"product_mix", label},
				fmt.Sprintf("product_mix.%s is %v; must be in [0, 1]", label, w))
		}
	}
}

func (v *validator) checkIngestionPaths(s *Scenario) {
	sum := s.IngestionPaths.Sum()
	if sum == 0 {
		// Optional — runner default is rest_direct=1.0.
		return
	}
	if !weightsApproxOne(sum) {
		v.addAt([]string{"ingestion_paths"},
			fmt.Sprintf("ingestion_paths weights sum to %.4f; must be 1.0 ± %g (or omit for default)", sum, weightTolerance))
	}
}

func (v *validator) checkPayloadVariation(s *Scenario) {
	for label, w := range map[string]float64{
		"small_pct":  s.PayloadVariation.SmallPct,
		"medium_pct": s.PayloadVariation.MediumPct,
		"large_pct":  s.PayloadVariation.LargePct,
	} {
		if w < 0 || w > 1 {
			v.addAt([]string{"payload_variation", label},
				fmt.Sprintf("payload_variation.%s is %v; must be in [0, 1]", label, w))
		}
	}
	sum := s.PayloadVariation.Sum()
	if !weightsApproxOne(sum) {
		v.addAt([]string{"payload_variation"},
			fmt.Sprintf("payload_variation small+medium+large = %.4f; must be 1.0 ± %g", sum, weightTolerance))
	}
}

func (v *validator) checkNegativePaths(s *Scenario) {
	for label, w := range map[string]float64{
		"late_events_pct":   s.NegativePaths.LateEventsPct,
		"future_events_pct": s.NegativePaths.FutureEventsPct,
		"malformed_pct":     s.NegativePaths.MalformedPct,
		"wrong_auth_pct":    s.NegativePaths.WrongAuthPct,
		"stale_keys_pct":    s.NegativePaths.StaleKeysPct,
		"oversize_pct":      s.NegativePaths.OversizePct,
	} {
		if w < 0 || w > 1 {
			v.addAt([]string{"negative_paths", label},
				fmt.Sprintf("negative_paths.%s is %v; must be in [0, 1]", label, w))
		}
	}
}

func (v *validator) checkLifecycle(s *Scenario) {
	if !s.Lifecycle.Enabled {
		return
	}
	for label, w := range map[string]float64{
		"upgrades_per_hour_pct":         s.Lifecycle.UpgradesPerHourPct,
		"downgrades_per_hour_pct":       s.Lifecycle.DowngradesPerHourPct,
		"pause_resume_per_hour_pct":     s.Lifecycle.PauseResumePerHourPct,
		"trial_conversion_per_hour_pct": s.Lifecycle.TrialConversionPerHourPct,
		"trial_cancel_per_hour_pct":     s.Lifecycle.TrialCancelPerHourPct,
		"migrate_per_hour_pct":          s.Lifecycle.MigratePerHourPct,
		"retry_payment_per_hour_pct":    s.Lifecycle.RetryPaymentPerHourPct,
	} {
		if w < 0 || w > 1 {
			v.addAt([]string{"lifecycle", label},
				fmt.Sprintf("lifecycle.%s is %v; must be in [0, 1]", label, w))
		}
	}
}

func (v *validator) checkPayments(s *Scenario) {
	if !s.Payments.Enabled {
		return
	}
	switch s.Payments.StripeMode {
	case StripeTest, StripeLive:
		// ok
	case "":
		// applyDefaults sets test when enabled — but if a downstream rewrite
		// cleared it, surface the requirement.
		v.addAt([]string{"payments", "stripe_mode"},
			"payments.stripe_mode is required when payments.enabled=true (use \"test\" unless you really mean \"live\")")
	default:
		v.addAt([]string{"payments", "stripe_mode"},
			fmt.Sprintf("payments.stripe_mode %q is not one of: test, live", s.Payments.StripeMode))
	}
	for label, w := range map[string]float64{
		"success_pct":            s.Payments.SuccessPct,
		"decline_pct":            s.Payments.DeclinePct,
		"insufficient_funds_pct": s.Payments.InsufficientFundsPct,
	} {
		if w < 0 || w > 1 {
			v.addAt([]string{"payments", label},
				fmt.Sprintf("payments.%s is %v; must be in [0, 1]", label, w))
		}
	}
	sum := s.Payments.SuccessPct + s.Payments.DeclinePct + s.Payments.InsufficientFundsPct
	if sum > 0 && !weightsApproxOne(sum) {
		v.addAt([]string{"payments"},
			fmt.Sprintf("payments success+decline+insufficient_funds = %.4f; must be 1.0 ± %g (or all 0 for runner default)", sum, weightTolerance))
	}
}

func (v *validator) checkTax(s *Scenario) {
	switch s.Tax.Engine {
	case TaxMock, TaxAvalara, TaxVertex:
		// ok
	default:
		v.addAt([]string{"tax", "engine"},
			fmt.Sprintf("tax.engine %q is not one of: mock, avalara, vertex", s.Tax.Engine))
	}
	for jurisdiction, rate := range s.Tax.Jurisdictions {
		if rate < 0 || rate > 1 {
			v.addAt([]string{"tax", "jurisdictions", jurisdiction},
				fmt.Sprintf("tax rate %v for %q must be in [0, 1] (express percentages as decimals: 0.0925 for 9.25%%)", rate, jurisdiction))
		}
	}
}

func (v *validator) checkERP(s *Scenario) {
	if !s.ERP.Enabled {
		return
	}
	if s.ERP.SyncSLASeconds <= 0 {
		v.addAt([]string{"erp", "sync_sla_seconds"},
			"erp.sync_sla_seconds must be > 0 when erp.enabled=true")
	}
	v.checkWeightMap([]string{"erp", "providers_per_tenant_mix"}, s.ERP.ProvidersPerTenantMix, true /* required */)
	for k := range s.ERP.ProvidersPerTenantMix {
		if !isKnownERPProvider(k) {
			v.addAt([]string{"erp", "providers_per_tenant_mix", k},
				fmt.Sprintf("provider %q is not one of: quickbooks, xero, netsuite, custom_webhook", k))
		}
	}
}

func (v *validator) checkCreditNotes(s *Scenario) {
	if !s.CreditNotes.Enabled {
		return
	}
	if s.CreditNotes.RefundPct < 0 || s.CreditNotes.RefundPct > 1 {
		v.addAt([]string{"credit_notes", "refund_pct"},
			fmt.Sprintf("credit_notes.refund_pct %v must be in [0, 1]", s.CreditNotes.RefundPct))
	}
	if s.CreditNotes.PartialPct < 0 || s.CreditNotes.PartialPct > 1 {
		v.addAt([]string{"credit_notes", "partial_pct"},
			fmt.Sprintf("credit_notes.partial_pct %v must be in [0, 1]", s.CreditNotes.PartialPct))
	}
}

func (v *validator) checkChaos(s *Scenario) {
	if !s.Chaos.Enabled {
		return
	}
	if len(s.Chaos.Events) == 0 {
		v.addAt([]string{"chaos", "events"},
			"chaos.events must contain at least one event when chaos.enabled=true")
		return
	}
	for i := range s.Chaos.Events {
		e := &s.Chaos.Events[i]
		basePath := []string{"chaos", "events", indexSeg(i)}
		if e.At.Std() < 0 {
			v.addAt(append(basePath, "at"), "chaos event 'at' must be >= 0")
		}
		if e.Duration.Std() <= 0 {
			v.addAt(append(basePath, "duration"), "chaos event 'duration' must be > 0")
		}
		if e.Type == "" {
			v.addAt(append(basePath, "type"), "chaos event 'type' is required")
		}
	}
}

func (v *validator) checkAssertions(s *Scenario) {
	if s.Assertions.EventsLostMax < 0 {
		v.addAt([]string{"assertions", "events_lost_max"}, "events_lost_max must be >= 0")
	}
	if s.Assertions.InvoiceRevenueDriftPctMax < 0 || s.Assertions.InvoiceRevenueDriftPctMax > 1 {
		v.addAt([]string{"assertions", "invoice_revenue_drift_pct_max"},
			"invoice_revenue_drift_pct_max must be in [0, 1]")
	}
	if s.Assertions.P99LatencyMsMax < 0 {
		v.addAt([]string{"assertions", "p99_latency_ms_max"}, "p99_latency_ms_max must be >= 0")
	}
	if s.Assertions.PerTenantP99FairnessMaxStddevPct < 0 || s.Assertions.PerTenantP99FairnessMaxStddevPct > 1 {
		v.addAt([]string{"assertions", "per_tenant_p99_fairness_max_stddev_pct"},
			"per_tenant_p99_fairness_max_stddev_pct must be in [0, 1]")
	}
	if s.Assertions.RedisCacheHitRatioMin < 0 || s.Assertions.RedisCacheHitRatioMin > 1 {
		v.addAt([]string{"assertions", "redis_cache_hit_ratio_min"},
			"redis_cache_hit_ratio_min must be in [0, 1]")
	}
	if s.Assertions.CrossTenantLeakageMax < 0 {
		v.addAt([]string{"assertions", "cross_tenant_leakage_max"},
			"cross_tenant_leakage_max must be >= 0")
	}
	// Strong rule: cross-tenant leakage MUST be exactly 0. Aforo is a multi-
	// tenant SaaS billing platform; even one leaked event is a data-isolation
	// breach. We refuse to load a scenario that opts out of this assertion.
	if s.Assertions.CrossTenantLeakageMax > 0 {
		v.addAt([]string{"assertions", "cross_tenant_leakage_max"},
			"cross_tenant_leakage_max must be 0 — Aforo is multi-tenant; any leakage is a hard failure")
	}
}

// checkCrossField runs the validations that depend on more than one section.
func (v *validator) checkCrossField(s *Scenario) {
	// stale_keys_pct > 0 requires CANCELLED or EXPIRED in some archetype's
	// subscription_state_mix. Without that, there are no stale keys to use,
	// and the negative-path injector would silently produce zero traffic.
	if s.NegativePaths.StaleKeysPct > 0 && !anyArchetypeHasStaleStates(s) {
		v.addAt([]string{"negative_paths", "stale_keys_pct"},
			"stale_keys_pct > 0 requires at least one archetype with CANCELLED or EXPIRED in subscription_state_mix")
	}
}

// --- helpers ------------------------------------------------------------

// weightsApproxOne reports whether sum is within weightTolerance of 1.0.
func weightsApproxOne(sum float64) bool {
	return math.Abs(sum-1.0) <= weightTolerance
}

// joinPath renders a path slice in dot-and-bracket notation. Index segments
// like "[3]" attach to the previous segment without a separating dot.
func joinPath(p []string) string {
	if len(p) == 0 {
		return ""
	}
	var b strings.Builder
	for i, seg := range p {
		if i > 0 && !isIndexSegment(seg) {
			b.WriteByte('.')
		}
		b.WriteString(seg)
	}
	return b.String()
}

// indexSeg renders an int as "[N]" — the marker findNode recognizes.
func indexSeg(i int) string { return fmt.Sprintf("[%d]", i) }

func isValidPricingModel(m PricingModel) bool {
	for _, x := range AllPricingModels {
		if m == x {
			return true
		}
	}
	return false
}

func isValidBillingMode(m BillingMode) bool {
	for _, x := range AllBillingModes {
		if m == x {
			return true
		}
	}
	return false
}

func isValidProductType(t ProductType) bool {
	for _, x := range AllProductTypes {
		if t == x {
			return true
		}
	}
	return false
}

func isValidSubscriptionState(s SubscriptionState) bool {
	for _, x := range AllSubscriptionStates {
		if s == x {
			return true
		}
	}
	return false
}

func isKnownERPProvider(k string) bool {
	switch k {
	case "quickbooks", "xero", "netsuite", "custom_webhook":
		return true
	}
	return false
}

func anyArchetypeHasStaleStates(s *Scenario) bool {
	for _, a := range s.Tenants.Archetypes {
		for state, w := range a.SubscriptionStateMix {
			if w > 0 && (state == StateCancelled || state == StateExpired) {
				return true
			}
		}
	}
	return false
}

func joinPricingModels() string {
	parts := make([]string, len(AllPricingModels))
	for i, m := range AllPricingModels {
		parts[i] = string(m)
	}
	return strings.Join(parts, ", ")
}

func joinBillingModes() string {
	parts := make([]string, len(AllBillingModes))
	for i, m := range AllBillingModes {
		parts[i] = string(m)
	}
	return strings.Join(parts, ", ")
}

func joinStates() string {
	parts := make([]string, len(AllSubscriptionStates))
	for i, s := range AllSubscriptionStates {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}
