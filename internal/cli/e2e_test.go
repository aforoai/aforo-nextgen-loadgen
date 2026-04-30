package cli

import (
	"bytes"
	"strings"
	"testing"
)

// TestE2E_RequiresScenario asserts the CLI surfaces a helpful error when
// --scenario is omitted. We cover this at the CLI layer (not the
// orchestrator) because scenario validation is a flag-parsing concern.
func TestE2E_RequiresScenario(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"e2e", "--target", "local"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when --scenario is missing; got nil")
	}
	if !strings.Contains(err.Error(), "scenario") {
		t.Errorf("error should mention --scenario; got: %v", err)
	}
}

// TestE2E_RequiresAdminToken asserts the CLI fails fast when
// AFORO_ADMIN_TOKEN is empty. Doctor itself can run without a token
// (auth checks SKIP) but seed/run require it; e2e fails early so the
// operator gets a clear remedy without waiting for doctor to finish.
func TestE2E_RequiresAdminToken(t *testing.T) {
	t.Setenv("AFORO_ADMIN_TOKEN", "")
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"e2e", "--scenario", "ci-smoke", "--target", "local"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error when AFORO_ADMIN_TOKEN is empty; got nil")
	}
	if !strings.Contains(err.Error(), "AFORO_ADMIN_TOKEN") {
		t.Errorf("error should mention AFORO_ADMIN_TOKEN; got: %v", err)
	}
}

// TestE2E_PauseResumeDelayParsing asserts an invalid duration string
// for --pause-resume-delay is reported with a useful error.
func TestE2E_PauseResumeDelayParsing(t *testing.T) {
	t.Setenv("AFORO_ADMIN_TOKEN", "tok")
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{
		"e2e",
		"--scenario", "ci-smoke",
		"--target", "local",
		"--pause-resume-delay", "not-a-duration",
	})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for malformed --pause-resume-delay")
	}
	if !strings.Contains(err.Error(), "pause-resume-delay") {
		t.Errorf("error should name the offending flag; got: %v", err)
	}
}

// TestDoctor_ShowsSummaryLine asserts doctor's --help output mentions
// the summary line so the contract with the orchestrator's parser holds.
func TestDoctor_HelpExitsZero(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"doctor", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("doctor --help should exit 0; got %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "--target") {
		t.Errorf("doctor --help should advertise --target; got %q", out)
	}
	if !strings.Contains(out, "--token-env") {
		t.Errorf("doctor --help should advertise --token-env; got %q", out)
	}
}

// TestE2E_HelpAdvertisesAllFlags asserts the headline flags are visible
// — operators learn about --include-billing/--include-lifecycle/--keep-data
// from this output.
func TestE2E_HelpAdvertisesAllFlags(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"e2e", "--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("e2e --help should exit 0; got %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"--scenario", "--target", "--include-billing",
		"--include-lifecycle", "--keep-data", "--out",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("e2e --help should advertise %s; got %q", want, out)
		}
	}
}
