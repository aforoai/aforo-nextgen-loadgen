package scenario

// applyDefaults fills in low-stakes defaults for fields the author may have
// omitted. It does NOT supply defaults for things the author likely intended
// to set explicitly (target_tps, duration, archetypes, weights). The
// validator will catch missing required fields.
//
// Defaults are deliberately minimal — every starter scenario in scenarios/
// is meant to be a complete, copy-able example, so users don't grow to
// depend on behaviors that aren't visible in their YAML.
func applyDefaults(s *Scenario) {
	if s == nil {
		return
	}
	if s.Tenants.Distribution == "" {
		s.Tenants.Distribution = DistUniform
	}
	if s.TimePattern == "" {
		s.TimePattern = TimeConstant
	}
	// PayloadVariation default — only applied when ALL three are zero,
	// otherwise the author has chosen and we leave their numbers alone.
	if s.PayloadVariation.SmallPct == 0 &&
		s.PayloadVariation.MediumPct == 0 &&
		s.PayloadVariation.LargePct == 0 {
		s.PayloadVariation = PayloadVariation{
			SmallPct:  0.7,
			MediumPct: 0.25,
			LargePct:  0.05,
		}
	}
	// Tax engine — mock by default. Authors who care set this explicitly.
	if s.Tax.Engine == "" {
		s.Tax.Engine = TaxMock
	}
	// Stripe mode — payments default to test mode. Validator additionally
	// requires payments.enabled=true to set this; we just guard the zero
	// value so a user who flips enabled later doesn't get a silent default
	// of "" → invalid.
	if s.Payments.Enabled && s.Payments.StripeMode == "" {
		s.Payments.StripeMode = StripeTest
	}
	if s.Payments.Enabled {
		if s.Payments.DunningMaxAttempts <= 0 {
			s.Payments.DunningMaxAttempts = 3
		}
		if s.Payments.DunningRetryIntervalSeconds <= 0 {
			s.Payments.DunningRetryIntervalSeconds = 60
		}
		if s.Payments.IdempotencyPrefix == "" {
			s.Payments.IdempotencyPrefix = "aforo-loadgen"
		}
	}
	if s.Tax.ToleranceUSD <= 0 {
		s.Tax.ToleranceUSD = 0.01
	}
	if s.ERP.Enabled {
		if s.ERP.MaxRetries <= 0 {
			s.ERP.MaxRetries = 3
		}
		// Default verification on — load tests should prove the round-trip
		// to the provider, not just that we POSTed.
		if !s.ERP.VerifyExternalIDs {
			s.ERP.VerifyExternalIDs = true
		}
	}
	if s.CreditNotes.Enabled {
		if s.CreditNotes.PartialAmountPct <= 0 {
			s.CreditNotes.PartialAmountPct = 0.5
		}
		if s.CreditNotes.ApplyToInvoicePct <= 0 {
			s.CreditNotes.ApplyToInvoicePct = 1.0
		}
		if s.CreditNotes.Reason == "" {
			s.CreditNotes.Reason = "PRORATION"
		}
	}
	if s.Wallet.HoldTTLSeconds <= 0 {
		s.Wallet.HoldTTLSeconds = 60
	}
	if s.FX.AppliedAt == "" {
		s.FX.AppliedAt = "bill_run_time"
	}
	// Assertions defaults — when the author sets none, refuse to allow
	// cross-tenant leakage. Other fields stay at zero (= unconfigured).
	// The runner treats zero as "no assertion" except where documented.
	if !assertionsTouched(s.Assertions) {
		s.Assertions.CrossTenantLeakageMax = 0
		s.Assertions.PerArchetypeBillingMatch = true
		s.Assertions.StaleKeyZeroFalsePositives = true
	}
}

// assertionsTouched reports whether the author set any field on Assertions.
// Used by applyDefaults to avoid clobbering an explicit empty Assertions{}
// (which the user may have set on purpose to mean "no assertions").
//
// We treat "any non-zero numeric or any explicit boolean intent" as touched.
// Distinguishing "user wrote `per_archetype_billing_match: false`" from
// "user omitted the key" is impossible from the decoded struct alone, so
// the convention is: if any other assertion field is set, leave booleans
// alone; otherwise apply the safe defaults.
func assertionsTouched(a Assertions) bool {
	return a.EventsLostMax != 0 ||
		a.InvoiceRevenueDriftPctMax != 0 ||
		a.P99LatencyMsMax != 0 ||
		a.PerTenantP99FairnessMaxStddevPct != 0 ||
		a.RedisCacheHitRatioMin != 0 ||
		a.CrossTenantLeakageMax != 0
}
