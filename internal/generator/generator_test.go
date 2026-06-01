package generator

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// TestGeneratorDeterminism — same seed + same scenario + same manifest
// produce the same first-N event sequence. This is the load-bearing
// reproducibility property the run engine promises.
func TestGeneratorDeterminism(t *testing.T) {
	scn := miniScenario()
	mfst := miniManifest()

	gA, err := NewGenerator(Config{Scenario: scn, Manifest: mfst, Now: fixedNow})
	if err != nil {
		t.Fatalf("gen A: %v", err)
	}
	gB, err := NewGenerator(Config{Scenario: scn, Manifest: mfst, Now: fixedNow})
	if err != nil {
		t.Fatalf("gen B: %v", err)
	}

	const N = 50
	a := collectN(t, gA, N)
	b := collectN(t, gB, N)
	if len(a) != N || len(b) != N {
		t.Fatalf("collected %d / %d events, want %d", len(a), len(b), N)
	}
	for i := range a {
		if a[i].EventID != b[i].EventID {
			t.Errorf("event %d: A.id=%s B.id=%s — non-deterministic", i, a[i].EventID, b[i].EventID)
		}
		if a[i].TenantID != b[i].TenantID {
			t.Errorf("event %d: tenant differs A=%s B=%s", i, a[i].TenantID, b[i].TenantID)
		}
		if a[i].Envelope.ProductType != b[i].Envelope.ProductType {
			t.Errorf("event %d: product differs A=%s B=%s", i, a[i].Envelope.ProductType, b[i].Envelope.ProductType)
		}
	}
}

// TestNegativePathInjectionRates — set each share to 0.05, sample 4000
// events, observed share for each kind must be within ±2pp.
func TestNegativePathInjectionRates(t *testing.T) {
	scn := miniScenario()
	scn.NegativePaths = scenario.NegativePaths{
		LateEventsPct:   0.05,
		FutureEventsPct: 0.05,
		MalformedPct:    0.05,
		WrongAuthPct:    0.05,
		StaleKeysPct:    0.05,
		OversizePct:     0.05,
	}
	scn.TargetTPS = 4000

	mfst := miniManifest()
	g, err := NewGenerator(Config{Scenario: scn, Manifest: mfst, Now: fixedNow})
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	const N = 4000
	events := collectNTimeout(t, g, N, 30*time.Second)
	if len(events) < N {
		t.Fatalf("collected %d events, want %d", len(events), N)
	}

	counts := map[NegativePathKind]int{}
	for _, e := range events {
		counts[e.NegativePath]++
	}

	for _, kind := range AllNegativePaths {
		got := float64(counts[kind]) / float64(len(events))
		want := 0.05
		if got < want-0.02 || got > want+0.02 {
			t.Errorf("negative_path %s: got %.4f, want %.4f ±0.02", kind, got, want)
		}
	}
}

// TestStaleKeyInjectorOnlyPicksFromStaleSubs — every event tagged
// stale_key MUST carry a subscription_id from the manifest's stale subs;
// none from active subs.
func TestStaleKeyInjectorOnlyPicksFromStaleSubs(t *testing.T) {
	scn := miniScenario()
	scn.NegativePaths = scenario.NegativePaths{StaleKeysPct: 1.0}
	scn.TargetTPS = 1000

	mfst := miniManifest()
	staleIDs := map[string]bool{}
	activeIDs := map[string]bool{}
	for _, t := range mfst.Tenants {
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				if s.Stale {
					staleIDs[s.SubscriptionID] = true
				} else {
					activeIDs[s.SubscriptionID] = true
				}
			}
		}
	}
	if len(staleIDs) == 0 {
		t.Fatal("test fixture broken: miniManifest must contain stale subs")
	}

	g, err := NewGenerator(Config{Scenario: scn, Manifest: mfst, Now: fixedNow})
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	events := collectN(t, g, 500)
	stCount := 0
	for _, e := range events {
		if e.NegativePath != NPStaleKey {
			continue
		}
		stCount++
		if !staleIDs[e.StaleSubscriptionID] {
			t.Errorf("stale_key event tagged subscription_id %q which is NOT in manifest stale subs", e.StaleSubscriptionID)
		}
		if activeIDs[e.StaleSubscriptionID] {
			t.Errorf("stale_key event tagged subscription_id %q which IS in active subs", e.StaleSubscriptionID)
		}
		if e.StaleReason == "" {
			t.Errorf("stale_key event missing stale_reason tag")
		}
	}
	if stCount == 0 {
		t.Errorf("no stale_key events generated despite 100%% injection")
	}
}

