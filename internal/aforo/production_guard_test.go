package aforo

import (
	"strings"
	"testing"
)

func TestProductionHosts(t *testing.T) {
	cases := []struct {
		name   string
		target Target
		want   []string
	}{
		{"local", LocalTarget, nil},
		{"demo", DemoTarget, nil},
		{"prod", ProdTarget, []string{"billing.aforo.ai", "catalog.aforo.ai", "customer.aforo.ai", "org.aforo.ai", "pricing.aforo.ai", "storefront.aforo.ai", "usage-ingestor.aforo.ai", "analytics.aforo.ai"}},
		{"staging mirrors prod", StagingTarget, []string{"x"}},
		{"apex", Target{Name: "apex", URLs: map[Service]string{ServiceUsageIngestor: "https://aforo.ai"}}, []string{"aforo.ai"}},
		{"lookalike is not prod", Target{Name: "x", URLs: map[Service]string{ServiceUsageIngestor: "https://notaforo.ai"}}, nil},
		{"bench host", Target{Name: "x", URLs: map[Service]string{ServiceUsageIngestor: "https://ingest.bench.example.com"}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.target.ProductionHosts()
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("want no production hosts, got %v", got)
				}
				return
			}
			if len(got) == 0 {
				t.Fatalf("want production hosts, got none")
			}
		})
	}
}

func TestGuardProduction(t *testing.T) {
	t.Setenv(AllowProductionEnv, "")
	t.Setenv("AFORO_CI_BASE_URL", "")
	t.Setenv("AFORO_CI_USAGE_INGESTOR_URL", "")
	if err := GuardProduction(LocalTarget, false); err != nil {
		t.Fatalf("local must pass: %v", err)
	}
	for _, tgt := range []Target{StagingTarget, ProdTarget, CITarget()} {
		err := GuardProduction(tgt, false)
		if err == nil {
			t.Fatalf("%s must be refused without acknowledgement", tgt.Name)
		}
		if !strings.Contains(err.Error(), "usage-ingestor.aforo.ai") {
			t.Fatalf("error should name the hosts: %v", err)
		}
		if err := GuardProduction(tgt, true); err != nil {
			t.Fatalf("%s with --allow-production must pass: %v", tgt.Name, err)
		}
	}
	t.Setenv(AllowProductionEnv, "true")
	if err := GuardProduction(ProdTarget, false); err == nil {
		t.Fatal(`env value "true" must not count as acknowledgement`)
	}
	t.Setenv(AllowProductionEnv, AllowProductionValue)
	if err := GuardProduction(ProdTarget, false); err != nil {
		t.Fatalf("exact env phrase must pass: %v", err)
	}
}

func TestGuardProductionCIWithOverride(t *testing.T) {
	t.Setenv(AllowProductionEnv, "")
	t.Setenv("AFORO_CI_BASE_URL", "https://bench-ingress.internal.example.com")
	if err := GuardProduction(CITarget(), false); err != nil {
		t.Fatalf("ci target pointed at a non-production ingress must pass: %v", err)
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"https://Usage-Ingestor.AFORO.ai/v1/ingest": "usage-ingestor.aforo.ai",
		"https://usage-ingestor.aforo.ai./v1":       "usage-ingestor.aforo.ai",
		"https://usage-ingestor.aforo.ai..:8443/x":  "usage-ingestor.aforo.ai",
		"usage-ingestor.aforo.ai.:8084":             "usage-ingestor.aforo.ai",
		"aforo.ai":                                  "aforo.ai",
		"http://[::1]:8084/mcp":                     "::1",
		"  ":                                        "",
		"http://":                                   "",
	}
	for in, want := range cases {
		if got := NormalizeHost(in); got != want {
			t.Errorf("NormalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestProductionHostsTrailingDot(t *testing.T) {
	for _, raw := range []string{
		"https://usage-ingestor.aforo.ai.",
		"https://usage-ingestor.aforo.ai./v1/ingest",
		"https://AFORO.AI.:443",
	} {
		tgt := Target{Name: "x", URLs: map[Service]string{ServiceUsageIngestor: raw}}
		if len(tgt.ProductionHosts()) != 1 {
			t.Errorf("%q must be detected as production, got %v", raw, tgt.ProductionHosts())
		}
	}
	// A lookalike with a trailing dot is still not production.
	tgt := Target{Name: "x", URLs: map[Service]string{ServiceUsageIngestor: "https://notaforo.ai."}}
	if hosts := tgt.ProductionHosts(); len(hosts) != 0 {
		t.Errorf("lookalike must not match, got %v", hosts)
	}
}

func TestGuardProductionDestinations(t *testing.T) {
	t.Setenv(AllowProductionEnv, "")
	local := []Destination{{Source: "target usage-ingestor", URL: "http://localhost:8084"}}
	if err := GuardProductionDestinations("local", local, false); err != nil {
		t.Fatalf("local destinations must pass: %v", err)
	}
	withEnv := append(append([]Destination(nil), local...), Destination{Source: "ai_agent_rest (AFORO_LOADGEN_INGEST_URL)", URL: "https://usage-ingestor.aforo.ai./v1/ingest"})
	err := GuardProductionDestinations("local", withEnv, false)
	if err == nil {
		t.Fatal("a production env destination must be refused even when the target is local")
	}
	if !strings.Contains(err.Error(), "AFORO_LOADGEN_INGEST_URL") || !strings.Contains(err.Error(), "usage-ingestor.aforo.ai") {
		t.Fatalf("error should name the host and its source: %v", err)
	}
	if err := GuardProductionDestinations("local", withEnv, true); err != nil {
		t.Fatalf("--allow-production must pass: %v", err)
	}
}

func TestGuardProductionChecksRegisteredSources(t *testing.T) {
	t.Setenv(AllowProductionEnv, "")
	destSourcesMu.Lock()
	saved := destSources
	destSources = nil
	destSourcesMu.Unlock()
	t.Cleanup(func() {
		destSourcesMu.Lock()
		destSources = saved
		destSourcesMu.Unlock()
	})

	url := "http://localhost:8080/mcp"
	RegisterDestinationSource(func() []Destination {
		return []Destination{{Source: "mcp_jsonrpc (AFORO_LOADGEN_MCP_URL)", URL: url}}
	})
	if err := GuardProduction(LocalTarget, false); err != nil {
		t.Fatalf("local env destination must pass: %v", err)
	}
	// Sources are evaluated at guard time, not registration time.
	url = "https://mcp.aforo.ai/mcp"
	if err := GuardProduction(LocalTarget, false); err == nil {
		t.Fatal("registered production destination must be refused")
	}
}
