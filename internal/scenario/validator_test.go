package scenario

import (
	"strings"
	"testing"
)

// validateString is a test helper: load a YAML string, validate, return errors.
// Returns nil for a clean validation pass. Fails the test on a load error so
// table-driven cases don't have to distinguish "load error" from "validation
// error" — every case here is meant to be loadable.
func validateString(t *testing.T, yaml string) ValidationErrors {
	t.Helper()
	doc, err := LoadFromBytes([]byte(yaml))
	if err != nil {
		t.Fatalf("unexpected load error: %v\nyaml:\n%s", err, yaml)
	}
	return Validate(doc)
}

// hasErrorContaining reports whether any ValidationError contains substr.
func hasErrorContaining(errs ValidationErrors, substr string) bool {
	for _, e := range errs {
		if strings.Contains(e.Message, substr) || strings.Contains(e.Path, substr) {
			return true
		}
	}
	return false
}

// hasErrorAtPath reports whether any error has the exact path.
func hasErrorAtPath(errs ValidationErrors, path string) bool {
	for _, e := range errs {
		if e.Path == path {
			return true
		}
	}
	return false
}

func TestValidate_Minimal_Clean(t *testing.T) {
	errs := validateString(t, minimalValid)
	if errs.HasErrors() {
		t.Errorf("expected clean validation; got:\n%s", errs.Error())
	}
}

func TestValidate_NilDocument(t *testing.T) {
	if errs := Validate(nil); !errs.HasErrors() {
		t.Errorf("Validate(nil) should error")
	}
	if errs := Validate(&Document{}); !errs.HasErrors() {
		t.Errorf("Validate(empty doc) should error")
	}
}

// --- table-driven: every documented rule fires ---------------------------