// TestStaleKeyMissingFailsAtStartup — scenario with stale_keys_pct > 0 but
// manifest has zero stale subs MUST fail in NewGenerator, not silently.
func TestStaleKeyMissingFailsAtStartup(t *testing.T) {
	scn := miniScenario()
	scn.NegativePaths = scenario.NegativePaths{StaleKeysPct: 0.1}
	mfst := miniManifestNoStale()

	_, err := NewGenerator(Config{Scenario: scn, Manifest: mfst, Now: fixedNow})
	if err == nil {
		t.Fatal("expected error for stale_keys_pct > 0 with no stale subs in manifest")
	}
}

// TestStaleKeyAccessorsAndStats — quick coverage for HasStaleCapacity,
// StaleSubsCount, TenantsActive, and the negative-path snapshot.
func TestStaleKeyAccessorsAndStats(t *testing.T) {
	scn := miniScenario()
	scn.NegativePaths = scenario.NegativePaths{StaleKeysPct: 0.5}
	g, err := NewGenerator(Config{Scenario: scn, Manifest: miniManifest(), Now: fixedNow})
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	if !g.HasStaleCapacity() {
		t.Errorf("HasStaleCapacity should be true")
	}
	if g.TenantsActive() < 1 {
		t.Errorf("TenantsActive = %d, want >= 1", g.TenantsActive())
	}
	if got := g.negPlanner.StaleSubsCount(); got < 1 {
		t.Errorf("StaleSubsCount = %d, want >= 1", got)
	}

	// Snapshot from an empty Stats is all zeros.
	snap := g.Stats().NegativeSnapshot()
	if len(snap) != len(AllNegativePaths) {
		t.Errorf("snapshot has %d entries, want %d", len(snap), len(AllNegativePaths))
	}
	if g.Stats().Generated.Load() != 0 {
		t.Errorf("Generated = %d, want 0 before run", g.Stats().Generated.Load())
	}
}

// TestWrongAuthNeverOverlapsManifest — sample 1000 wrong_auth events, none
// of the fabricated keys should appear in the manifest.
func TestWrongAuthNeverOverlapsManifest(t *testing.T) {
	scn := miniScenario()
	scn.NegativePaths = scenario.NegativePaths{WrongAuthPct: 1.0}
	scn.TargetTPS = 1000

	mfst := miniManifest()
	realKeys := map[string]bool{}
	for _, t := range mfst.Tenants {
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				for _, k := range s.APIKeys {
					realKeys[k.Secret] = true
				}
			}
		}
	}

	g, err := NewGenerator(Config{Scenario: scn, Manifest: mfst, Now: fixedNow})
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	events := collectN(t, g, 1000)
	wa := 0
	for _, e := range events {
		if e.NegativePath != NPWrongAuth {
			continue
		}
		wa++
		if !e.Auth.IsFabricated {
			t.Errorf("wrong_auth event has IsFabricated=false")
		}
		if realKeys[e.Auth.Token] {
			t.Errorf("wrong_auth event token %q matches a real seeded key — fabricated should never overlap", e.Auth.Token)
		}
	}
	if wa < 900 {
		t.Errorf("expected ~1000 wrong_auth events, got %d", wa)
	}
}

