package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// TestSeedRefusesProductionTargets locks the guard on the command that
// creates tenants: --target staging resolves to the production hosts, and
// the June 2026 runs recorded as "staging" seeded production that way.
func TestSeedRefusesProductionTargets(t *testing.T) {
	t.Setenv(aforo.AllowProductionEnv, "")
	t.Setenv("AFORO_CI_BASE_URL", "")
	for _, target := range []string{"staging", "prod", "https://usage-ingestor.aforo.ai"} {
		t.Run(target, func(t *testing.T) {
			cmd := NewRootCommand()
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{"seed", "--scenario", "ci-smoke", "--target", target,
				"--out", filepath.Join(t.TempDir(), "manifest.json")})
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), "refusing to send traffic to production") {
				t.Fatalf("want production refusal, got err=%v output=%s", err, buf.String())
			}
		})
	}
}

// TestSeedDryRunAllowedAgainstStaging: a dry run sends nothing, so it must
// not be blocked.
func TestSeedDryRunAllowedAgainstStaging(t *testing.T) {
	t.Setenv(aforo.AllowProductionEnv, "")
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"seed", "--scenario", "ci-smoke", "--target", "staging", "--dry-run",
		"--out", filepath.Join(t.TempDir(), "manifest.json")})
	if err := cmd.Execute(); err != nil && strings.Contains(err.Error(), "refusing to send traffic to production") {
		t.Fatalf("dry run must not be refused: %v", err)
	}
}

// TestRunRefusesHighTPSWithoutAcknowledgement mirrors the trigger server's
// acknowledge_high_tps rail on the CLI.
func TestRunRefusesHighTPSWithoutAcknowledgement(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "manifest.json")
	seedCmd := NewRootCommand()
	var sb bytes.Buffer
	seedCmd.SetOut(&sb)
	seedCmd.SetErr(&sb)
	seedCmd.SetArgs([]string{"seed", "--scenario", "ci-smoke", "--target", "local", "--dry-run", "--out", manifest})
	if err := seedCmd.Execute(); err != nil {
		t.Fatalf("dry-run seed: %v\n%s", err, sb.String())
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "scenarios", "ci-smoke.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	high := regexp.MustCompile(`(?m)^target_tps:\s*\d+`).ReplaceAll(raw, []byte("target_tps: 2000"))
	scen := filepath.Join(dir, "high.yaml")
	if err := os.WriteFile(scen, high, 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"run", "--scenario", scen, "--manifest", manifest, "--target", "local", "--duration", "1s", "--metrics-addr", ""})
	err = cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--i-know-what-im-doing") {
		t.Fatalf("want high-TPS refusal, got err=%v", err)
	}
}