func TestValidate_TableDriven(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(string) string
		wantPath   string
		wantSubstr string
	}{
		{
			name:       "missing schema_version",
			mutate:     func(y string) string { return strings.Replace(y, "schema_version: 1\n", "", 1) },
			wantPath:   "schema_version",
			wantSubstr: "required",
		},
		{
			name:       "schema_version too high",
			mutate:     func(y string) string { return strings.Replace(y, "schema_version: 1", "schema_version: 999", 1) },
			wantPath:   "schema_version",
			wantSubstr: "newer release",
		},
		{
			name:       "missing name",
			mutate:     func(y string) string { return strings.Replace(y, "name: minimal", "name: \"\"", 1) },
			wantPath:   "name",
			wantSubstr: "required",
		},
		{
			name:       "non-kebab name",
			mutate:     func(y string) string { return strings.Replace(y, "name: minimal", "name: BadName", 1) },
			wantPath:   "name",
			wantSubstr: "kebab-case",
		},
		{
			name:       "non-kebab name with trailing hyphen",
			mutate:     func(y string) string { return strings.Replace(y, "name: minimal", "name: trailing-", 1) },
			wantPath:   "name",
			wantSubstr: "kebab-case",
		},
		{
			name:       "target_tps zero",
			mutate:     func(y string) string { return strings.Replace(y, "target_tps: 50", "target_tps: 0", 1) },
			wantPath:   "target_tps",
			wantSubstr: "must be > 0",
		},
		{
			name:       "negative seed",
			mutate:     func(y string) string { return y + "\nseed: -1\n" },
			wantPath:   "seed",
			wantSubstr: "must be >= 0",
		},
		{
			name:       "duration zero",
			mutate:     func(y string) string { return strings.Replace(y, "duration: 60s", "duration: 0s", 1) },
			wantPath:   "duration",
			wantSubstr: "must be > 0",
		},
		{
			name:       "tenants.count zero",
			mutate:     func(y string) string { return strings.Replace(y, "count: 1", "count: 0", 1) },
			wantPath:   "tenants.count",
			wantSubstr: "must be > 0",
		},
		{
			name: "bad pricing_model",
			mutate: func(y string) string {
				return strings.Replace(y, "pricing_model: PER_UNIT", "pricing_model: NONSENSE", 1)
			},
			wantPath:   "tenants.archetypes[0].pricing_model",
			wantSubstr: "is not one of",
		},
		{
			name:       "bad billing_mode",
			mutate:     func(y string) string { return strings.Replace(y, "billing_mode: POSTPAID", "billing_mode: INVALID", 1) },
			wantPath:   "tenants.archetypes[0].billing_mode",
			wantSubstr: "is not one of",
		},
		{
			name:       "bad time_pattern",
			mutate:     func(y string) string { return y + "\ntime_pattern: lol\n" },
			wantPath:   "time_pattern",
			wantSubstr: "is not one of",
		},
		{
			name:       "bad distribution",
			mutate:     func(y string) string { return strings.Replace(y, "count: 1\n", "count: 1\n  distribution: bogus\n", 1) },
			wantPath:   "tenants.distribution",
			wantSubstr: "is not one of",
		},
		{
			name:       "weight out of range",
			mutate:     func(y string) string { return strings.Replace(y, "weight: 1.0", "weight: 1.5", 1) },
			wantPath:   "tenants.archetypes[0].weight",
			wantSubstr: "must be in [0, 1]",
		},
		{
			name: "PREPAID without wallet balance",
			mutate: func(y string) string {
				return strings.Replace(y, "billing_mode: POSTPAID", "billing_mode: PREPAID", 1)
			},
			wantPath:   "tenants.archetypes[0].rate_config.wallet_initial_balance_usd",
			wantSubstr: "wallet_initial_balance_usd > 0",
		},
		{
			name: "FLAT_RATE without flat_fee_usd",
			mutate: func(y string) string {
				y = strings.Replace(y, "pricing_model: PER_UNIT", "pricing_model: FLAT_RATE", 1)
				y = strings.Replace(y, "per_unit_rate_usd: 0.001", "per_unit_rate_usd: 0", 1)
				return y
			},
			wantPath:   "tenants.archetypes[0].rate_config.flat_fee_usd",
			wantSubstr: "flat_fee_usd > 0",
		},
		{
			name: "GRADUATED without tiers",
			mutate: func(y string) string {
				return strings.Replace(y, "pricing_model: PER_UNIT", "pricing_model: GRADUATED", 1)
			},
			wantPath:   "tenants.archetypes[0].rate_config.graduated_tiers",
			wantSubstr: "at least one tier",
		},
		{
			name: "VOLUME_TIERED without tiers",
			mutate: func(y string) string {
				return strings.Replace(y, "pricing_model: PER_UNIT", "pricing_model: VOLUME_TIERED", 1)
			},
			wantPath:   "tenants.archetypes[0].rate_config.volume_tiers",
			wantSubstr: "at least one tier",
		},
		{
			name: "PERCENTAGE without rate",
			mutate: func(y string) string {
				return strings.Replace(y, "pricing_model: PER_UNIT", "pricing_model: PERCENTAGE", 1)
			},
			wantPath:   "tenants.archetypes[0].rate_config.percentage_rate",
			wantSubstr: "percentage_rate > 0",
		},
		{
			name: "INCLUDED_QUOTA without included_free_units",
			mutate: func(y string) string {
				return strings.Replace(y, "pricing_model: PER_UNIT", "pricing_model: INCLUDED_QUOTA", 1)
			},
			wantPath:   "tenants.archetypes[0].rate_config",
			wantSubstr: "INCLUDED_QUOTA",
		},
		{
			name: "subscription_state_mix sum != 1.0",
			mutate: func(y string) string {
				return strings.Replace(y, "ACTIVE: 1.0", "ACTIVE: 0.5, TRIALING: 0.4", 1)
			},
			wantPath:   "tenants.archetypes[0].subscription_state_mix",
			wantSubstr: "sum to",
		},
		{
			name: "subscription_state_mix unknown state",
			mutate: func(y string) string {
				return strings.Replace(y, "ACTIVE: 1.0", "BOGUS_STATE: 1.0", 1)
			},
			wantPath:   "tenants.archetypes[0].subscription_state_mix.BOGUS_STATE",
			wantSubstr: "is not one of",
		},
		{
			name: "currency_mix sum != 1.0",
			mutate: func(y string) string {
				// Insert a currency_mix that doesn't sum.
				return strings.Replace(y, "subscription_state_mix:", "currency_mix: { USD: 0.5, EUR: 0.4 }\n      subscription_state_mix:", 1)
			},
			wantPath:   "tenants.archetypes[0].currency_mix",
			wantSubstr: "sum to",
		},
		{
			name: "stale_keys_pct without CANCELLED/EXPIRED",
			mutate: func(y string) string {
				return y + "\nnegative_paths:\n  stale_keys_pct: 0.01\n"
			},
			wantPath:   "negative_paths.stale_keys_pct",
			wantSubstr: "CANCELLED or EXPIRED",
		},
		{
			name: "negative_paths pct out of range",
			mutate: func(y string) string {
				return y + "\nnegative_paths:\n  malformed_pct: 1.5\n"
			},
			wantPath:   "negative_paths.malformed_pct",
			wantSubstr: "must be in [0, 1]",
		},
		{
			name: "payload_variation does not sum",
			mutate: func(y string) string {
				return y + "\npayload_variation:\n  small_pct: 0.5\n  medium_pct: 0.2\n  large_pct: 0.1\n"
			},
			wantPath:   "payload_variation",
			wantSubstr: "must be 1.0",
		},
		{
			name: "product_mix does not sum",
			mutate: func(y string) string {
				return y + "\nproduct_mix:\n  API: 0.5\n  AI_AGENT: 0.4\n"
			},
			wantPath:   "product_mix",
			wantSubstr: "sum to",
		},
		{
			name: "ingestion_paths does not sum",
			mutate: func(y string) string {
				return y + "\ningestion_paths:\n  rest_direct: 0.7\n  sdk_node: 0.2\n"
			},
			wantPath:   "ingestion_paths",
			wantSubstr: "sum to",
		},
		{
			name: "payments enabled, bad stripe_mode",
			mutate: func(y string) string {
				return y + "\npayments:\n  enabled: true\n  stripe_mode: nonsense\n"
			},
			wantPath:   "payments.stripe_mode",
			wantSubstr: "is not one of",
		},
		{
			name: "payments pct out of range",
			mutate: func(y string) string {
				return y + "\npayments:\n  enabled: true\n  stripe_mode: test\n  success_pct: 1.5\n"
			},
			wantPath:   "payments.success_pct",
			wantSubstr: "must be in [0, 1]",
		},
		{
			name: "tax engine invalid",
			mutate: func(y string) string {
				return y + "\ntax:\n  engine: bogus\n"
			},
			wantPath:   "tax.engine",
			wantSubstr: "is not one of",
		},
		{
			name: "tax rate out of range",
			mutate: func(y string) string {
				return y + "\ntax:\n  engine: mock\n  jurisdictions:\n    US-CA: 1.5\n"
			},
			wantPath:   "tax.jurisdictions.US-CA",
			wantSubstr: "must be in [0, 1]",
		},
		{
			name: "erp enabled, bad provider",
			mutate: func(y string) string {
				return y + "\nerp:\n  enabled: true\n  sync_sla_seconds: 60\n  providers_per_tenant_mix:\n    bogus_provider: 1.0\n"
			},
			wantPath:   "erp.providers_per_tenant_mix.bogus_provider",
			wantSubstr: "is not one of",
		},
		{
			name: "erp enabled without sync_sla",
			mutate: func(y string) string {
				return y + "\nerp:\n  enabled: true\n  providers_per_tenant_mix:\n    quickbooks: 1.0\n"
			},
			wantPath:   "erp.sync_sla_seconds",
			wantSubstr: "must be > 0",
		},
		{
			name: "credit_notes pct out of range",
			mutate: func(y string) string {
				return y + "\ncredit_notes:\n  enabled: true\n  refund_pct: -0.5\n"
			},
			wantPath:   "credit_notes.refund_pct",
			wantSubstr: "must be in [0, 1]",
		},
		{
			name: "lifecycle pct out of range",
			mutate: func(y string) string {
				return y + "\nlifecycle:\n  enabled: true\n  upgrades_per_hour_pct: 5.0\n"
			},
			wantPath:   "lifecycle.upgrades_per_hour_pct",
			wantSubstr: "must be in [0, 1]",
		},
		{
			name: "chaos enabled with no events",
			mutate: func(y string) string {
				return y + "\nchaos:\n  enabled: true\n"
			},
			wantPath:   "chaos.events",
			wantSubstr: "at least one event",
		},
		{
			name: "chaos event missing type",
			mutate: func(y string) string {
				return y + "\nchaos:\n  enabled: true\n  events:\n    - at: 1m\n      duration: 30s\n"
			},
			wantPath:   "chaos.events[0].type",
			wantSubstr: "required",
		},
		{
			name: "assertions cross_tenant_leakage > 0",
			mutate: func(y string) string {
				return y + "\nassertions:\n  cross_tenant_leakage_max: 5\n"
			},
			wantPath:   "assertions.cross_tenant_leakage_max",
			wantSubstr: "Aforo is multi-tenant",
		},
		{
			name: "duplicate archetype name",
			mutate: func(y string) string {
				// Add a second archetype with the same name; weights also need to sum to 1.
				dup := `    - name: a
      weight: 0.5
      pricing_model: PER_UNIT
      billing_mode: POSTPAID
      product_types: [API]
      customer_count: 5
      subscription_state_mix: { ACTIVE: 1.0 }
      rate_config: { per_unit_rate_usd: 0.001 }
`
				y = strings.Replace(y, "weight: 1.0", "weight: 0.5", 1)
				return y + dup
			},
			wantPath:   "tenants.archetypes[1].name",
			wantSubstr: "also defined",
		},
		{
			name: "missing archetypes",
			mutate: func(y string) string {
				// Replace the single archetype block with empty list.
				return strings.Replace(y, `archetypes:
    - name: a
      weight: 1.0
      pricing_model: PER_UNIT
      billing_mode: POSTPAID
      product_types: [API]
      customer_count: 5
      subscription_state_mix: { ACTIVE: 1.0 }
      rate_config:
        per_unit_rate_usd: 0.001
`, "archetypes: []\n", 1)
			},
			wantPath:   "tenants.archetypes",
			wantSubstr: "at least one archetype",
		},
		{
			name: "missing product_types",
			mutate: func(y string) string {
				return strings.Replace(y, "product_types: [API]", "product_types: []", 1)
			},
			wantPath:   "tenants.archetypes[0].product_types",
			wantSubstr: "must list at least one",
		},
		{
			name: "unknown product_type",
			mutate: func(y string) string {
				return strings.Replace(y, "product_types: [API]", "product_types: [BOGUS]", 1)
			},
			wantPath:   "tenants.archetypes[0].product_types[0]",
			wantSubstr: "is not one of",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			y := tc.mutate(minimalValid)
			doc, err := LoadFromBytes([]byte(y))
			if err != nil {
				// Some mutations (missing schema_version) yield a load error
				// because of the unmarshal step; that's a valid outcome too —
				// downstream callers see the same error class.
				if tc.wantSubstr == "required" || tc.wantSubstr == "newer release" {
					return // load-time enforcement is acceptable substitute
				}
				t.Fatalf("unexpected load error for case %q: %v\nyaml:\n%s", tc.name, err, y)
			}
			errs := Validate(doc)
			if !errs.HasErrors() {
				t.Fatalf("expected validation error containing %q at path %q; got clean pass\nyaml:\n%s",
					tc.wantSubstr, tc.wantPath, y)
			}
			if tc.wantPath != "" && !hasErrorAtPath(errs, tc.wantPath) {
				t.Errorf("expected error at path %q; got:\n%s", tc.wantPath, errs.Error())
			}
			if tc.wantSubstr != "" && !hasErrorContaining(errs, tc.wantSubstr) {
				t.Errorf("expected error containing %q; got:\n%s", tc.wantSubstr, errs.Error())
			}
		})
	}
}