// TestOversizeInjectorExceedsLimit — every oversize event encodes to >10 MiB
// when JSON-marshaled.
func TestOversizeInjectorExceedsLimit(t *testing.T) {
	scn := miniScenario()
	scn.NegativePaths = scenario.NegativePaths{OversizePct: 1.0}
	scn.TargetTPS = 50

	mfst := miniManifest()
	g, err := NewGenerator(Config{Scenario: scn, Manifest: mfst, Now: fixedNow})
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	events := collectN(t, g, 5)
	for i, e := range events {
		if e.NegativePath != NPOversize {
			continue
		}
		size := 0
		if v, ok := e.Envelope.Metadata["_oversize_pad"].(string); ok {
			size = len(v)
		}
		if size <= 10*1024*1024 {
			t.Errorf("event %d: oversize pad %d bytes, want >10 MiB", i, size)
		}
	}
}

// TestFutureLatePathTimestamps — timestamps must shift correctly.
func TestFutureLatePathTimestamps(t *testing.T) {
	now := time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC)
	scn := miniScenario()
	scn.NegativePaths = scenario.NegativePaths{LateEventsPct: 0.5, FutureEventsPct: 0.5}
	scn.TargetTPS = 1000

	mfst := miniManifest()
	g, err := NewGenerator(Config{
		Scenario: scn,
		Manifest: mfst,
		Now:      func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	events := collectN(t, g, 100)

	for _, e := range events {
		switch e.NegativePath {
		case NPLate:
			diff := now.Sub(e.Envelope.OccurredAt)
			if diff < 90*time.Minute || diff > 150*time.Minute {
				t.Errorf("late_event ts diff %s, want ~120m", diff)
			}
		case NPFuture:
			diff := e.Envelope.OccurredAt.Sub(now)
			// Pacer may have advanced now slightly; tolerate ±60s.
			if diff < 9*time.Minute || diff > 11*time.Minute {
				t.Errorf("future_event ts diff %s, want ~10m", diff)
			}
		}
	}
}

// --- helpers ---

// collectN drains the generator's events channel with a context-bounded
// pacer until N events are received or 10s elapses.
func collectN(t *testing.T, g *Generator, n int) []*Event {
	return collectNTimeout(t, g, n, 10*time.Second)
}

// collectNTimeout is collectN with a custom deadline.
func collectNTimeout(t *testing.T, g *Generator, n int, timeout time.Duration) []*Event {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	pacer := newCountingPacer(n)
	go func() {
		err := g.Run(ctx, pacer)
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("generator returned: %v", err)
		}
	}()

	out := make([]*Event, 0, n)
	for e := range g.Out() {
		out = append(out, e)
		if len(out) >= n {
			pacer.cancel()
			break
		}
	}
	return out
}

// countingPacer fires N ticks at zero delay then returns context.Canceled.
type countingPacer struct {
	max    int
	count  int
	cancel func()
	ctx    context.Context
}

func newCountingPacer(max int) *countingPacer {
	ctx, cancel := context.WithCancel(context.Background())
	return &countingPacer{max: max, cancel: cancel, ctx: ctx}
}
func (p *countingPacer) Wait(ctx context.Context) (time.Time, error) {
	select {
	case <-ctx.Done():
		return time.Time{}, ctx.Err()
	case <-p.ctx.Done():
		return time.Time{}, context.Canceled
	default:
	}
	if p.count >= p.max {
		return time.Time{}, context.Canceled
	}
	p.count++
	return time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC), nil
}
func (p *countingPacer) Multiplier() float64     { return 1.0 }
func (p *countingPacer) SetMultiplier(_ float64) {}
func (p *countingPacer) Stop()                   { p.cancel() }

func fixedNow() time.Time { return time.Date(2026, 4, 29, 10, 0, 0, 0, time.UTC) }

func miniScenario() *scenario.Scenario {
	return &scenario.Scenario{
		SchemaVersion:    1,
		Name:             "test-mini",
		TargetTPS:        50,
		Duration:         scenario.Duration(time.Minute),
		Seed:             42,
		Tenants:          scenario.Tenants{Count: 2, Distribution: scenario.DistUniform},
		ProductMix:       scenario.ProductMix{API: 1.0},
		IngestionPaths:   scenario.IngestionPaths{RestDirect: 1.0},
		PayloadVariation: scenario.PayloadVariation{SmallPct: 1.0},
	}
}

