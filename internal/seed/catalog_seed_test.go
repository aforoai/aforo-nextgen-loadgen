package seed

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// richCatalog exercises the full catalog-mode matrix: 3 pricing models
// (FLAT_RATE / PER_UNIT / INCLUDED_QUOTA), 3 billing modes (POSTPAID / PREPAID
// / HYBRID), and 4 lifecycle states (ACTIVE / CANCELLED / TRIALING / PAUSED) so
// every wallet / payment-method / api-key / transition branch in
// seedCatalogTenant runs. All names are human/real-world (Req #1).
const richCatalog = `{
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
    { "key": "pay-as-you-go", "name": "Pay-as-you-go", "pricingModel": "PER_UNIT", "product": "translation-api", "primaryUnit": "api-calls", "currency": "USD", "config": { "perUnitRateUsd": 0.002, "billingTiming": "IN_ARREARS" } },
    { "key": "enterprise", "name": "Enterprise", "pricingModel": "INCLUDED_QUOTA", "product": "translation-api", "primaryUnit": "tokens-processed", "currency": "USD", "config": { "platformFeeUsd": 2500.0, "includedFreeUnits": 50000000, "overageRateUsd": 0.0008, "billingTiming": "IN_ADVANCE" } }
  ],
  "offerings": [
    { "key": "translation-starter-monthly", "name": "Translation API — Starter (Monthly)", "billingMode": "POSTPAID", "ratePlan": "starter" },
    { "key": "translation-payg-prepaid", "name": "Translation API — Pay-as-you-go (Prepaid)", "billingMode": "PREPAID", "ratePlan": "pay-as-you-go" },
    { "key": "translation-enterprise-hybrid", "name": "Translation API — Enterprise (Hybrid)", "billingMode": "HYBRID", "ratePlan": "enterprise" }
  ],
  "customers": [
    { "key": "acme-robotics", "name": "Acme Robotics", "email": "billing@acmerobotics.com", "status": "ACTIVE", "companySize": "ENTERPRISE" },
    { "key": "vertex-health", "name": "Vertex Health", "email": "ap@vertexhealth.com", "status": "ACTIVE", "companySize": "MID" }
  ],
  "subscriptions": [
    { "key": "sub-acme-starter", "name": "Acme Robotics — Starter (Active)", "customer": "acme-robotics", "offering": "translation-starter-monthly", "planLabel": "Starter", "lifecycleState": "ACTIVE", "startedDaysAgo": 90 },
    { "key": "sub-acme-payg", "name": "Acme Robotics — Pay-as-you-go (Cancelled)", "customer": "acme-robotics", "offering": "translation-payg-prepaid", "planLabel": "Pay-as-you-go", "lifecycleState": "CANCELLED", "cancelledDaysAgo": 10 },
    { "key": "sub-vertex-enterprise", "name": "Vertex Health — Enterprise (Trialing)", "customer": "vertex-health", "offering": "translation-enterprise-hybrid", "planLabel": "Enterprise", "lifecycleState": "TRIALING", "trialEndsInDays": 14 },
    { "key": "sub-vertex-starter", "name": "Vertex Health — Starter (Paused)", "customer": "vertex-health", "offering": "translation-starter-monthly", "planLabel": "Starter", "lifecycleState": "PAUSED", "pausedDaysAgo": 5 }
  ]
}`

const goldenTestTenantID = "demo_golden_test"