// --- additional bounds coverage on rarely-set fields --------------------

func TestValidate_AssertionsBoundsTable(t *testing.T) {
	cases := []struct {
		name     string
		yamlTail string
		wantPath string
	}{
		{
			name:     "events_lost_max negative",
			yamlTail: "\nassertions:\n  events_lost_max: -1\n",
			wantPath: "assertions.events_lost_max",
		},
		{
			name:     "invoice_revenue_drift > 1",
			yamlTail: "\nassertions:\n  invoice_revenue_drift_pct_max: 1.5\n",
			wantPath: "assertions.invoice_revenue_drift_pct_max",
		},
		{
			name:     "p99 latency negative",
			yamlTail: "\nassertions:\n  p99_latency_ms_max: -100\n",
			wantPath: "assertions.p99_latency_ms_max",
		},
		{
			name:     "fairness > 1",
			yamlTail: "\nassertions:\n  per_tenant_p99_fairness_max_stddev_pct: 1.2\n",
			wantPath: "assertions.per_tenant_p99_fairness_max_stddev_pct",
		},
		{
			name:     "redis cache hit < 0",
			yamlTail: "\nassertions:\n  redis_cache_hit_ratio_min: -0.1\n",
			wantPath: "assertions.redis_cache_hit_ratio_min",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateString(t, minimalValid+tc.yamlTail)
			if !errs.HasErrors() || !hasErrorAtPath(errs, tc.wantPath) {
				t.Errorf("expected error at %q; got: %s", tc.wantPath, errs.Error())
			}
		})
	}
}