func miniManifest() *seed.Manifest {
	stale := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	return &seed.Manifest{
		ManifestVersion: seed.ManifestVersion,
		RunID:           "test-run",
		Target:          "local",
		Scenario:        "test-mini",
		Tenants: []seed.ManifestTenant{
			{
				TenantID:     "tenant-1",
				ExternalID:   "loadgen-tenant-test-1",
				Archetype:    "test-archetype",
				PricingModel: scenario.PricingPerUnit,
				BillingMode:  scenario.BillingPostpaid,
				Products: []seed.ManifestProduct{{
					ProductID:   "product-1",
					Name:        "p-1",
					SeedKey:     "loadgen-product-test-1",
					ProductType: scenario.ProductAPI,
					MetricIDs:   []string{"metric-1"},

					Metrics: []seed.ManifestMetric{{ID: "metric-1", Name: "metric_1"}},
				}},
				Customers: []seed.ManifestCustomer{
					{
						CustomerID: "cust-1",
						Email:      "loadgen-cust-1@loadgen.aforo.test",
						SeedKey:    "loadgen-cust-1",
						Currency:   "USD",
						Subscriptions: []seed.ManifestSubscription{
							{
								SubscriptionID: "sub-active-1",
								Status:         scenario.StateActive,
								APIKeys: []seed.ManifestAPIKey{
									{KeyID: "k-1", Secret: "sk_live_real_key_active_1", CredentialType: "BEARER_TOKEN"},
								},
							},
							{
								SubscriptionID: "sub-stale-1",
								Status:         scenario.StateCancelled,
								Stale:          true,
								StaleReason:    "subscription_cancelled",
								StaleSince:     &stale,
								APIKeys: []seed.ManifestAPIKey{
									{KeyID: "k-2", Secret: "sk_live_real_key_revoked_1", CredentialType: "BEARER_TOKEN", Revoked: true, RevokedAt: &stale},
								},
							},
						},
					},
				},
			},
			{
				TenantID:     "tenant-2",
				ExternalID:   "loadgen-tenant-test-2",
				Archetype:    "test-archetype",
				PricingModel: scenario.PricingFlatRate,
				BillingMode:  scenario.BillingPostpaid,
				Products: []seed.ManifestProduct{{
					ProductID:   "product-2",
					Name:        "p-2",
					SeedKey:     "loadgen-product-test-2",
					ProductType: scenario.ProductAPI,
					MetricIDs:   []string{"metric-2"},

					Metrics: []seed.ManifestMetric{{ID: "metric-2", Name: "metric_2"}},
				}},
				Customers: []seed.ManifestCustomer{
					{
						CustomerID: "cust-2",
						Email:      "loadgen-cust-2@loadgen.aforo.test",
						SeedKey:    "loadgen-cust-2",
						Currency:   "USD",
						Subscriptions: []seed.ManifestSubscription{
							{
								SubscriptionID: "sub-active-2",
								Status:         scenario.StateActive,
								APIKeys: []seed.ManifestAPIKey{
									{KeyID: "k-3", Secret: "sk_live_real_key_active_2", CredentialType: "BEARER_TOKEN"},
								},
							},
						},
					},
				},
			},
		},
	}
}

func miniManifestNoStale() *seed.Manifest {
	return &seed.Manifest{
		ManifestVersion: seed.ManifestVersion,
		Tenants: []seed.ManifestTenant{
			{
				TenantID:   "t-1",
				ExternalID: "loadgen-tenant-1",
				Archetype:  "x",
				Products:   []seed.ManifestProduct{{ProductID: "p-1", ProductType: scenario.ProductAPI}},
				Customers: []seed.ManifestCustomer{
					{
						CustomerID: "c-1",
						Subscriptions: []seed.ManifestSubscription{
							{
								SubscriptionID: "s-1",
								Status:         scenario.StateActive,
								APIKeys: []seed.ManifestAPIKey{
									{KeyID: "k-1", Secret: "sk_live_x", CredentialType: "BEARER_TOKEN"},
								},
							},
						},
					},
				},
			},
		},
	}
}
