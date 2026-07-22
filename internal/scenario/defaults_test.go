package scenario

import (
	"math"
	"testing"
)

// TestApplyDefaults_v1RateCardsBackfill locks the v1→v2 back-compat path:
// an archetype declared with legacy top-level PricingModel / RateConfig
// and NO explicit RateCards must, after applyDefaults, carry exactly one
// RateCardSpec named "default" whose fields mirror the archetype's
// top-level values. This guarantee is what lets every existing scenario
// keep producing byte-identical output after the schema-version bump.
func TestApplyDefaults_v1RateCardsBackfill(t *testing.T) {
	s := &Scenario{
		Tenants: Tenants{
			Archetypes: []TenantArchetype{{
				Name:             "legacy",
				PricingModel:     PricingPerUnit,
				BillingMode:      BillingPostpaid,
				RateConfig:       RateConfig{PerUnitRateUSD: 0.01},
				MetricConfigs:    map[string]MetricOverride{"api_calls": {PricingModel: PricingPerUnit, PerUnitRateUSD: 0.02}},
				DimensionPricing: map[string]float64{"web_search": 1.5},
			}},
		},
	}
	applyDefaults(s)
	a := s.Tenants.Archetypes[0]
	if len(a.RateCards) != 1 {
		t.Fatalf("RateCards len = %d, want 1 (backfilled from legacy scalars)", len(a.RateCards))
	}
	rc := a.RateCards[0]
	if rc.Name != "default" {
		t.Errorf("RateCards[0].Name = %q, want %q", rc.Name, "default")
	}
	if rc.PricingModel != PricingPerUnit {
		t.Errorf("PricingModel = %q, want %q (from top-level)", rc.PricingModel, PricingPerUnit)
	}
	if rc.BillingMode != BillingPostpaid {
		t.Errorf("BillingMode = %q, want %q (from top-level)", rc.BillingMode, BillingPostpaid)
	}
	if rc.RateConfig.PerUnitRateUSD != 0.01 {
		t.Errorf("RateConfig.PerUnitRateUSD = %v, want 0.01 (from top-level)", rc.RateConfig.PerUnitRateUSD)
	}
	if rc.MetricConfigs["api_calls"].PerUnitRateUSD != 0.02 {
		t.Errorf("MetricConfigs backfill lost the api_calls entry")
	}
	if rc.DimensionPricing["web_search"] != 1.5 {
		t.Errorf("DimensionPricing backfill lost the web_search entry")
	}
	if rc.CustomerShare != 1.0 {
		t.Errorf("CustomerShare = %v, want 1.0 (single card = 100%%)", rc.CustomerShare)
	}
}

// TestApplyDefaults_v2ExplicitRateCards_InheritTopLevel asserts that an
// operator-authored archetype with explicit RateCards inherits any
// per-card field the operator left empty from the top-level equivalents
// (BillingMode, RateConfig, MetricConfigs, DimensionPricing) — matching
// the per-archetype "shared defaults" convention.
func TestApplyDefaults_v2ExplicitRateCards_InheritTopLevel(t *testing.T) {
	s := &Scenario{
		Tenants: Tenants{
			Archetypes: []TenantArchetype{{
				Name:        "v2",
				BillingMode: BillingPrepaid, // top-level default
				RateConfig:  RateConfig{PerUnitRateUSD: 0.005, WalletInitialBalanceUSD: 100},
				RateCards: []RateCardSpec{
					// Card 1: everything inherited from top-level except PricingModel.
					{Name: "starter", PricingModel: PricingPerUnit, CustomerShare: 0.6},
					// Card 2: explicit BillingMode override.
					{Name: "pro", PricingModel: PricingFlatRate, BillingMode: BillingPostpaid, RateConfig: RateConfig{FlatFeeUSD: 99}, CustomerShare: 0.4},
				},
			}},
		},
	}
	applyDefaults(s)
	a := s.Tenants.Archetypes[0]
	if a.RateCards[0].BillingMode != BillingPrepaid {
		t.Errorf("card starter: BillingMode = %q, want inherited %q", a.RateCards[0].BillingMode, BillingPrepaid)
	}
	if a.RateCards[0].RateConfig.PerUnitRateUSD != 0.005 {
		t.Errorf("card starter: RateConfig inherited-check lost PerUnitRateUSD")
	}
	if a.RateCards[1].BillingMode != BillingPostpaid {
		t.Errorf("card pro: BillingMode = %q, want explicit %q (not inherited)", a.RateCards[1].BillingMode, BillingPostpaid)
	}
	if a.RateCards[1].RateConfig.FlatFeeUSD != 99 {
		t.Errorf("card pro: explicit RateConfig lost FlatFeeUSD=99")
	}
	// Shares sum to 1.0.
	sum := a.RateCards[0].CustomerShare + a.RateCards[1].CustomerShare
	if math.Abs(sum-1.0) > 1e-9 {
		t.Errorf("customer_share sum = %v, want 1.0", sum)
	}
}