func TestValidate_CreditNoteBoundsBoth(t *testing.T) {
	tail := "\ncredit_notes:\n  enabled: true\n  refund_pct: 0.5\n  partial_pct: 1.5\n"
	errs := validateString(t, minimalValid+tail)
	if !hasErrorAtPath(errs, "credit_notes.partial_pct") {
		t.Errorf("expected partial_pct out-of-range error; got: %s", errs.Error())
	}
}

func TestValidate_PayloadVariationFieldOutOfRange(t *testing.T) {
	tail := "\npayload_variation:\n  small_pct: 1.5\n  medium_pct: 0.0\n  large_pct: 0.0\n"
	errs := validateString(t, minimalValid+tail)
	// Two failures expected: per-field bound + sum-not-1.0
	if !hasErrorAtPath(errs, "payload_variation.small_pct") {
		t.Errorf("expected per-field error; got: %s", errs.Error())
	}
}

func TestValidate_ChaosEventNegativeAt(t *testing.T) {
	tail := `
chaos:
  enabled: true
  events:
    - at: 0s
      duration: 30s
      type: kafka_kill
      params:
        instance_id: i-aaaa
`
	// at=0 is allowed (epoch zero is "fire immediately"); check only bounds.
	errs := validateString(t, minimalValid+tail)
	if errs.HasErrors() {
		t.Errorf("at=0 with valid type should be clean; got: %s", errs.Error())
	}
}

