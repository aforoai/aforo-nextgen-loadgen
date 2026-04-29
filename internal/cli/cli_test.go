package cli

import (
	"bytes"
	"strings"
	"testing"
)

// expectedSubcommands is the contract for Session 1 — change deliberately.
// If you add or rename a subcommand, update this list AND the README roadmap
// in the same commit.
var expectedSubcommands = []string{
	"doctor",
	"e2e",
	"lifecycle",
	"payments",
	"replay",
	"report",
	"run",
	"scenarios",
	"seed",
	"server",
	"validate",
	"version",
}

func TestRootHelpListsAllSubcommands(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"--help"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("--help returned error: %v", err)
	}
	out := buf.String()
	for _, name := range expectedSubcommands {
		if !strings.Contains(out, name) {
			t.Errorf("--help output missing subcommand %q\n--- output ---\n%s", name, out)
		}
	}
}

func TestEverySubcommandExitsZero(t *testing.T) {
	// Subcommands that are fully wired with real entry points need at least
	// one flag/argument to do non-error work. Map each one to invocation args
	// that should print useful output without hitting the network.
	//
	// run + replay are wired in Session 4. Calling them with --help is the
	// safest no-network path: cobra prints usage and exits 0.
	specialArgs := map[string][]string{
		"seed":   {"seed", "--scenario", "ci-smoke", "--dry-run"},
		"run":    {"run", "--help"},
		"replay": {"replay", "--help"},
	}
	for _, name := range expectedSubcommands {
		t.Run(name, func(t *testing.T) {
			cmd := NewRootCommand()
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			args := []string{name}
			if a, ok := specialArgs[name]; ok {
				args = a
			}
			cmd.SetArgs(args)

			if err := cmd.Execute(); err != nil {
				t.Fatalf("subcommand %q returned error: %v\noutput: %s", name, err, buf.String())
			}
			if buf.Len() == 0 {
				t.Errorf("subcommand %q produced no output; expected status message", name)
			}
		})
	}
}

func TestStubsAdvertiseSession(t *testing.T) {
	// Every stub except `version` and the fully-implemented subcommands must
	// announce the session it's slated for so users running pre-release
	// builds know what to expect.
	//
	// Implemented in Session 2: scenarios (parent + 4 sub-leaves).
	// Implemented in Session 3: seed.
	// Implemented in Session 4: run, replay.
	stubs := []string{
		"doctor", "e2e", "lifecycle", "payments", "report",
		"server", "validate",
	}
	for _, name := range stubs {
		t.Run(name, func(t *testing.T) {
			cmd := NewRootCommand()
			var buf bytes.Buffer
			cmd.SetOut(&buf)
			cmd.SetErr(&buf)
			cmd.SetArgs([]string{name})

			if err := cmd.Execute(); err != nil {
				t.Fatalf("subcommand %q returned error: %v", name, err)
			}
			out := buf.String()
			if !strings.Contains(out, "not yet implemented") {
				t.Errorf("stub %q must contain \"not yet implemented\"; got: %s", name, out)
			}
			if !strings.Contains(out, "Session ") {
				t.Errorf("stub %q must reference a Session number; got: %s", name, out)
			}
		})
	}
}

func TestVersionSubcommandPrintsBuildMetadata(t *testing.T) {
	cmd := NewRootCommand()
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)
	cmd.SetArgs([]string{"version"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("version returned error: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"aforo-loadgen", "commit", "built"} {
		if !strings.Contains(out, want) {
			t.Errorf("version output missing %q; got: %s", want, out)
		}
	}
}

func TestGlobalFlagsAreRegistered(t *testing.T) {
	cmd := NewRootCommand()
	for _, name := range []string{"target", "config", "log-level", "json-logs"} {
		if cmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("global flag --%s is not registered on root", name)
		}
	}
}
