package seed

import (
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestManifestRoundTrip(t *testing.T) {
	m := NewManifest("seed-2026-04-29-abc123", "local", "matrix-billing", time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC))
	staleSince := time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC)
	m.AppendTenant(ManifestTenant{
		TenantID:     "ten-1",
		ExternalID:   "loadgen-tenant-mtx-flat-postpaid-001",
		Archetype:    "mtx-flat-postpaid",
		PricingModel: scenario.PricingFlatRate,
		BillingMode:  scenario.BillingPostpaid,
		Customers: []ManifestCustomer{
			{
				CustomerID: "cust-1",
				Currency:   "USD",
				Subscriptions: []ManifestSubscription{
					{
						SubscriptionID: "sub-1",
						Status:         scenario.StateCancelled,
						Stale:          true,
						StaleReason:    "subscription_cancelled",
						StaleSince:     &staleSince,
						APIKeys: []ManifestAPIKey{
							{KeyID: "key-1", Secret: "sk_live_x", CredentialType: "BEARER_TOKEN", Revoked: true, RevokedAt: &staleSince},
						},
					},
					{
						SubscriptionID: "sub-2",
						Status:         scenario.StateActive,
						APIKeys: []ManifestAPIKey{
							{KeyID: "key-2", Secret: "sk_live_y", CredentialType: "BEARER_TOKEN"},
						},
					},
				},
			},
		},
	})
	m.Finalize()

	if m.Summary.TotalTenants != 1 {
		t.Errorf("TotalTenants = %d, want 1", m.Summary.TotalTenants)
	}
	if m.Summary.TotalCustomers != 1 {
		t.Errorf("TotalCustomers = %d, want 1", m.Summary.TotalCustomers)
	}
	if m.Summary.TotalSubs != 2 {
		t.Errorf("TotalSubs = %d, want 2", m.Summary.TotalSubs)
	}
	if m.Summary.StaleKeysCount != 1 {
		t.Errorf("StaleKeysCount = %d, want 1", m.Summary.StaleKeysCount)
	}
	if m.Summary.ByArchetype["mtx-flat-postpaid"] != 1 {
		t.Errorf("ByArchetype mismatch: %v", m.Summary.ByArchetype)
	}
	if m.Summary.ByPricingModel["FLAT_RATE"] != 1 {
		t.Errorf("ByPricingModel mismatch: %v", m.Summary.ByPricingModel)
	}
	if m.Summary.ByBillingMode["POSTPAID"] != 1 {
		t.Errorf("ByBillingMode mismatch: %v", m.Summary.ByBillingMode)
	}
	if m.Summary.ByCurrency["USD"] != 1 {
		t.Errorf("ByCurrency mismatch: %v", m.Summary.ByCurrency)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	if _, err := m.Save(path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if loaded.RunID != m.RunID || loaded.Scenario != m.Scenario {
		t.Errorf("round-trip header mismatch")
	}
	if loaded.Summary.StaleKeysCount != 1 {
		t.Errorf("loaded stale count = %d, want 1", loaded.Summary.StaleKeysCount)
	}
	if !loaded.Tenants[0].Customers[0].Subscriptions[0].APIKeys[0].Revoked {
		t.Errorf("CANCELLED sub key should be revoked in round-tripped manifest")
	}
}

func TestManifestRejectsWrongVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "manifest.json")
	bad := map[string]any{
		"manifest_version": 99,
		"run_id":           "x",
	}
	data, _ := json.Marshal(bad)
	if err := writeFile(path, data); err != nil {
		t.Fatal(err)
	}
	_, err := LoadManifest(path)
	if err == nil {
		t.Fatalf("expected version mismatch error")
	}
}

func TestManifestAppendIsConcurrencySafe(t *testing.T) {
	m := NewManifest("r", "local", "s", time.Now())
	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	for i := 0; i < N; i++ {
		i := i
		go func() {
			defer wg.Done()
			m.AppendTenant(ManifestTenant{
				ExternalID: pad("t-", i, 4),
				Archetype:  "x",
			})
		}()
	}
	wg.Wait()
	m.Finalize()
	if len(m.Tenants) != N {
		t.Fatalf("got %d tenants, want %d", len(m.Tenants), N)
	}
	// Sorted by external_id.
	for i := 1; i < len(m.Tenants); i++ {
		if m.Tenants[i-1].ExternalID > m.Tenants[i].ExternalID {
			t.Fatalf("not sorted at i=%d: %q vs %q", i, m.Tenants[i-1].ExternalID, m.Tenants[i].ExternalID)
		}
	}
}

func writeFile(path string, data []byte) error {
	f, err := openTrunc(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(data)
	return err
}