func TestValidate_ChaosUnknownType(t *testing.T) {
	tail := `
chaos:
  enabled: true
  events:
    - at: 1m
      duration: 30s
      type: not_a_real_type
`
	errs := validateString(t, minimalValid+tail)
	if !hasErrorAtPath(errs, "chaos.events[0].type") {
		t.Errorf("expected unknown chaos type error; got: %s", errs.Error())
	}
}

func TestValidate_ChaosMissingRequiredParam(t *testing.T) {
	tail := `
chaos:
  enabled: true
  events:
    - at: 1m
      duration: 30s
      type: redis_flush
      params:
        bastion_instance_id: i-bastion
`
	// Missing cache_endpoint must be flagged.
	errs := validateString(t, minimalValid+tail)
	if !hasErrorAtPath(errs, "chaos.events[0].params.cache_endpoint") {
		t.Errorf("expected missing cache_endpoint error; got: %s", errs.Error())
	}
}

func TestValidate_ChaosInlineParamsHoist(t *testing.T) {
	// Inline shorthand: instance_id and cluster_name are hoisted into Params.
	tail := `
chaos:
  enabled: true
  events:
    - at: 1m
      duration: 30s
      type: kafka_kill
      instance_id: i-inline
      cluster_name: msk-perf-1
`
	errs := validateString(t, minimalValid+tail)
	if errs.HasErrors() {
		t.Errorf("inline params should hoist into Params; got: %s", errs.Error())
	}
}

