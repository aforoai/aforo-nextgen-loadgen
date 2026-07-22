package scenario

import "fmt"

// isZeroRateConfig reports whether every RateConfig field is at its zero
// value, meaning the caller left the config unset. Used by applyDefaults'
// v2 RateCards backfill to decide whether a per-card RateConfig should
// inherit from the archetype's top-level one. Per-field inheritance is
// intentionally NOT done — carrying an all-zero RateConfig alongside a
// non-zero one would silently drop tier / included-free fields the
// operator meant to set at plan level.
func isZeroRateConfig(rc RateConfig) bool {
	return rc.FlatFeeUSD == 0 &&
		rc.PerUnitRateUSD == 0 &&
		rc.PercentageRate == 0 &&
		rc.ChargeBasePerEventUSD == 0 &&
		rc.MinFeeUSD == 0 &&
		rc.IncludedFreeUnits == 0 &&
		rc.BlockSizeUnits == 0 &&
		len(rc.GraduatedTiers) == 0 &&
		len(rc.VolumeTiers) == 0 &&
		rc.WalletInitialBalanceUSD == 0 &&
		rc.TrialDays == 0
}

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
	// Backfill ProductsPerType=0 → 1 so existing scenarios (which never had
	// this field) keep the historical single-product-per-type behavior.
	// Explicit ProductsPerType=1 is also legal and equivalent.
	for i := range s.Tenants.Archetypes {
		if s.Tenants.Archetypes[i].ProductsPerType == 0 {
			s.Tenants.Archetypes[i].ProductsPerType = 1
		}
	}
	// Backfill RateCards (v2 schema, 2026-07-22). When the archetype has NO
	// explicit RateCards, synthesize a single card from the legacy top-level
	// PricingModel / BillingMode / RateConfig / MetricConfigs /
	// DimensionPricing so every v1 scenario keeps producing byte-identical
	// output. When the author DID declare RateCards, per-card fields
	// (BillingMode, RateConfig, MetricConfigs, DimensionPricing) fall back
	// to the archetype's top-level values so authors can share defaults
	// across cards without repetition. CustomerShare defaults to equal
	// share when unset on ALL cards.
	for i := range s.Tenants.Archetypes {
		a := &s.Tenants.Archetypes[i]
		if len(a.RateCards) == 0 {
			a.RateCards = []RateCardSpec{{
				Name:             "default",
				PricingModel:     a.PricingModel,
				BillingMode:      a.BillingMode,
				RateConfig:       a.RateConfig,
				MetricConfigs:    a.MetricConfigs,
				DimensionPricing: a.DimensionPricing,
				CustomerShare:    1.0,
			}}
			continue
		}
		// Per-card inheritance from top-level.
		anySharesSet := false
		for j := range a.RateCards {
			rc := &a.RateCards[j]
			if rc.Name == "" {
				rc.Name = fmt.Sprintf("card-%d", j+1)
			}
			if rc.PricingModel == "" {
				rc.PricingModel = a.PricingModel
			}
			if rc.BillingMode == "" {
				rc.BillingMode = a.BillingMode
			}
			// Whole-struct inheritance for RateConfig: if the caller left
			// the field entirely zero-valued we inherit; otherwise we
			// respect their explicit config wholesale (per the docstring on
			// RateCardSpec.RateConfig — per-field inheritance is NOT
			// applied).
			if isZeroRateConfig(rc.RateConfig) {
				rc.RateConfig = a.RateConfig
			}
			if rc.MetricConfigs == nil {
				rc.MetricConfigs = a.MetricConfigs
			}
			if rc.DimensionPricing == nil {
				rc.DimensionPricing = a.DimensionPricing
			}
			if rc.CustomerShare > 0 {
				anySharesSet = true
			}
		}
		if !anySharesSet {
			share := 1.0 / float64(len(a.RateCards))
			for j := range a.RateCards {
				a.RateCards[j].CustomerShare = share
			}
		}
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
