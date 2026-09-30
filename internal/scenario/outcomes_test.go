package scenario

import (
	"math"
	"strings"
	"testing"
)

func outcomeArchetype(weights map[string]float64) TenantArchetype {
	a := baseArchetype()
	a.PricingModel = PricingOutcomeBased
	a.RateConfig = RateConfig{PerUnitRateUSD: 0.01, OutcomeWeights: weights}
	return a
}

func staircaseArchetype(bands []StaircaseBand) TenantArchetype {
	a := baseArchetype()
	a.PricingModel = PricingStaircase
	a.RateConfig = RateConfig{StaircaseBands: bands}
	return a
}

func validateArchetype(a TenantArchetype) ValidationErrors {
	return Validate(&Document{Scenario: baseScenario(a)})
}

func TestValidate_OutcomeWeights_ValidAccepted(t *testing.T) {
	// Lower-case keys are fine — pricing and billing match case-insensitively.
	a := outcomeArchetype(map[string]float64{"success": 1, "PARTIAL": 0.5, "Hitl_Required": 0})
	if errs := validateArchetype(a); errs.HasErrors() {
		t.Fatalf("valid OUTCOME_BASED archetype rejected: %s", errs.Error())
	}
}

func TestValidate_OutcomeWeights_Rules(t *testing.T) {
	cases := []struct {
		name    string
		weights map[string]float64
		want    string
	}{
		{"missing", nil, "requires outcome_weights"},
		{"above one", map[string]float64{"SUCCESS": 1.5}, "must be in [0, 1]"},
		{"negative", map[string]float64{"SUCCESS": 1, "ERROR": -0.1}, "must be in [0, 1]"},
		{"nan", map[string]float64{"SUCCESS": math.NaN()}, "must be in [0, 1]"},
		{"all zero", map[string]float64{"SUCCESS": 0, "ERROR": 0}, "at least one weight > 0"},
		{"unknown key", map[string]float64{"SUCCESS": 1, "UNAUTHORIZED": 0}, "not a canonical execution status"},
		{"case duplicate", map[string]float64{"SUCCESS": 1, "success": 0.5}, "name the same status"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateArchetype(outcomeArchetype(tc.weights))
			if !hasErrorContaining(errs, tc.want) {
				t.Errorf("want error containing %q; got %s", tc.want, errs.Error())
			}
		})
	}
}

func TestValidate_OutcomeBased_RequiresRate(t *testing.T) {
	a := outcomeArchetype(map[string]float64{"SUCCESS": 1})
	a.RateConfig.PerUnitRateUSD = 0
	if errs := validateArchetype(a); !hasErrorContaining(errs, "per_unit_rate_usd > 0") {
		t.Errorf("OUTCOME_BASED with no rate should be rejected; got %s", errs.Error())
	}
}

func TestValidate_OutcomeWeights_OnMetricOverride(t *testing.T) {
	a := baseArchetype()
	a.MetricConfigs = map[string]MetricOverride{
		"API Calls": {PricingModel: PricingOutcomeBased, PerUnitRateUSD: 0.01,
			OutcomeWeights: map[string]float64{"SUCCESS": 0}},
	}
	if errs := validateArchetype(a); !hasErrorContaining(errs, "at least one weight > 0") {
		t.Errorf("metric override all-zero weights should be rejected; got %s", errs.Error())
	}
}