func TestValidate_ChaosInstantaneousZeroDuration(t *testing.T) {
	// redis_flush is a one-shot fire; duration: 0 is allowed.
	tail := `
chaos:
  enabled: true
  events:
    - at: 1m
      duration: 0s
      type: redis_flush
      params:
        bastion_instance_id: i-bastion
        cache_endpoint: perf-redis.amazonaws.com:6379
`
	errs := validateString(t, minimalValid+tail)
	if errs.HasErrors() {
		t.Errorf("zero-duration redis_flush should be clean; got: %s", errs.Error())
	}
}

func TestValidate_ChaosCHSlowdownNonPositiveLatency(t *testing.T) {
	tail := `
chaos:
  enabled: true
  events:
    - at: 1m
      duration: 5m
      type: ch_slowdown
      params:
        instance_id: i-ch
        latency_ms: 0
`
	errs := validateString(t, minimalValid+tail)
	if !hasErrorAtPath(errs, "chaos.events[0].params.latency_ms") {
		t.Errorf("expected latency_ms > 0 error; got: %s", errs.Error())
	}
}

func TestValidate_EmptyArchetypeName(t *testing.T) {
	y := strings.Replace(minimalValid, "name: a", `name: ""`, 1)
	errs := validateString(t, y)
	if !hasErrorAtPath(errs, "tenants.archetypes[0].name") {
		t.Errorf("expected empty-name error; got: %s", errs.Error())
	}
}

func TestValidate_ZeroCustomerCount(t *testing.T) {
	y := strings.Replace(minimalValid, "customer_count: 5", "customer_count: 0", 1)
	errs := validateString(t, y)
	if !hasErrorAtPath(errs, "tenants.archetypes[0].customer_count") {
		t.Errorf("expected customer_count error; got: %s", errs.Error())
	}
}

// --- error formatting ---------------------------------------------------

func TestValidationError_FormatWithLocation(t *testing.T) {
	errs := validateString(t, strings.Replace(minimalValid, "name: minimal", "name: BadName", 1))
	if !errs.HasErrors() {
		t.Fatalf("expected an error")
	}
	e := errs[0]
	if e.Line == 0 || e.Column == 0 {
		t.Errorf("expected file:line:col on validation error; got Line=%d Col=%d", e.Line, e.Column)
	}
	msg := e.Error()
	// File is empty for in-memory loads — the location prefix uses
	// "<scenario>" instead. We still expect line:col.
	if !strings.Contains(msg, ":") {
		t.Errorf("error %q lacks line:col separator", msg)
	}
}

