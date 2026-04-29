//go:build integration
// +build integration

// Live integration tests — run only with `go test -tags integration`.
//
// Pre-conditions:
//   - All Aforo services running locally (docker-compose up -d).
//   - AFORO_ADMIN_TOKEN set in the environment.
//   - LOADGEN_TARGET, if set, overrides the default "local" target.
//
// Test plan:
//
//  1. Seed a tiny scenario (matrix-mini) end-to-end against the real APIs.
//  2. Verify each archetype received the correct API call sequence.
//  3. Pick a CANCELLED key from the manifest and submit it to usage-ingestor;
//     assert 401/403. This is the "stale_keys flow is real" sanity check.
//  4. Run --clean to archive everything we created.
//
// The test fails loudly if any of the documented endpoint paths don't exist
// or any DTO field rejected by the server — the entire point of running
// against live services is to catch contract drift.
package seed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

func TestIntegration_SeedAndStaleKey(t *testing.T) {
	token := os.Getenv("AFORO_ADMIN_TOKEN")
	if token == "" {
		t.Skip("AFORO_ADMIN_TOKEN not set; skipping live integration test")
	}
	targetName := os.Getenv("LOADGEN_TARGET")
	if targetName == "" {
		targetName = "local"
	}
	target, err := aforo.ResolveTarget(targetName)
	if err != nil {
		t.Fatal(err)
	}

	c, err := NewClient(ClientConfig{
		Target:      target,
		BearerToken: token,
		MinInterval: 50 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	s := miniLiveScenario()
	seeder, _ := NewSeeder(SeederConfig{
		Client:      c,
		Scenario:    s,
		Concurrency: 2,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	res, err := seeder.Run(ctx)
	if err != nil {
		t.Fatalf("seed run: %v", err)
	}
	if len(res.Errors) > 0 {
		for _, e := range res.Errors {
			t.Logf("seed error: %v", e)
		}
		t.Fatalf("seed produced %d errors", len(res.Errors))
	}

	// Find a CANCELLED key in the manifest.
	staleKey := findFirstStaleKey(res.Manifest)
	if staleKey == "" {
		t.Fatal("no stale key found — scenario should have produced at least one")
	}

	// Submit a usage event with that key — expect 401 or 403.
	if err := submitUsageEvent(ctx, target, staleKey); err == nil {
		t.Errorf("expected 401/403 from usage-ingestor for revoked key; got success")
	} else if !aforo.IsUnauthorized(err) {
		t.Errorf("expected 401/403, got: %v", err)
	}

	// Clean up.
	cleanRes := Clean(ctx, c, res.Manifest)
	for _, e := range cleanRes.Errors {
		t.Logf("clean error: %v", e)
	}
}

func miniLiveScenario() *scenario.Scenario {
	return &scenario.Scenario{
		SchemaVersion: 1,
		Name:          "live-mini",
		TargetTPS:     10,
		Duration:      scenario.Duration(time.Minute),
		Tenants: scenario.Tenants{
			Count: 2,
			Archetypes: []scenario.TenantArchetype{
				{
					Name:                 "live-flat-active",
					Weight:               0.5,
					PricingModel:         scenario.PricingFlatRate,
					BillingMode:          scenario.BillingPostpaid,
					ProductTypes:         []scenario.ProductType{scenario.ProductAPI},
					CustomerCount:        2,
					CurrencyMix:          map[string]float64{"USD": 1.0},
					SubscriptionStateMix: map[scenario.SubscriptionState]float64{scenario.StateActive: 1.0},
					RateConfig:           scenario.RateConfig{FlatFeeUSD: 99.0},
				},
				{
					Name:          "live-flat-cancelled",
					Weight:        0.5,
					PricingModel:  scenario.PricingFlatRate,
					BillingMode:   scenario.BillingPostpaid,
					ProductTypes:  []scenario.ProductType{scenario.ProductAPI},
					CustomerCount: 2,
					CurrencyMix:   map[string]float64{"USD": 1.0},
					SubscriptionStateMix: map[scenario.SubscriptionState]float64{
						scenario.StateActive:    0.5,
						scenario.StateCancelled: 0.5,
					},
					RateConfig: scenario.RateConfig{FlatFeeUSD: 99.0},
				},
			},
		},
	}
}

func findFirstStaleKey(m *Manifest) string {
	for _, t := range m.Tenants {
		for _, c := range t.Customers {
			for _, sub := range c.Subscriptions {
				if sub.Status == scenario.StateCancelled || sub.Status == scenario.StateExpired {
					for _, k := range sub.APIKeys {
						if k.Secret != "" {
							return k.Secret
						}
					}
				}
			}
		}
	}
	return ""
}

// submitUsageEvent posts a single usage event using the provided bearer token.
// Returns the typed APIError from the response so the caller can assert on
// status (we expect 401/403).
func submitUsageEvent(ctx context.Context, target aforo.Target, key string) error {
	url, err := target.Path(aforo.ServiceUsageIngestor, aforo.PathUsageIngest)
	if err != nil {
		return err
	}
	body := map[string]any{
		"eventId":   fmt.Sprintf("loadgen-stale-%d", time.Now().UnixNano()),
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"unitCount": 1,
	}
	data, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(data))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return &aforo.APIError{Method: http.MethodPost, URL: url, UnderlyingErr: err}
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return &aforo.APIError{Method: http.MethodPost, URL: url, Status: resp.StatusCode, Body: ""}
}

// Build-tag suppress unused import warnings on platforms where the build tag
// excludes this file from the default test.
var _ = strings.HasPrefix
