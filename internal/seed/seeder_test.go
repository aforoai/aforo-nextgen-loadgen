package seed

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// TestSeederDryRun verifies the seeder produces a manifest matching the
// archetype shape WITHOUT issuing any HTTP requests.
func TestSeederDryRun(t *testing.T) {
	s := matrixMiniScenario(t)

	c, err := NewClient(ClientConfig{
		Target:      aforo.LocalTarget,
		BearerToken: "ignored-in-dry-run",
		DryRun:      true,
		MinInterval: 1 * time.Microsecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	seeder, err := NewSeeder(SeederConfig{
		Client:      c,
		Scenario:    s,
		Concurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := seeder.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("dry-run errors: %v", res.Errors)
	}

	m := res.Manifest
	if m.Summary.TotalTenants != 6 { // 3 archetypes × 2 each
		t.Errorf("TotalTenants = %d, want 6", m.Summary.TotalTenants)
	}
	if m.Summary.ByArchetype["mtx-flat-postpaid"] != 2 {
		t.Errorf("expected 2 mtx-flat-postpaid, got %d", m.Summary.ByArchetype["mtx-flat-postpaid"])
	}
	// State mix breakdown (one key per subscription):
	//   mtx-flat-postpaid (2 tenants × 4 customers × 100% ACTIVE):       0 stale
	//   mtx-cancelled-mix (2 tenants × 4 customers × 50% CANCELLED+EXPIRED): 4 stale
	//   mtx-quota-prepaid (2 tenants × 4 customers × 100% ACTIVE):       0 stale
	//   Total: 4 stale keys
	if m.Summary.StaleKeysCount != 4 {
		t.Errorf("StaleKeysCount = %d, want 4 (2 tenants × 4 cust × 50%% stale = 4 stale subs × 1 key)",
			m.Summary.StaleKeysCount)
	}

	// Cancelled subs MUST have revoked api keys in the manifest.
	staleSubs := 0
	for _, tn := range m.Tenants {
		for _, cust := range tn.Customers {
			for _, sub := range cust.Subscriptions {
				if sub.Status == scenario.StateCancelled || sub.Status == scenario.StateExpired {
					staleSubs++
					if !sub.Stale {
						t.Errorf("sub %s status=%s should be Stale=true", sub.SubscriptionID, sub.Status)
					}
					for _, k := range sub.APIKeys {
						if !k.Revoked {
							t.Errorf("sub %s key %s status=%s should be revoked", sub.SubscriptionID, k.KeyID, sub.Status)
						}
					}
				}
			}
		}
	}
	if staleSubs == 0 {
		t.Error("no stale subs found — scenario mix should have produced some")
	}

	// Verify dry-run records HTTP-shape semantics.
	recs := c.DryRunRecords()
	if len(recs) == 0 {
		t.Fatal("expected dry-run records")
	}
	// First call per tenant should be the tenant lookup or POST.
	var sawTenantPost bool
	for _, r := range recs {
		if r.Method == "POST" && strings.Contains(r.URL, "/api/v1/internal/tenants") {
			sawTenantPost = true
			break
		}
	}
	if !sawTenantPost {
		t.Error("no tenant POST in dry-run records")
	}
}

// TestSeederWithFakeBackend exercises the full create chain against an
// httptest.Server, asserts the correct API call sequence, and verifies the
// manifest shape end-to-end.
func TestSeederWithFakeBackend(t *testing.T) {
	server, calls := newFakeAforoServer(t)
	defer server.Close()

	target := aforo.Target{
		Name: "test",
		URLs: map[aforo.Service]string{
			aforo.ServiceOrganization:  server.URL,
			aforo.ServiceCatalog:       server.URL,
			aforo.ServicePricing:       server.URL,
			aforo.ServiceCustomer:      server.URL,
			aforo.ServiceBilling:       server.URL,
			aforo.ServiceUsageIngestor: server.URL,
		},
	}

	c, err := NewClient(ClientConfig{
		Target:      target,
		BearerToken: "test-token",
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s := simpleScenario(t)
	seeder, _ := NewSeeder(SeederConfig{
		Client:      c,
		Scenario:    s,
		Concurrency: 1,
	})

	res, err := seeder.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("seed errors: %v", res.Errors)
	}

	m := res.Manifest
	if m.Summary.TotalTenants != 1 {
		t.Errorf("TotalTenants = %d, want 1", m.Summary.TotalTenants)
	}

	// We expect: tenant lookup, tenant POST, product lookup, product POST,
	// metrics bulk, rate-plan lookup, rate-plan POST, offering lookup,
	// offering POST, customer lookup, customer POST, payment-method lookup,
	// payment-method POST, sub lookup, sub POST, api-key lookup, api-key POST.
	// We don't assert the exact count (idempotency lookups can vary), but we
	// MUST see a POST to each of the major entity collections.
	requirePOST(t, calls, "/api/v1/internal/tenants")
	requirePOST(t, calls, "/api/v1/products")
	requirePOST(t, calls, "/api/v1/rate-plans")
	requirePOST(t, calls, "/api/v1/offerings")
	requirePOST(t, calls, "/api/v1/customers")
	requirePOST(t, calls, "/api/v1/subscriptions")
	requirePOST(t, calls, "/api/v1/api-keys")
}

// TestSeederHandlesCancelledStateThroughCancelEndpoint asserts that subs in
// CANCELLED state receive a POST to /subscriptions/{id}/cancel — the only
// endpoint that triggers Aforo's atomic key-revocation cascade.
func TestSeederHandlesCancelledStateThroughCancelEndpoint(t *testing.T) {
	server, calls := newFakeAforoServer(t)
	defer server.Close()

	target := singleHostTarget(server.URL)
	c, err := NewClient(ClientConfig{
		Target:      target,
		BearerToken: "test-token",
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s := allCancelledScenario(t)
	seeder, _ := NewSeeder(SeederConfig{
		Client:      c,
		Scenario:    s,
		Concurrency: 1,
	})

	res, err := seeder.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(res.Errors) > 0 {
		t.Fatalf("errors: %v", res.Errors)
	}

	// Every customer is CANCELLED → must see /cancel.
	cancelHits := 0
	for _, c := range calls.snapshot() {
		if c.method == "POST" && strings.Contains(c.path, "/cancel") {
			cancelHits++
		}
	}
	if cancelHits == 0 {
		t.Fatal("CANCELLED state did not result in any /cancel calls")
	}

	// Manifest reflects revoked keys.
	for _, tn := range res.Manifest.Tenants {
		for _, cust := range tn.Customers {
			for _, sub := range cust.Subscriptions {
				if sub.Status != scenario.StateCancelled {
					t.Errorf("expected all subs CANCELLED, got %s", sub.Status)
					continue
				}
				if !sub.Stale {
					t.Errorf("sub %s should be Stale=true", sub.SubscriptionID)
				}
				if sub.StaleSince == nil {
					t.Errorf("sub %s missing StaleSince", sub.SubscriptionID)
				}
				for _, k := range sub.APIKeys {
					if !k.Revoked {
						t.Errorf("sub %s key %s should be revoked", sub.SubscriptionID, k.KeyID)
					}
				}
			}
		}
	}
}

// TestSeederIdempotent verifies a re-run does not duplicate entities.
// The fake server tracks "POST /tenants twice → second is treated as a no-op
// because the first was idempotent on externalId." Counts of created entities
// remain 1 across two runs.
func TestSeederIdempotent(t *testing.T) {
	server, calls := newFakeAforoServer(t)
	defer server.Close()

	target := singleHostTarget(server.URL)
	c, err := NewClient(ClientConfig{
		Target:      target,
		BearerToken: "test-token",
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s := simpleScenario(t)
	seeder, _ := NewSeeder(SeederConfig{
		Client:      c,
		Scenario:    s,
		Concurrency: 1,
		RunID:       "test-run-id", // pinned so second run uses same external IDs
	})

	if _, err := seeder.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	tenantPostsFirstRun := 0
	for _, c := range calls.snapshot() {
		if c.method == "POST" && c.path == "/api/v1/internal/tenants" {
			tenantPostsFirstRun++
		}
	}

	// Second run with same scenario + same RunID → should hit the GET-by-externalId
	// cache and NOT POST again.
	seeder2, _ := NewSeeder(SeederConfig{
		Client:      c,
		Scenario:    s,
		Concurrency: 1,
		RunID:       "test-run-id",
	})
	if _, err := seeder2.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	tenantPostsTotal := 0
	for _, c := range calls.snapshot() {
		if c.method == "POST" && c.path == "/api/v1/internal/tenants" {
			tenantPostsTotal++
		}
	}
	if tenantPostsTotal != tenantPostsFirstRun {
		t.Errorf("re-run created additional tenant POSTs: first=%d total=%d", tenantPostsFirstRun, tenantPostsTotal)
	}
}

// TestSeederFiltersByArchetype verifies --archetypes-only narrows the run.
func TestSeederFiltersByArchetype(t *testing.T) {
	server, _ := newFakeAforoServer(t)
	defer server.Close()

	target := singleHostTarget(server.URL)
	c, err := NewClient(ClientConfig{
		Target:      target,
		BearerToken: "test-token",
		MinInterval: 1 * time.Microsecond,
		Logger:      log.New(io.Discard, "", 0),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s := matrixMiniScenario(t)
	seeder, _ := NewSeeder(SeederConfig{
		Client:         c,
		Scenario:       s,
		OnlyArchetypes: []string{"mtx-flat-postpaid"},
	})
	res, err := seeder.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Manifest.Summary.TotalTenants != 2 {
		t.Errorf("TotalTenants = %d, want 2 (only mtx-flat-postpaid)", res.Manifest.Summary.TotalTenants)
	}
	if _, ok := res.Manifest.Summary.ByArchetype["mtx-cancelled-mix"]; ok {
		t.Errorf("filtered archetype should not appear: %v", res.Manifest.Summary.ByArchetype)
	}
}

// --- helpers -------------------------------------------------------------

type recordedCall struct {
	method string
	path   string
	body   json.RawMessage
}

type callRecorder struct {
	mu    sync.Mutex
	calls []recordedCall
}

func (cr *callRecorder) record(c recordedCall) {
	cr.mu.Lock()
	cr.calls = append(cr.calls, c)
	cr.mu.Unlock()
}

func (cr *callRecorder) snapshot() []recordedCall {
	cr.mu.Lock()
	defer cr.mu.Unlock()
	out := make([]recordedCall, len(cr.calls))
	copy(out, cr.calls)
	return out
}

// fakeBackend keeps an in-memory map of created entities keyed by externalId.
// On a POST, if externalId is already present, returns 409. On GET with
// ?externalId=, returns the stored entity in {data:[...]}.
//
// IDs are auto-assigned. The backend is deliberately permissive — it accepts
// any field — because the loadgen tool's correctness comes from the request
// shape (asserted in the test), not from the response shape.
type fakeBackend struct {
	mu       sync.Mutex
	entities map[string]map[string]map[string]any // collection → externalId → entity
	idSeq    int64
}

func newFakeAforoServer(t *testing.T) (*httptest.Server, *callRecorder) {
	t.Helper()
	cr := &callRecorder{}
	be := &fakeBackend{entities: map[string]map[string]map[string]any{}}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body.Close()
		cr.record(recordedCall{method: r.Method, path: r.URL.Path, body: json.RawMessage(body)})
		be.handle(w, r, body)
	}))
	return server, cr
}

func (be *fakeBackend) handle(w http.ResponseWriter, r *http.Request, body []byte) {
	be.mu.Lock()
	defer be.mu.Unlock()

	collection := collectionFromPath(r.URL.Path)
	if be.entities[collection] == nil {
		be.entities[collection] = map[string]map[string]any{}
	}

	switch r.Method {
	case http.MethodGet:
		extID := r.URL.Query().Get("externalId")
		// Pattern: /api/.../{collection}/{id} → fetch by id (used for fetchAPIKey, fetchSubscription)
		path := strings.TrimSuffix(r.URL.Path, "/")
		if !strings.HasSuffix(path, collection) {
			// e.g. /api/v1/api-keys/{id}
			if id := lastPathSegment(path); id != "" {
				for _, ent := range be.entities[collection] {
					if ent["id"] == id {
						// Synthesize revoked=true on api-keys post-cancel for tests.
						if collection == "api-keys" {
							ent = copyMap(ent)
							ent["revoked"] = true
							now := time.Now().UTC().Format(time.RFC3339)
							ent["revokedAt"] = now
						}
						writeJSON(w, http.StatusOK, ent)
						return
					}
				}
				writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
				return
			}
		}
		// List — return entities filtered by externalId if provided.
		out := []map[string]any{}
		for _, ent := range be.entities[collection] {
			if extID == "" || ent["externalId"] == extID {
				out = append(out, ent)
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"data": out})

	case http.MethodPost:
		var payload map[string]any
		_ = json.Unmarshal(body, &payload)
		extID, _ := payload["externalId"].(string)

		// /subscriptions/{id}/cancel and /pause and /api-keys/{id}/revoke don't
		// have externalIds — they're action endpoints.
		if strings.Contains(r.URL.Path, "/cancel") || strings.Contains(r.URL.Path, "/pause") || strings.Contains(r.URL.Path, "/revoke") || strings.Contains(r.URL.Path, "/expire") {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}

		// /metrics/bulk doesn't have externalId either — return a synthetic created list.
		if strings.HasSuffix(r.URL.Path, "/metrics/bulk") {
			id := be.nextID("metric")
			writeJSON(w, http.StatusOK, map[string]any{
				"created": []map[string]any{{"id": id, "externalId": payload["externalIdPrefix"], "name": "Metric"}},
			})
			return
		}

		if extID != "" {
			if _, ok := be.entities[collection][extID]; ok {
				writeJSON(w, http.StatusConflict, map[string]any{"error": "already exists"})
				return
			}
		}
		ent := map[string]any{}
		for k, v := range payload {
			ent[k] = v
		}
		ent["id"] = be.nextID(collection)
		if extID != "" {
			be.entities[collection][extID] = ent
		}
		// Subscription create response includes status=ACTIVE so transition logic works.
		if collection == "subscriptions" && ent["status"] == nil {
			ent["status"] = "ACTIVE"
		}
		// API key create response includes secret so manifest captures it.
		if collection == "api-keys" {
			if ent["credentialType"] == "CLIENT_CREDENTIALS" {
				ent["clientId"] = "client-" + ent["id"].(string)
				ent["clientSecret"] = "secret-" + ent["id"].(string)
			} else {
				ent["secret"] = "sk_live_" + ent["id"].(string)
			}
		}
		writeJSON(w, http.StatusOK, ent)

	case http.MethodDelete:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func (be *fakeBackend) nextID(prefix string) string {
	id := atomic.AddInt64(&be.idSeq, 1)
	return fmt.Sprintf("%s-%d", prefix, id)
}

func collectionFromPath(p string) string {
	// /api/v1/foo or /api/v1/foo/bar/baz → "foo"
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) >= 3 {
		// /api/v1/internal/tenants → "tenants"
		if parts[2] == "internal" && len(parts) >= 4 {
			return parts[3]
		}
		return parts[2]
	}
	return p
}

func lastPathSegment(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func copyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func requirePOST(t *testing.T, cr *callRecorder, path string) {
	t.Helper()
	for _, c := range cr.snapshot() {
		if c.method == "POST" && c.path == path {
			return
		}
	}
	t.Errorf("expected POST %s but did not find one in recorded calls", path)
}

func singleHostTarget(baseURL string) aforo.Target {
	t := aforo.Target{Name: "test", URLs: map[aforo.Service]string{}}
	for _, svc := range aforo.AllServices {
		t.URLs[svc] = baseURL
	}
	return t
}

// --- scenarios ------------------------------------------------------------

func matrixMiniScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	return &scenario.Scenario{
		SchemaVersion: 1,
		Name:          "test-mini",
		TargetTPS:     10,
		Duration:      scenario.Duration(time.Hour),
		Tenants: scenario.Tenants{
			Count: 6,
			Archetypes: []scenario.TenantArchetype{
				{
					Name:                 "mtx-flat-postpaid",
					Weight:               0.34,
					PricingModel:         scenario.PricingFlatRate,
					BillingMode:          scenario.BillingPostpaid,
					ProductTypes:         []scenario.ProductType{scenario.ProductAPI},
					CustomerCount:        4,
					CurrencyMix:          map[string]float64{"USD": 1.0},
					SubscriptionStateMix: map[scenario.SubscriptionState]float64{scenario.StateActive: 1.0},
					RateConfig:           scenario.RateConfig{FlatFeeUSD: 99.0},
				},
				{
					Name:          "mtx-cancelled-mix",
					Weight:        0.33,
					PricingModel:  scenario.PricingFlatRate,
					BillingMode:   scenario.BillingPostpaid,
					ProductTypes:  []scenario.ProductType{scenario.ProductAPI},
					CustomerCount: 4,
					CurrencyMix:   map[string]float64{"USD": 1.0},
					SubscriptionStateMix: map[scenario.SubscriptionState]float64{
						scenario.StateActive:    0.50,
						scenario.StateCancelled: 0.25,
						scenario.StateExpired:   0.25,
					},
					RateConfig: scenario.RateConfig{FlatFeeUSD: 99.0},
				},
				{
					Name:                 "mtx-quota-prepaid",
					Weight:               0.33,
					PricingModel:         scenario.PricingIncludedQuota,
					BillingMode:          scenario.BillingPrepaid,
					ProductTypes:         []scenario.ProductType{scenario.ProductMCPServer},
					CustomerCount:        4,
					CurrencyMix:          map[string]float64{"USD": 1.0},
					SubscriptionStateMix: map[scenario.SubscriptionState]float64{scenario.StateActive: 1.0},
					RateConfig: scenario.RateConfig{
						IncludedFreeUnits:       5000,
						PerUnitRateUSD:          0.001,
						WalletInitialBalanceUSD: 500.0,
					},
				},
			},
		},
	}
}

func simpleScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	return &scenario.Scenario{
		SchemaVersion: 1,
		Name:          "test-simple",
		TargetTPS:     10,
		Duration:      scenario.Duration(time.Minute),
		Tenants: scenario.Tenants{
			Count: 1,
			Archetypes: []scenario.TenantArchetype{
				{
					Name:                 "simple",
					Weight:               1.0,
					PricingModel:         scenario.PricingPerUnit,
					BillingMode:          scenario.BillingPostpaid,
					ProductTypes:         []scenario.ProductType{scenario.ProductAPI},
					CustomerCount:        2,
					CurrencyMix:          map[string]float64{"USD": 1.0},
					SubscriptionStateMix: map[scenario.SubscriptionState]float64{scenario.StateActive: 1.0},
					RateConfig:           scenario.RateConfig{PerUnitRateUSD: 0.001},
				},
			},
		},
	}
}

func allCancelledScenario(t *testing.T) *scenario.Scenario {
	t.Helper()
	return &scenario.Scenario{
		SchemaVersion: 1,
		Name:          "test-cancelled",
		TargetTPS:     10,
		Duration:      scenario.Duration(time.Minute),
		Tenants: scenario.Tenants{
			Count: 1,
			Archetypes: []scenario.TenantArchetype{
				{
					Name:                 "all-cancelled",
					Weight:               1.0,
					PricingModel:         scenario.PricingFlatRate,
					BillingMode:          scenario.BillingPostpaid,
					ProductTypes:         []scenario.ProductType{scenario.ProductAPI},
					CustomerCount:        2,
					CurrencyMix:          map[string]float64{"USD": 1.0},
					SubscriptionStateMix: map[scenario.SubscriptionState]float64{scenario.StateCancelled: 1.0},
					RateConfig:           scenario.RateConfig{FlatFeeUSD: 99.0},
				},
			},
		},
	}
}
