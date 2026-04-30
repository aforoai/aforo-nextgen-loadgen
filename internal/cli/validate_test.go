package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/validate"
)

// TestValidate_ReportSubcommands_RoundTrip writes a synthetic run output
// directory + manifest, runs validate-then-report through the package
// internals (avoiding os.Exit by calling the work functions directly),
// and asserts: validation.json + report.html both written, no FAIL.
//
// This is the integration test for Session 5: the same flow CI runs
// against ci-smoke output.
func TestValidate_ReportSubcommands_RoundTrip(t *testing.T) {
	tmp := t.TempDir()

	// Write run.json
	rr := &runner.RunResult{
		RunID:           "rt-1",
		ScenarioName:    "ci-smoke",
		Target:          "local",
		StartedAt:       time.Now().Add(-1 * time.Minute).UTC(),
		StoppedAt:       time.Now().UTC(),
		Duration:        time.Minute,
		TargetTPS:       50,
		EventsGenerated: 100,
		EventsSubmitted: 100,
		EventsSucceeded: 100,
		PerTenant:       map[string]int64{"t-X": 100},
		PerArchetype:    map[string]int64{"ar-X": 100},
		LatencyP50ms:    1.0,
	}
	mustWriteJSON(t, filepath.Join(tmp, "run.json"), rr)

	// Write scenario.yaml — minimal valid v1 schema.
	scenarioYAML := `schema_version: 1
name: ci-smoke
target_tps: 50
duration: 60s
seed: 1
tenants:
  count: 1
  archetypes:
    - name: ar-X
      weight: 1.0
      pricing_model: PER_UNIT
      billing_mode: POSTPAID
      product_types: [API]
      customer_count: 1
      currency_mix: { USD: 1.0 }
      subscription_state_mix: { ACTIVE: 1.0 }
      rate_config:
        per_unit_rate_usd: 0.001
product_mix: { API: 1.0 }
ingestion_paths: { rest_direct: 1.0 }
payload_variation: { small_pct: 1.0 }
assertions:
  events_lost_max: 0
  cross_tenant_leakage_max: 0
`
	mustWrite(t, filepath.Join(tmp, "scenario.yaml"), []byte(scenarioYAML))

	// Manifest
	mf := &seed.Manifest{
		ManifestVersion: seed.ManifestVersion,
		RunID:           "rt-1",
		Target:          "local",
		Scenario:        "ci-smoke",
		CreatedAt:       time.Now().UTC(),
		Tenants:         []seed.ManifestTenant{{TenantID: "t-X", Archetype: "ar-X"}},
	}
	manifestPath := filepath.Join(tmp, "manifest.json")
	if _, err := mf.Save(manifestPath); err != nil {
		t.Fatal(err)
	}

	// Run validate via the work function (skipping the os.Exit branch).
	out := &bytes.Buffer{}
	errOut := &bytes.Buffer{}
	flags := &validateFlags{
		runOutput:    tmp,
		target:       "local",
		manifest:     manifestPath,
		tolerancePct: 0.001,
	}
	// runValidate will call os.Exit on FAIL; since this is a happy-path
	// run, it should return nil without exiting.
	if err := runValidate(context.Background(), out, errOut, flags); err != nil {
		t.Fatalf("validate: %v", err)
	}

	// validation.json was written.
	valPath := filepath.Join(tmp, "validation.json")
	report, err := validate.LoadValidationReport(valPath)
	if err != nil {
		t.Fatalf("load report: %v", err)
	}
	if report.Summary.Failed > 0 {
		t.Fatalf("expected no FAILs, got %d", report.Summary.Failed)
	}
	if !strings.Contains(out.String(), "summary:") {
		t.Fatal("expected validate stdout to include 'summary:'")
	}

	// Now run report.
	out.Reset()
	errOut.Reset()
	if err := runReport(context.Background(), out, errOut, &reportFlags{runOutput: tmp}); err != nil {
		t.Fatalf("report: %v", err)
	}
	htmlPath := filepath.Join(tmp, "report.html")
	if _, err := os.Stat(htmlPath); err != nil {
		t.Fatalf("report.html not written: %v", err)
	}
}

func mustWriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, path, data)
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil { // #nosec G306 — test temp file
		t.Fatal(err)
	}
}