// newCatalogSeeder wires a fake-backend Client + a catalog-mode Seeder for a
// parsed catalog. Returns the seeder + the call recorder so tests can assert
// the API call sequence + recorded bodies.
func newCatalogSeeder(t *testing.T, cat *DemoCatalog) (*Seeder, *callRecorder) {
	t.Helper()
	stubFakeBackendStripeEnv(t)
	server, calls := newFakeAforoServer(t)
	t.Cleanup(server.Close)

	c, err := NewClient(ClientConfig{
		Target:      singleHostTarget(server.URL),
		BearerToken: "test-token",
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	seeder, err := NewSeeder(SeederConfig{
		Client:        c,
		Scenario:      simpleScenario(t), // archetypes ignored in catalog-mode
		Catalog:       cat,
		ReuseTenantID: goldenTestTenantID,
		RunID:         "catalog-test-run",
		Logger:        log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	return seeder, calls
}

// TestCatalogMode_RequiresReuseTenantID asserts catalog-mode is fenced to one
// fixed tenant (NewSeeder rejects it without a tenant id).
func TestCatalogMode_RequiresReuseTenantID(t *testing.T) {
	cat, err := ParseCatalog([]byte(richCatalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, err := NewClient(ClientConfig{Target: singleHostTarget("http://unused"), DryRun: true, MinInterval: time.Microsecond})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := NewSeeder(SeederConfig{Client: c, Scenario: simpleScenario(t), Catalog: cat}); err == nil {
		t.Fatal("expected NewSeeder to reject catalog-mode without ReuseTenantID")
	}
}

// TestCatalogMode_FullChain drives the rich catalog through the full create
// chain against the fake backend and asserts the API call sequence + manifest.
func TestCatalogMode_FullChain(t *testing.T) {
	cat, err := ParseCatalog([]byte(richCatalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	seeder, calls := newCatalogSeeder(t, cat)

	res, err := seeder.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("catalog seed errors: %v", res.Errors)
	}

	// Catalog-mode reuses the golden tenant → it MUST NOT provision a tenant.
	for _, call := range calls.snapshot() {
		if call.method == "POST" && call.path == "/api/v1/internal/tenants" {
			t.Fatalf("catalog-mode must reuse the golden tenant; saw tenant POST %s", call.path)
		}
	}

	// Every major entity collection must be POSTed.
	requirePOST(t, calls, "/api/v1/products")
	requirePOST(t, calls, "/api/v1/metrics/bulk")
	requirePOST(t, calls, "/api/v1/rateplans")
	requirePOST(t, calls, "/api/v1/offerings")
	requirePOST(t, calls, "/api/v1/customers")
	requirePOST(t, calls, "/api/v1/subscriptions")
	requirePOST(t, calls, "/api/v1/api-keys")

	// CANCELLED → /cancel cascade; PAUSED → /pause.
	var sawCancel, sawPause bool
	for _, call := range calls.snapshot() {
		if call.method == "POST" && strings.Contains(call.path, "/cancel") {
			sawCancel = true
		}
		if call.method == "POST" && strings.Contains(call.path, "/pause") {
			sawPause = true
		}
	}
	if !sawCancel {
		t.Error("CANCELLED subscription produced no /cancel call")
	}
	if !sawPause {
		t.Error("PAUSED subscription produced no /pause call")
	}

	// Manifest shape.
	m := res.Manifest
	if len(m.Tenants) != 1 {
		t.Fatalf("manifest tenants = %d, want 1", len(m.Tenants))
	}
	tn := m.Tenants[0]
	if tn.TenantID != goldenTestTenantID {
		t.Errorf("tenant id = %q, want %q", tn.TenantID, goldenTestTenantID)
	}
	if len(tn.Products) != 1 {
		t.Errorf("products = %d, want 1", len(tn.Products))
	}
	if len(tn.RatePlans) != 3 {
		t.Errorf("ratePlans = %d, want 3", len(tn.RatePlans))
	}
	if len(tn.Offerings) != 3 {
		t.Errorf("offerings = %d, want 3", len(tn.Offerings))
	}
	if len(tn.Customers) != 2 {
		t.Errorf("customers = %d, want 2", len(tn.Customers))
	}
	totalSubs := 0
	for _, cu := range tn.Customers {
		totalSubs += len(cu.Subscriptions)
	}
	if totalSubs != 4 {
		t.Errorf("subscriptions = %d, want 4", totalSubs)
	}

	// Req #1: no synthetic display name reached the manifest.
	if got := tn.Products[0].Name; got != "Translation API" {
		t.Errorf("product name = %q, want %q", got, "Translation API")
	}
	for _, p := range tn.Products {
		if syntheticName.MatchString(p.Name) {
			t.Errorf("synthetic product name leaked into manifest: %q", p.Name)
		}
	}
	for _, rp := range tn.RatePlans {
		if syntheticName.MatchString(rp.Name) {
			t.Errorf("synthetic rate plan name leaked into manifest: %q", rp.Name)
		}
	}

	// TRIALING sub carries no API key; non-trialing subs each carry one.
	for _, cu := range tn.Customers {
		for _, sub := range cu.Subscriptions {
			if sub.Status == "TRIALING" {
				if len(sub.APIKeys) != 0 {
					t.Errorf("TRIALING sub %s should have no api key, got %d", sub.SubscriptionID, len(sub.APIKeys))
				}
			} else if len(sub.APIKeys) == 0 {
				t.Errorf("%s sub %s should carry an api key", sub.Status, sub.SubscriptionID)
			}
		}
	}
}

// TestCatalogMode_IncludedQuotaBaseFee asserts the INCLUDED_QUOTA platform fee
// is mapped onto the rate plan's baseFee in the create body (so an enterprise
// included-quota plan surfaces its recurring fee, not $0).
func TestCatalogMode_IncludedQuotaBaseFee(t *testing.T) {
	cat, err := ParseCatalog([]byte(richCatalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	seeder, calls := newCatalogSeeder(t, cat)
	if _, err := seeder.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	var enterpriseBaseFee float64
	var found bool
	for _, call := range calls.snapshot() {
		if call.method != "POST" || call.path != "/api/v1/rateplans" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(call.body, &body); err != nil {
			continue
		}
		if name, _ := body["name"].(string); name == "Enterprise" {
			found = true
			if bf, ok := body["baseFee"].(float64); ok {
				enterpriseBaseFee = bf
			}
		}
	}
	if !found {
		t.Fatal("no Enterprise rate plan POST recorded")
	}
	if enterpriseBaseFee != 2500.0 {
		t.Errorf("Enterprise baseFee = %v, want 2500 (platformFeeUsd mapped to baseFee)", enterpriseBaseFee)
	}
}

// TestCatalogMode_DateRebase asserts subscription dates are re-based from the
// catalog's relative-date fields: an ACTIVE sub with startedDaysAgo carries a
// backdated startDate, and a TRIALING sub carries startTrial + trialEndsAt.
func TestCatalogMode_DateRebase(t *testing.T) {
	cat, err := ParseCatalog([]byte(richCatalog))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	seeder, calls := newCatalogSeeder(t, cat)
	if _, err := seeder.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}

	today := time.Now().UTC().Format("2006-01-02")
	var sawBackdatedStart, sawTrial bool
	for _, call := range calls.snapshot() {
		if call.method != "POST" || call.path != "/api/v1/subscriptions" {
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(call.body, &body); err != nil {
			continue
		}
		// The Acme "Active" sub has startedDaysAgo:90 → startDate must be in the past.
		if sd, _ := body["startDate"].(string); sd != "" && sd < today {
			sawBackdatedStart = true
		}
		// The Vertex "Trialing" sub must carry startTrial + a trialEndsAt.
		if st, _ := body["startTrial"].(bool); st {
			if _, ok := body["trialEndsAt"]; ok {
				sawTrial = true
			}
		}
	}
	if !sawBackdatedStart {
		t.Error("no subscription POST carried a backdated startDate (date-rebase not applied)")
	}
	if !sawTrial {
		t.Error("TRIALING subscription POST did not carry startTrial + trialEndsAt")
	}
}

// TestCatalogMode_RealGoldenCatalog opportunistically drives the ACTUAL shipped
// golden catalog through the full create chain against the fake backend, when
// run inside the full workspace. Proves the real catalog seeds end-to-end
// (modulo live-backend behavior). Skips cleanly in loadgen-only CI.
func TestCatalogMode_RealGoldenCatalog(t *testing.T) {
	path := filepath.Join("..", "..", "..", "aforo-nextgen-docker", "demo", "demo-seed-catalog.json")
	cat, err := LoadCatalog(path)
	if err != nil {
		t.Skipf("real golden catalog not loadable (%s): %v", path, err)
	}
	seeder, calls := newCatalogSeeder(t, cat)
	res, err := seeder.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("real golden catalog seed errors: %v", res.Errors)
	}

	requirePOST(t, calls, "/api/v1/products")
	requirePOST(t, calls, "/api/v1/rateplans")
	requirePOST(t, calls, "/api/v1/offerings")
	requirePOST(t, calls, "/api/v1/customers")
	requirePOST(t, calls, "/api/v1/subscriptions")

	tn := res.Manifest.Tenants[0]
	if len(tn.Products) != len(cat.Products) {
		t.Errorf("manifest products = %d, catalog products = %d", len(tn.Products), len(cat.Products))
	}
	if len(tn.RatePlans) != len(cat.RatePlans) {
		t.Errorf("manifest ratePlans = %d, catalog ratePlans = %d", len(tn.RatePlans), len(cat.RatePlans))
	}
	if len(tn.Offerings) != len(cat.Offerings) {
		t.Errorf("manifest offerings = %d, catalog offerings = %d", len(tn.Offerings), len(cat.Offerings))
	}
	if len(tn.Customers) != len(cat.Customers) {
		t.Errorf("manifest customers = %d, catalog customers = %d", len(tn.Customers), len(cat.Customers))
	}
	// Every visitor-facing name in the manifest must be human (Req #1).
	for _, p := range tn.Products {
		if syntheticName.MatchString(p.Name) {
			t.Errorf("synthetic product name in real golden manifest: %q", p.Name)
		}
	}
	t.Logf("real golden catalog seeded through full chain: %d products, %d rate plans, %d offerings, %d customers",
		len(tn.Products), len(tn.RatePlans), len(tn.Offerings), len(tn.Customers))
}
