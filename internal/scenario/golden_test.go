package scenario

import (
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/scenarios"
)

// TestGolden_BuiltInScenariosLoadAndValidate is the contract: every scenario
// shipped in the scenarios/ directory MUST load (strict YAML decode) and MUST
// validate clean. CI gates merges on this — a scenario that doesn't validate
// is a broken release artifact.
func TestGolden_BuiltInScenariosLoadAndValidate(t *testing.T) {
	names := scenarios.Names()
	if len(names) == 0 {
		t.Fatal("no built-in scenarios bundled — embed.FS missing or empty")
	}

	// Sanity-check the catalog count. Sessions add scenarios; this number
	// is updated when a session ships new ones. The hard contract is the
	// `expectedNames` list below — anything in it MUST exist; new entries
	// can be added without breaking older tests.
	const expectedCount = 21
	if len(names) != expectedCount {
		t.Errorf("catalog has %d scenarios; expected %d (%v)",
			len(names), expectedCount, names)
	}

	// Each canonical scenario name MUST be present. If a future session
	// renames one, update both this list and docs/scenario-schema.md.
	expectedNames := []string{
		// Session 2 (Crawl/Walk/Run + matrix + lifecycle):
		"ci-smoke",
		"crawl-e2e",
		"lifecycle-stress",
		"matrix-billing",
		"run-15k-7day",
		"walk-realistic-50t",
		// Session 9 (payments / ERP / multi-currency / wallet):
		"payments-stripe-test",
		"erp-sync-validation",
		"multi-currency",
		"wallet-lifecycle",
		// Session 10 (CI integration):
		"ci-mcp-only",
		"ci-mcp-jsonrpc",
		"ci-billing",
		"ci-payments-mock",
		"ci-stale-keys",
		// P8 (AI_AGENT descriptor per-capability coverage):
		"ci-ai-agent-rest",
		// P14 (AI_AGENT wire protocol against @aforo/agent-test-server):
		"ci-ai-agent-wire",
		// AGENTIC_API coverage gate:
		"ci-agentic-api",
		// DEMO-P1 (golden demo tenant usage trickle):
		"demo-golden",
		// Demo complete test:
		"demo-complete-test",
		// Full descriptor-driven billable-unit coverage (2026-07-22):
		"coverage-all-dimensions",
	}
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for _, want := range expectedNames {
		if !have[want] {
			t.Errorf("expected built-in scenario %q is missing", want)
		}
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			data, err := scenarios.Read(name)
			if err != nil {
				t.Fatalf("scenarios.Read(%q): %v", name, err)
			}
			doc, err := LoadFromBytes(data)
			if err != nil {
				t.Fatalf("LoadFromBytes(%q): %v", name, err)
			}
			// Scenario.Name must match the file basename so list output and
			// scenario.show by name don't drift.
			if doc.Scenario.Name != name {
				t.Errorf("scenario %q has Name=%q; basename and Name must match", name, doc.Scenario.Name)
			}
			// Schema version pinned to current.
			if doc.Scenario.SchemaVersion != CurrentSchemaVersion {
				t.Errorf("scenario %q schema_version=%d; want %d",
					name, doc.Scenario.SchemaVersion, CurrentSchemaVersion)
			}
			if errs := Validate(doc); errs.HasErrors() {
				t.Errorf("scenario %q failed validation:\n%s", name, errs.Error())
			}
		})
	}
}

// TestGolden_MatrixBillingHas18BasePricingBillingCombos asserts the matrix
// scenario lives up to its name: every (pricing × billing) combo appears at
// least once across its archetypes.
func TestGolden_MatrixBillingCoversAllCombos(t *testing.T) {
	data, err := scenarios.Read("matrix-billing")
	if err != nil {
		t.Fatalf("Read(matrix-billing): %v", err)
	}
	doc, err := LoadFromBytes(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	type combo struct {
		Pricing PricingModel
		Billing BillingMode
	}
	have := make(map[combo]bool)
	for _, a := range doc.Scenario.Tenants.Archetypes {
		have[combo{a.PricingModel, a.BillingMode}] = true
	}
	missing := []combo{}
	for _, p := range AllPricingModels {
		for _, b := range AllBillingModes {
			if !have[combo{p, b}] {
				missing = append(missing, combo{p, b})
			}
		}
	}
	if len(missing) > 0 {
		t.Errorf("matrix-billing missing combos: %+v", missing)
	}
	// The contract says ≥30 archetypes (18 base + ≥12 variants).
	if got := len(doc.Scenario.Tenants.Archetypes); got < 30 {
		t.Errorf("matrix-billing has %d archetypes; want >= 30", got)
	}
}

// TestGolden_LifecycleStressEnablesLifecycle asserts the lifecycle profile
// is actually on with non-trivial rates — guards against a future edit that
// silently regresses the scenario's purpose.
func TestGolden_LifecycleStressEnablesLifecycle(t *testing.T) {
	data, err := scenarios.Read("lifecycle-stress")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	doc, err := LoadFromBytes(data)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !doc.Scenario.Lifecycle.Enabled {
		t.Error("lifecycle-stress must have lifecycle.enabled=true")
	}
	if doc.Scenario.Lifecycle.UpgradesPerHourPct == 0 &&
		doc.Scenario.Lifecycle.DowngradesPerHourPct == 0 &&
		doc.Scenario.Lifecycle.PauseResumePerHourPct == 0 &&
		doc.Scenario.Lifecycle.TrialConversionPerHourPct == 0 {
		t.Error("lifecycle-stress must drive at least one transition class")
	}
}