func TestValidate_StaircaseBands_ValidAccepted(t *testing.T) {
	a := staircaseArchetype([]StaircaseBand{
		{FromUnits: 1, ToUnits: 100, PriceUSD: 10},
		{FromUnits: 101, ToUnits: 500, PriceUSD: 30},
		{FromUnits: 501, PriceUSD: 50}, // open-ended top band
	})
	if errs := validateArchetype(a); errs.HasErrors() {
		t.Fatalf("valid STAIRCASE archetype rejected: %s", errs.Error())
	}
	// A bounded top band is also valid (billing charges the last band above it).
	b := staircaseArchetype([]StaircaseBand{{FromUnits: 0, ToUnits: 10, PriceUSD: 1}, {FromUnits: 11, ToUnits: 20, PriceUSD: 2}})
	if errs := validateArchetype(b); errs.HasErrors() {
		t.Fatalf("bounded STAIRCASE rejected: %s", errs.Error())
	}
	// Pricing sorts steps by tierStart before validating (and
	// PricingCalculator sorts before matching), so bands written out of
	// order are valid there — and must be valid here.
	c := staircaseArchetype([]StaircaseBand{
		{FromUnits: 501, PriceUSD: 50},
		{FromUnits: 1, ToUnits: 100, PriceUSD: 10},
		{FromUnits: 101, ToUnits: 500, PriceUSD: 30},
	})
	if errs := validateArchetype(c); errs.HasErrors() {
		t.Fatalf("out-of-order STAIRCASE bands rejected (pricing accepts them): %s", errs.Error())
	}
	// ...while a real gap is still caught.
	d := staircaseArchetype([]StaircaseBand{{FromUnits: 102, PriceUSD: 20}, {FromUnits: 1, ToUnits: 100, PriceUSD: 10}})
	if errs := validateArchetype(d); !hasErrorContaining(errs, "bands must be contiguous: from_units 102 must be 101") {
		t.Fatalf("out-of-order gap not reported; got %s", errs.Error())
	}
}

func TestValidate_StaircaseBands_Rules(t *testing.T) {
	cases := []struct {
		name  string
		bands []StaircaseBand
		want  string
	}{
		{"missing", nil, "requires at least one band"},
		{"gap", []StaircaseBand{{1, 100, 10}, {102, 0, 20}}, "must be contiguous"},
		{"overlap", []StaircaseBand{{1, 100, 10}, {100, 0, 20}}, "must be contiguous"},
		{"open not last", []StaircaseBand{{1, 0, 10}, {101, 200, 20}}, "only valid on the highest band"},
		{"first starts high", []StaircaseBand{{5, 100, 10}}, "must start at 0 or 1"},
		{"zero price", []StaircaseBand{{1, 100, 0}}, "price_usd 0 must be > 0"},
		{"to below from", []StaircaseBand{{1, 100, 10}, {101, 50, 20}}, "is below from_units"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateArchetype(staircaseArchetype(tc.bands))
			if !hasErrorContaining(errs, tc.want) {
				t.Errorf("want error containing %q; got %s", tc.want, errs.Error())
			}
		})
	}
}

func TestValidate_RateCard_OutcomeAndStaircase(t *testing.T) {
	a := baseArchetype()
	a.RateCards = []RateCardSpec{
		{Name: "outcome", PricingModel: PricingOutcomeBased, CustomerShare: 0.5,
			RateConfig: RateConfig{PerUnitRateUSD: 0.01, OutcomeWeights: map[string]float64{"SUCCESS": 1, "BOGUS": 1}}},
		{Name: "package", PricingModel: PricingStaircase, CustomerShare: 0.5,
			RateConfig: RateConfig{StaircaseBands: []StaircaseBand{{1, 10, 5}, {12, 0, 9}}}},
	}
	errs := validateArchetype(a)
	for _, want := range []string{"rate_cards[0].rate_config.outcome_weights.BOGUS", "must be contiguous"} {
		if !hasErrorContaining(errs, want) {
			t.Errorf("want error containing %q; got %s", want, errs.Error())
		}
	}
}

