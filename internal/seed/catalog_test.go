package seed

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// validCatalog is a minimal but fully-valid Northwind-style catalog covering
// the structural core + one tiered plan, used to assert the happy path.
const validCatalog = `{
  "version": "1.0.0",
  "company": { "name": "Northwind AI", "currency": "USD" },
  "billableUnits": [
    { "key": "api-calls", "name": "API Calls", "unit": "calls", "aggregation": "COUNT" },
    { "key": "tokens-processed", "name": "Tokens Processed", "unit": "tokens", "aggregation": "SUM" }
  ],
  "products": [
    { "key": "translation-api", "name": "Translation API", "type": "API", "billableUnits": ["api-calls", "tokens-processed"] }
  ],
  "ratePlans": [
    { "key": "starter", "name": "Starter", "pricingModel": "FLAT_RATE", "product": "translation-api", "primaryUnit": "api-calls", "currency": "USD", "config": { "flatFeeUsd": 49.0, "billingTiming": "IN_ADVANCE" } },
    { "key": "growth", "name": "Growth", "pricingModel": "GRADUATED", "product": "translation-api", "primaryUnit": "api-calls", "currency": "USD",
      "config": { "billingTiming": "IN_ARREARS", "tiers": [
        { "tierStart": 0, "tierEnd": 100000, "unitPriceUsd": 0.002 },
        { "tierStart": 100000, "tierEnd": null, "unitPriceUsd": 0.001 }
      ] } }
  ],
  "offerings": [
    { "key": "translation-growth-monthly", "name": "Translation API — Growth (Monthly)", "billingMode": "POSTPAID", "ratePlan": "growth" }
  ],
  "customers": [
    { "key": "acme-robotics", "name": "Acme Robotics", "email": "billing@acmerobotics.com", "status": "ACTIVE" }
  ],
  "subscriptions": [
    { "key": "sub-acme-growth", "name": "Acme Robotics — Growth (Active)", "customer": "acme-robotics", "offering": "translation-growth-monthly", "planLabel": "Growth", "lifecycleState": "ACTIVE", "startedDaysAgo": 90 }
  ]
}`

func TestParseCatalog_Valid(t *testing.T) {
	cat, err := ParseCatalog([]byte(validCatalog))
	if err != nil {
		t.Fatalf("expected valid catalog to parse, got: %v", err)
	}
	if cat.Company.Name != "Northwind AI" {
		t.Errorf("company.name = %q, want Northwind AI", cat.Company.Name)
	}
	if got := len(cat.Products); got != 1 {
		t.Errorf("products = %d, want 1", got)
	}
	if got := len(cat.RatePlans); got != 2 {
		t.Errorf("ratePlans = %d, want 2", got)
	}
	if cat.Subscriptions[0].LifecycleState != "ACTIVE" {
		t.Errorf("sub state = %q, want ACTIVE", cat.Subscriptions[0].LifecycleState)
	}
}