// TestApplyDefaults_v2RateCards_EqualShareDefault asserts that an
// operator-authored RateCards slice with NO CustomerShare set on any card
// gets an equal-share default (1.0/N) so scenarios can express "3 cards,
// spread customers evenly" without repeating a share on each entry.
func TestApplyDefaults_v2RateCards_EqualShareDefault(t *testing.T) {
	s := &Scenario{
		Tenants: Tenants{
			Archetypes: []TenantArchetype{{
				Name: "equal-shares",
				RateCards: []RateCardSpec{
					{Name: "a", PricingModel: PricingPerUnit, RateConfig: RateConfig{PerUnitRateUSD: 0.01}},
					{Name: "b", PricingModel: PricingPerUnit, RateConfig: RateConfig{PerUnitRateUSD: 0.02}},
					{Name: "c", PricingModel: PricingPerUnit, RateConfig: RateConfig{PerUnitRateUSD: 0.03}},
				},
			}},
		},
	}
	applyDefaults(s)
	shares := []float64{
		s.Tenants.Archetypes[0].RateCards[0].CustomerShare,
		s.Tenants.Archetypes[0].RateCards[1].CustomerShare,
		s.Tenants.Archetypes[0].RateCards[2].CustomerShare,
	}
	for i, sh := range shares {
		if math.Abs(sh-(1.0/3.0)) > 1e-9 {
			t.Errorf("card[%d].CustomerShare = %v, want %v (equal-share default)", i, sh, 1.0/3.0)
		}
	}
}

// TestApplyDefaults_ProductsPerType_DefaultsToOne — Issue 2 fix, backfill
// behavior. Existing scenarios never set products_per_type; applyDefaults
// backfills 0 → 1 so those scenarios keep the historical
// single-product-per-type shape.
func TestApplyDefaults_ProductsPerType_DefaultsToOne(t *testing.T) {
	s := &Scenario{
		Tenants: Tenants{
			Archetypes: []TenantArchetype{
				{Name: "a"}, // ProductsPerType zero-valued
				{Name: "b", ProductsPerType: 4},
			},
		},
	}
	applyDefaults(s)
	if got := s.Tenants.Archetypes[0].ProductsPerType; got != 1 {
		t.Errorf("archetype a: ProductsPerType = %d, want 1 (default)", got)
	}
	if got := s.Tenants.Archetypes[1].ProductsPerType; got != 4 {
		t.Errorf("archetype b: ProductsPerType = %d, want 4 (explicit not overwritten)", got)
	}
}

func TestApplyDefaults_Distribution(t *testing.T) {
	s := &Scenario{}
	applyDefaults(s)
	if s.Tenants.Distribution != DistUniform {
		t.Errorf("Tenants.Distribution = %q; want %q", s.Tenants.Distribution, DistUniform)
	}
}

func TestApplyDefaults_TimePattern(t *testing.T) {
	s := &Scenario{}
	applyDefaults(s)
	if s.TimePattern != TimeConstant {
		t.Errorf("TimePattern = %q; want %q", s.TimePattern, TimeConstant)
	}
}

