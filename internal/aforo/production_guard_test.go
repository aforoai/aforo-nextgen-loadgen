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
