package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// runCmd is a tiny helper: build a fresh root command, set args, capture
// stdout/stderr separately, return both plus the error.
func runCmd(t *testing.T, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := NewRootCommand()
	var outBuf, errBuf bytes.Buffer
	cmd.SetOut(&outBuf)
	cmd.SetErr(&errBuf)
	cmd.SetArgs(args)
	err = cmd.Execute()
	return outBuf.String(), errBuf.String(), err
}

func TestScenarios_List_PrintsAllSixBuiltins(t *testing.T) {
	out, _, err := runCmd(t, "scenarios", "list")
	if err != nil {
		t.Fatalf("scenarios list: %v", err)
	}
	for _, want := range []string{
		"ci-smoke",
		"crawl-e2e",
		"lifecycle-stress",
		"matrix-billing",
		"run-15k-7day",
		"walk-realistic-50t",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scenarios list missing %q\nout:\n%s", want, out)
		}
	}
	for _, want := range []string{"NAME", "TARGET TPS", "DURATION", "TENANTS", "ARCHETYPES"} {
		if !strings.Contains(out, want) {
			t.Errorf("scenarios list missing header %q\nout:\n%s", want, out)
		}
	}
}

func TestScenarios_Validate_BuiltInPasses(t *testing.T) {
	// Write the built-in ci-smoke YAML to disk and validate via the CLI.
	dir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("..", "..", "scenarios", "ci-smoke.yaml"))
	if err != nil {
		t.Fatalf("read source ci-smoke.yaml: %v", err)
	}
	dst := filepath.Join(dir, "ci-smoke.yaml")
	if err := os.WriteFile(dst, src, 0o600); err != nil {
		t.Fatalf("write tmp ci-smoke.yaml: %v", err)
	}

	out, _, err := runCmd(t, "scenarios", "validate", dst)
	if err != nil {
		t.Fatalf("scenarios validate %s: %v", dst, err)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("expected 'ok' in stdout; got: %s", out)
	}
}

func TestScenarios_Validate_FailsLoudlyOnStaleKeysWithoutCancelled(t *testing.T) {
	dir := t.TempDir()
	bad := []byte(`schema_version: 1
name: bad-stale
target_tps: 10
duration: 30s
tenants:
  count: 1
  archetypes:
    - name: only-active
      weight: 1.0
      pricing_model: PER_UNIT
      billing_mode: POSTPAID
      product_types: [API]
      customer_count: 5
      subscription_state_mix: { ACTIVE: 1.0 }
      rate_config:
        per_unit_rate_usd: 0.001
negative_paths:
  stale_keys_pct: 0.01
`)
	dst := filepath.Join(dir, "bad-stale.yaml")
	if err := os.WriteFile(dst, bad, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	_, errOut, err := runCmd(t, "scenarios", "validate", dst)
	if err == nil {
		t.Errorf("expected non-nil error for invalid scenario")
	}
	// The error must include the exact path so a CI failure is actionable.
	if !strings.Contains(errOut, "negative_paths.stale_keys_pct") {
		t.Errorf("error missing path; got:\n%s", errOut)
	}
	if !strings.Contains(errOut, "CANCELLED or EXPIRED") {
		t.Errorf("error missing cross-field message; got:\n%s", errOut)
	}
}

func TestScenarios_Validate_FailsOnMissingFile(t *testing.T) {
	_, _, err := runCmd(t, "scenarios", "validate", filepath.Join(t.TempDir(), "no-such-file.yaml"))
	if err == nil {
		t.Errorf("validate missing-file should error")
	}
}

func TestScenarios_Show_PrintsKnown(t *testing.T) {
	out, _, err := runCmd(t, "scenarios", "show", "ci-smoke")
	if err != nil {
		t.Fatalf("scenarios show ci-smoke: %v", err)
	}
	if !strings.Contains(out, "schema_version: 1") {
		t.Errorf("show output missing schema_version: 1\n%s", out)
	}
	if !strings.Contains(out, "name: ci-smoke") {
		t.Errorf("show output missing scenario name\n%s", out)
	}
}

func TestScenarios_Show_UnknownReturnsHelpfulError(t *testing.T) {
	_, _, err := runCmd(t, "scenarios", "show", "no-such-scenario")
	if err == nil {
		t.Fatal("scenarios show <unknown>: want error, got nil")
	}
	if !strings.Contains(err.Error(), "scenarios list") {
		t.Errorf("error should suggest `scenarios list`; got %q", err)
	}
}

func TestScenarios_Archetypes_PrintsAllForWalkScenario(t *testing.T) {
	out, _, err := runCmd(t, "scenarios", "archetypes", "walk-realistic-50t")
	if err != nil {
		t.Fatalf("scenarios archetypes walk-realistic-50t: %v", err)
	}
	// All 8 archetype names must appear.
	for _, want := range []string{
		"walk-api-postpaid-perunit",
		"walk-api-prepaid-flat",
		"walk-aiagent-postpaid-quota",
		"walk-aiagent-hybrid-graduated",
		"walk-mcp-postpaid-perunit",
		"walk-mcp-prepaid-volume",
		"walk-agentic-postpaid-percentage",
		"walk-agentic-hybrid-flat",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("archetypes output missing %q\nout:\n%s", want, out)
		}
	}
	// Header columns appear once.
	for _, want := range []string{"NAME", "WEIGHT", "PRICING", "BILLING", "PRODUCT TYPES", "CUSTOMERS"} {
		if !strings.Contains(out, want) {
			t.Errorf("archetypes output missing header %q", want)
		}
	}
}

func TestScenarios_Archetypes_UnknownReturnsHelpfulError(t *testing.T) {
	_, _, err := runCmd(t, "scenarios", "archetypes", "no-such-scenario")
	if err == nil {
		t.Fatal("archetypes <unknown>: want error, got nil")
	}
	if !strings.Contains(err.Error(), "scenarios list") {
		t.Errorf("error should suggest `scenarios list`; got %q", err)
	}
}

func TestScenarios_BareParentPrintsHelp(t *testing.T) {
	// `aforo-loadgen scenarios` (no sub) should print Cobra-generated help
	// and exit 0 — same shape as TestEverySubcommandExitsZero requires.
	out, _, err := runCmd(t, "scenarios")
	if err != nil {
		t.Fatalf("scenarios (no args): %v", err)
	}
	for _, want := range []string{"list", "validate", "show", "archetypes"} {
		if !strings.Contains(out, want) {
			t.Errorf("`scenarios` help missing leaf %q\nout:\n%s", want, out)
		}
	}
}