func TestApplyDefaults_PreservesAuthorPayloadVariation(t *testing.T) {
	s := &Scenario{
		PayloadVariation: PayloadVariation{
			SmallPct:  0.5,
			MediumPct: 0.5,
		},
	}
	applyDefaults(s)
	if s.PayloadVariation.SmallPct != 0.5 || s.PayloadVariation.MediumPct != 0.5 || s.PayloadVariation.LargePct != 0 {
		t.Errorf("author payload variation overwritten: %+v", s.PayloadVariation)
	}
}

func TestApplyDefaults_PayloadVariationFromZero(t *testing.T) {
	s := &Scenario{}
	applyDefaults(s)
	if s.PayloadVariation.SmallPct != 0.7 || s.PayloadVariation.MediumPct != 0.25 || s.PayloadVariation.LargePct != 0.05 {
		t.Errorf("default payload variation wrong: %+v", s.PayloadVariation)
	}
}

func TestApplyDefaults_TaxEngine(t *testing.T) {
	s := &Scenario{}
	applyDefaults(s)
	if s.Tax.Engine != TaxMock {
		t.Errorf("Tax.Engine = %q; want %q", s.Tax.Engine, TaxMock)
	}
}

func TestApplyDefaults_StripeModeOnlyWhenEnabled(t *testing.T) {
	disabled := &Scenario{}
	applyDefaults(disabled)
	if disabled.Payments.StripeMode != "" {
		t.Errorf("disabled payments should not get a stripe_mode default; got %q", disabled.Payments.StripeMode)
	}

	enabled := &Scenario{Payments: Payments{Enabled: true}}
	applyDefaults(enabled)
	if enabled.Payments.StripeMode != StripeTest {
		t.Errorf("enabled payments default stripe_mode = %q; want %q", enabled.Payments.StripeMode, StripeTest)
	}
}

func TestApplyDefaults_AssertionsBooleansSetToSafeOnFreshScenario(t *testing.T) {
	s := &Scenario{}
	applyDefaults(s)
	if !s.Assertions.PerArchetypeBillingMatch {
		t.Error("per_archetype_billing_match should default true")
	}
	if !s.Assertions.StaleKeyZeroFalsePositives {
		t.Error("stale_key_zero_false_positives should default true")
	}
}

func TestApplyDefaults_AssertionsLeftAloneIfAnyTouched(t *testing.T) {
	// If the author set one numeric assertion, we leave the booleans alone
	// even if they're at zero values — the author may have meant to opt out.
	s := &Scenario{Assertions: Assertions{P99LatencyMsMax: 500}}
	applyDefaults(s)
	if s.Assertions.PerArchetypeBillingMatch {
		t.Error("per_archetype_billing_match should NOT be auto-set when other assertions touched")
	}
}

func TestApplyDefaults_NilSafe(t *testing.T) {
	// Don't panic on a nil pointer; a guard exists so future call sites
	// (config loaders, migration) can pass nil without ceremony.
	applyDefaults(nil)
}

func TestAssertionsTouched_TableDriven(t *testing.T) {
	cases := map[string]struct {
		a    Assertions
		want bool
	}{
		"zero":          {Assertions{}, false},
		"events_lost":   {Assertions{EventsLostMax: 1}, true},
		"revenue_drift": {Assertions{InvoiceRevenueDriftPctMax: 0.01}, true},
		"p99":           {Assertions{P99LatencyMsMax: 100}, true},
		"fairness":      {Assertions{PerTenantP99FairnessMaxStddevPct: 0.1}, true},
		"redis":         {Assertions{RedisCacheHitRatioMin: 0.9}, true},
		"cross_tenant":  {Assertions{CrossTenantLeakageMax: 5}, true},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := assertionsTouched(tc.a); got != tc.want {
				t.Errorf("assertionsTouched(%+v) = %v; want %v", tc.a, got, tc.want)
			}
		})
	}
}