func TestValidationError_DeterministicOrder(t *testing.T) {
	// Two errors at known different lines should sort by line ascending.
	yaml := strings.Replace(minimalValid, "name: minimal", "name: BadName", 1)
	yaml = strings.Replace(yaml, "target_tps: 50", "target_tps: 0", 1)
	errs := validateString(t, yaml)
	if len(errs) < 2 {
		t.Fatalf("expected >= 2 errors; got %d", len(errs))
	}
	for i := 1; i < len(errs); i++ {
		if errs[i-1].Line > errs[i].Line {
			t.Errorf("errors out of order: %d:%d came before %d:%d",
				errs[i-1].Line, errs[i-1].Column, errs[i].Line, errs[i].Column)
		}
	}
}

// --- helpers ------------------------------------------------------------

func TestKebabCaseRegex_Allows(t *testing.T) {
	good := []string{"a", "ab", "abc-def", "ci-smoke", "walk-realistic-50t", "x9", "9x", "a-1-b"}
	for _, s := range good {
		if !kebabCase.MatchString(s) {
			t.Errorf("kebabCase rejected valid name %q", s)
		}
	}
}

func TestKebabCaseRegex_Rejects(t *testing.T) {
	bad := []string{"", "Bad", "ABC", "-leading", "trailing-", "two_words", "with space", "UPPER"}
	for _, s := range bad {
		if kebabCase.MatchString(s) {
			t.Errorf("kebabCase accepted invalid name %q", s)
		}
	}
}

func TestWeightsApproxOne(t *testing.T) {
	// Stay clear of the exact boundary (0.999 / 1.001) — those literals
	// don't round-trip through float64 cleanly and we'd be testing IEEE-754
	// quirks rather than the rule. weightTolerance is 0.001; 0.0005 either
	// side is comfortably inside, 0.002 either side is comfortably outside.
	cases := map[float64]bool{
		1.0:    true,
		0.9995: true,
		1.0005: true,
		0.998:  false,
		1.002:  false,
		0.0:    false,
		0.5:    false,
	}
	for sum, want := range cases {
		if got := weightsApproxOne(sum); got != want {
			t.Errorf("weightsApproxOne(%v) = %v; want %v", sum, got, want)
		}
	}
}

func TestJoinPath(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{[]string{}, ""},
		{[]string{"a"}, "a"},
		{[]string{"a", "b"}, "a.b"},
		{[]string{"a", "b", "[3]", "c"}, "a.b[3].c"},
		{[]string{"tenants", "archetypes", "[0]", "weight"}, "tenants.archetypes[0].weight"},
	}
	for _, tc := range cases {
		if got := joinPath(tc.in); got != tc.want {
			t.Errorf("joinPath(%v) = %q; want %q", tc.in, got, tc.want)
		}
	}
}

// TestWarnings_PercentageMissingChargeBase ensures the advisory fires when
// a PERCENTAGE archetype omits charge_base_per_event_usd, and stays quiet
// when the field is set.
func TestWarnings_PercentageMissingChargeBase(t *testing.T) {
	missing := strings.Replace(
		minimalValid,
		"pricing_model: PER_UNIT",
		"pricing_model: PERCENTAGE",
		1,
	)
	missing = strings.Replace(missing,
		"per_unit_rate_usd: 0.001",
		"percentage_rate: 0.025",
		1,
	)
	doc, err := LoadFromBytes([]byte(missing))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	w := Warnings(doc)
	if len(w) != 1 || !strings.Contains(w[0], "PERCENTAGE") || !strings.Contains(w[0], "charge_base_per_event_usd") {
		t.Fatalf("expected one PERCENTAGE warning; got %v", w)
	}

	// With the field set, no warning.
	withBase := strings.Replace(missing,
		"percentage_rate: 0.025",
		"percentage_rate: 0.025\n        charge_base_per_event_usd: 100.0",
		1,
	)
	doc2, err := LoadFromBytes([]byte(withBase))
	if err != nil {
		t.Fatalf("load 2: %v", err)
	}
	if w := Warnings(doc2); len(w) != 0 {
		t.Fatalf("expected no warnings with charge_base set; got %v", w)
	}
}