// Each fixture mutates the valid catalog to trip exactly one validation rule.
func TestValidate_RejectsBadCatalogs(t *testing.T) {
	cases := []struct {
		name      string
		json      string
		wantInErr string
	}{
		{
			name:      "unknown offering→ratePlan ref",
			json:      strings.Replace(validCatalog, `"ratePlan": "growth"`, `"ratePlan": "nonexistent"`, 1),
			wantInErr: "ratePlan ref \"nonexistent\" not found",
		},
		{
			name:      "invalid product type",
			json:      strings.Replace(validCatalog, `"type": "API"`, `"type": "WIDGET"`, 1),
			wantInErr: "invalid type",
		},
		{
			name:      "synthetic product name (Req #1)",
			json:      strings.Replace(validCatalog, `"name": "Translation API"`, `"name": "Loadgen Product API"`, 1),
			wantInErr: "synthetic name",
		},
		{
			name:      "synthetic customer name (Customer N style → prod_ prefix)",
			json:      strings.Replace(validCatalog, `"name": "Acme Robotics"`, `"name": "prod_7"`, 1),
			wantInErr: "synthetic name",
		},
		{
			name:      "GRADUATED top tier not open-ended",
			json:      strings.Replace(validCatalog, `{ "tierStart": 100000, "tierEnd": null, "unitPriceUsd": 0.001 }`, `{ "tierStart": 100000, "tierEnd": 200000, "unitPriceUsd": 0.001 }`, 1),
			wantInErr: "top tier must be open-ended",
		},
		{
			name:      "FLAT_RATE missing flatFeeUsd",
			json:      strings.Replace(validCatalog, `"config": { "flatFeeUsd": 49.0, "billingTiming": "IN_ADVANCE" }`, `"config": { "billingTiming": "IN_ADVANCE" }`, 1),
			wantInErr: "FLAT_RATE requires config.flatFeeUsd > 0",
		},
		{
			name:      "bad subscription lifecycle state",
			json:      strings.Replace(validCatalog, `"lifecycleState": "ACTIVE"`, `"lifecycleState": "ZOMBIE"`, 1),
			wantInErr: "invalid lifecycleState",
		},
		{
			name:      "unknown field (typo guard)",
			json:      strings.Replace(validCatalog, `"company": { "name": "Northwind AI", "currency": "USD" }`, `"company": { "name": "Northwind AI", "currancy": "USD" }`, 1),
			wantInErr: "parse catalog",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte(tc.json))
			if err == nil {
				t.Fatalf("expected rejection, got nil error")
			}
			if !strings.Contains(err.Error(), tc.wantInErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tc.wantInErr)
			}
		})
	}
}

func TestToRateConfig_TierMapping(t *testing.T) {
	cat, err := ParseCatalog([]byte(validCatalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	var growth CatalogRatePlan
	for _, rp := range cat.RatePlans {
		if rp.Key == "growth" {
			growth = rp
		}
	}
	rc := growth.ToRateConfig()
	if len(rc.GraduatedTiers) != 2 {
		t.Fatalf("graduatedTiers = %d, want 2", len(rc.GraduatedTiers))
	}
	if rc.GraduatedTiers[0].UpToUnits != 100000 || rc.GraduatedTiers[0].UnitPriceUSD != 0.002 {
		t.Errorf("tier[0] = %+v, want UpToUnits=100000 UnitPriceUSD=0.002", rc.GraduatedTiers[0])
	}
	if rc.GraduatedTiers[1].UpToUnits != 0 { // open-ended top tier → 0
		t.Errorf("tier[1].UpToUnits = %d, want 0 (open-ended)", rc.GraduatedTiers[1].UpToUnits)
	}
	if len(rc.VolumeTiers) != 0 {
		t.Errorf("GRADUATED plan should not populate VolumeTiers, got %d", len(rc.VolumeTiers))
	}
}

// TestRealGoldenCatalog opportunistically validates the actual shipped catalog
// in the sibling aforo-nextgen-docker repo when this is run inside the full
// workspace. Skips cleanly in CI where only the loadgen repo is checked out.
func TestRealGoldenCatalog(t *testing.T) {
	path := filepath.Join("..", "..", "..", "aforo-nextgen-docker", "demo", "demo-seed-catalog.json")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("real golden catalog not present (%s) — skipping cross-repo validation", path)
	}
	cat, err := LoadCatalog(path)
	if err != nil {
		t.Fatalf("real demo-seed-catalog.json failed validation: %v", err)
	}
	if len(cat.Products) == 0 || len(cat.Customers) == 0 || len(cat.Subscriptions) == 0 {
		t.Fatalf("real catalog looks empty: products=%d customers=%d subs=%d",
			len(cat.Products), len(cat.Customers), len(cat.Subscriptions))
	}
	t.Logf("real golden catalog OK: %d products, %d units, %d rate plans, %d offerings, %d customers, %d subscriptions",
		len(cat.Products), len(cat.BillableUnits), len(cat.RatePlans), len(cat.Offerings), len(cat.Customers), len(cat.Subscriptions))
}