func TestValidate_OutcomeMix(t *testing.T) {
	s := baseScenario(baseArchetype())
	s.OutcomeMix = map[string]float64{"SUCCESS": 0.9, "partial": 0.05, "HITL_REQUIRED": 0.05}
	s.OutcomeMixByProduct = map[ProductType]map[string]float64{ProductMCPServer: {"SUCCESS": 0.8, "ERROR": 0.2}}
	if errs := Validate(&Document{Scenario: s}); errs.HasErrors() {
		t.Fatalf("valid outcome_mix rejected: %s", errs.Error())
	}

	bad := []struct {
		name   string
		mutate func(*Scenario)
		want   string
	}{
		{"unknown status", func(s *Scenario) { s.OutcomeMix = map[string]float64{"success": 0.5, "rate_limited": 0.5} }, "not a canonical execution status"},
		{"sum", func(s *Scenario) { s.OutcomeMix = map[string]float64{"SUCCESS": 0.5} }, "sum to 0.5000"},
		{"case dup", func(s *Scenario) { s.OutcomeMix = map[string]float64{"SUCCESS": 0.5, "success": 0.5} }, "name the same status"},
		{"bad product", func(s *Scenario) {
			s.OutcomeMixByProduct = map[ProductType]map[string]float64{"GRPC": {"SUCCESS": 1}}
		}, "is not one of"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			s := baseScenario(baseArchetype())
			tc.mutate(s)
			errs := Validate(&Document{Scenario: s})
			if !hasErrorContaining(errs, tc.want) {
				t.Errorf("want error containing %q; got %s", tc.want, errs.Error())
			}
		})
	}
}

func TestEffectiveOutcomeMix(t *testing.T) {
	s := &Scenario{
		OutcomeMix:          map[string]float64{"success": 0.9, "PARTIAL": 0.1},
		OutcomeMixByProduct: map[ProductType]map[string]float64{ProductMCPServer: {"SUCCESS": 3, "ERROR": 1}},
	}
	mix, configured := s.EffectiveOutcomeMix(ProductAPI)
	if !configured || math.Abs(mix["SUCCESS"]-0.9) > 1e-12 || math.Abs(mix["PARTIAL"]-0.1) > 1e-12 {
		t.Errorf("outcome_mix not applied/upper-cased: %v configured=%v", mix, configured)
	}
	// Per-product override wins and is normalised: 3/(3+1) = 0.75.
	mix, _ = s.EffectiveOutcomeMix(ProductMCPServer)
	if math.Abs(mix["SUCCESS"]-0.75) > 1e-12 || math.Abs(mix["ERROR"]-0.25) > 1e-12 {
		t.Errorf("per-product mix: %v", mix)
	}
	// No mix → built-in default, not configured.
	mix, configured = (&Scenario{}).EffectiveOutcomeMix(ProductAIAgent)
	if configured || math.Abs(mix["SUCCESS"]-0.92) > 1e-12 {
		t.Errorf("default agent mix: %v configured=%v", mix, configured)
	}
	// Every default key is canonical and every default sums to 1.
	for _, pt := range AllProductTypes {
		sum := 0.0
		for k, v := range DefaultOutcomeMix(pt) {
			if !IsExecutionStatus(k) || k != strings.ToUpper(k) {
				t.Errorf("%s default mix has non-canonical key %q", pt, k)
			}
			sum += v
		}
		if math.Abs(sum-1) > 1e-9 {
			t.Errorf("%s default mix sums to %v", pt, sum)
		}
	}
}

func TestExecutionStatuses_Canonical(t *testing.T) {
	// Exactly the 11 statuses in aforo-common contracts/execution-statuses.json.
	want := "SUCCESS,PARTIAL,TIMEOUT,ERROR,VALIDATION_FAILED,FAILED,FAILURE,CANCELLED,PENDING,BLOCKED,HITL_REQUIRED"
	if got := strings.Join(ExecutionStatuses, ","); got != want {
		t.Errorf("ExecutionStatuses = %s; want %s", got, want)
	}
	if !IsExecutionStatus(" hitl_required ") || IsExecutionStatus("unauthorized") || IsExecutionStatus("") {
		t.Error("IsExecutionStatus normalisation wrong")
	}
}
