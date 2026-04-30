package validate

import (
	"context"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// TestStaleKey_CleanClickHouseProducesZeroFalsePositives is the
// acceptance-criteria-driven test:
//
//	"given a manifest with K stale subs, validator correctly identifies
//	 ZERO false positives in a clean ClickHouse"
//
// The fakeBackend reports zero events for every revoked key — the platform
// invalidated cache on revoke and rejected every replay. Validator MUST
// PASS.
func TestStaleKey_CleanClickHouseProducesZeroFalsePositives(t *testing.T) {
	rr := minimalRunResult()
	rr.NegativePathCounts = map[generator.NegativePathKind]int64{
		generator.NPStaleKey: 5,
	}
	rr.ExpectedFailures = 5

	mf := manifestWithStaleKeys(3) // 3 revoked keys

	// Backend reports ZERO events on every revoked key — clean state.
	fb := &fakeBackend{
		caps: Capabilities{EventQueries: true},
		eventsByKey: map[string]int64{
			"k-revoked-0": 0,
			"k-revoked-1": 0,
			"k-revoked-2": 0,
		},
	}

	in := &Inputs{
		Run:        rr,
		Manifest:   mf,
		Scenario:   minimalScenario(),
		Backend:    fb,
		OnlyChecks: []string{CheckNegativePaths, CheckInvariants},
	}
	v, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := v.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			t.Fatalf("expected zero failures on clean ClickHouse; %s FAILED: %s",
				c.Name, c.Reason)
		}
	}
}

// TestStaleKey_PlantedFalsePositiveCaught is the dual:
//
//	"AND correctly identifies a planted false positive"
//
// One revoked key has a non-zero count — both Check 6.e.3 AND Check 7.g
// MUST FAIL. This is the "test the test" line in the spec.
func TestStaleKey_PlantedFalsePositiveCaught(t *testing.T) {
	rr := minimalRunResult()
	rr.NegativePathCounts = map[generator.NegativePathKind]int64{
		generator.NPStaleKey: 5,
	}
	rr.ExpectedFailures = 5

	mf := manifestWithStaleKeys(3)

	// PLANT ONE — k-revoked-1 has 7 successful ingestions.
	fb := &fakeBackend{
		caps: Capabilities{EventQueries: true},
		eventsByKey: map[string]int64{
			"k-revoked-0": 0,
			"k-revoked-1": 7, // planted
			"k-revoked-2": 0,
		},
	}

	in := &Inputs{
		Run:        rr,
		Manifest:   mf,
		Scenario:   minimalScenario(),
		Backend:    fb,
		OnlyChecks: []string{CheckNegativePaths, CheckInvariants},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())

	failed := map[string]bool{}
	for _, c := range r.Checks {
		if c.Status == StatusFail {
			failed[c.Name] = true
		}
	}
	if !failed[CheckNegativePaths] {
		t.Fatal("Check 6.e.3 must FAIL on a planted false positive")
	}
	if !failed[CheckInvariants] {
		t.Fatal("Check 7.g must FAIL on the same signal")
	}
}

// manifestWithStaleKeys builds a manifest with k revoked api keys spread
// across one tenant. Keys are named k-revoked-0..k-revoked-(k-1) so tests
// can address them by index.
func manifestWithStaleKeys(k int) *seed.Manifest {
	subs := make([]seed.ManifestSubscription, 0, k+1)
	subs = append(subs, seed.ManifestSubscription{
		SubscriptionID: "sub-active",
		Stale:          false,
		APIKeys:        []seed.ManifestAPIKey{{KeyID: "k-active"}},
	})
	for i := 0; i < k; i++ {
		subs = append(subs, seed.ManifestSubscription{
			SubscriptionID: "sub-cancelled-" + string(rune('A'+i)),
			Stale:          true,
			StaleReason:    "subscription_cancelled",
			APIKeys: []seed.ManifestAPIKey{
				{KeyID: "k-revoked-" + string(rune('0'+i)), Revoked: true},
			},
		})
	}
	return &seed.Manifest{
		ManifestVersion: seed.ManifestVersion,
		RunID:           "stale-test",
		Target:          "local",
		Scenario:        "stale-test",
		Tenants: []seed.ManifestTenant{
			{
				TenantID:  "t-stale",
				Archetype: "ar-stale",
				Customers: []seed.ManifestCustomer{
					{CustomerID: "c-stale", Subscriptions: subs},
				},
			},
		},
	}
}
