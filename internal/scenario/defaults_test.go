package scenario

import "testing"

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
