package driver

import (
	"strings"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

func clearDriverEnv(t *testing.T) {
	t.Helper()
	t.Setenv(aforo.AllowProductionEnv, "")
	for _, d := range envURLDrivers {
		t.Setenv(d.envVar, "")
	}
}

// Every env-URL driver read by a constructor must be visible to the
// production guard, whatever the run's target is.
func TestGuardProductionCatchesEnvURLDrivers(t *testing.T) {
	for _, d := range envURLDrivers {
		t.Run(d.name, func(t *testing.T) {
			clearDriverEnv(t)
			if err := aforo.GuardProduction(aforo.LocalTarget, false); err != nil {
				t.Fatalf("local target with no driver env must pass: %v", err)
			}
			for _, prodURL := range []string{
				"https://usage-ingestor.aforo.ai/v1/ingest",
				"https://usage-ingestor.aforo.ai./v1/ingest", // trailing-dot FQDN
				"https://MCP.Aforo.AI:8443/mcp",
			} {
				t.Setenv(d.envVar, prodURL)
				err := aforo.GuardProduction(aforo.LocalTarget, false)
				if err == nil {
					t.Fatalf("%s=%s must be refused without --allow-production", d.envVar, prodURL)
				}
				if !strings.Contains(err.Error(), d.envVar) {
					t.Fatalf("refusal should name %s: %v", d.envVar, err)
				}
				if err := aforo.GuardProduction(aforo.LocalTarget, true); err != nil {
					t.Fatalf("--allow-production must pass: %v", err)
				}
			}
			t.Setenv(d.envVar, "http://localhost:8090")
			if err := aforo.GuardProduction(aforo.LocalTarget, false); err != nil {
				t.Fatalf("non-production driver URL must pass: %v", err)
			}
		})
	}
}

// The guard must check the same URL the driver uses: drive each
// constructor from the env var and compare.
func TestEnvURLDriversReadRegisteredVariable(t *testing.T) {
	clearDriverEnv(t)
	t.Setenv(AIAgentRESTEnvURL, "http://rest.example.test/v1/ingest")
	t.Setenv(MCPJsonRPCEnvURL, "http://mcp.example.test/mcp")
	t.Setenv(AIAgentWireEnvURL, "http://wire.example.test")

	rest, err := NewAIAgentREST(AIAgentRESTConfig{})
	if err != nil {
		t.Fatal(err)
	}
	mcp, err := NewMCPJsonRPC(MCPJsonRPCConfig{})
	if err != nil {
		t.Fatal(err)
	}
	wire, err := NewAIAgentWire(AIAgentWireConfig{})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{"ai_agent_rest": rest.url, "mcp_jsonrpc": mcp.url, "ai_agent_wire": wire.url}

	dests := EnvDestinations()
	if len(dests) != len(envURLDrivers) {
		t.Fatalf("want %d env destinations, got %v", len(envURLDrivers), dests)
	}
	for _, dst := range dests {
		name := strings.SplitN(dst.Source, " ", 2)[0]
		if got[name] != strings.TrimRight(dst.URL, "/") && got[name] != dst.URL {
			t.Errorf("%s: driver uses %q but guard checks %q", name, got[name], dst.URL)
		}
	}
}

func TestEnvURLDriversAreKnownPaths(t *testing.T) {
	for _, d := range envURLDrivers {
		if !IsKnown(d.name) {
			t.Errorf("envURLDrivers lists %q, which is not a registered ingestion path", d.name)
		}
	}
}

func TestEnvDriverURLRejectsUnregisteredVariable(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("reading an unregistered env var must panic")
		}
	}()
	_ = envDriverURL("", "AFORO_LOADGEN_SOMETHING_NEW_URL")
}
