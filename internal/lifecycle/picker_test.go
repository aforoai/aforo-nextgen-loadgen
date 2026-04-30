package lifecycle

import (
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

func makeManifest(states []scenario.SubscriptionState) *seed.Manifest {
	subs := make([]seed.ManifestSubscription, len(states))
	for i, s := range states {
		subs[i] = seed.ManifestSubscription{
			SubscriptionID: "sub-" + string(rune('a'+i)),
			Status:         s,
		}
	}
	return &seed.Manifest{
		Tenants: []seed.ManifestTenant{
			{
				TenantID:  "ten-1",
				Archetype: "ar-1",
				Offerings: []seed.ManifestOffering{
					{OfferingID: "off-1"},
					{OfferingID: "off-2"},
				},
				Customers: []seed.ManifestCustomer{
					{CustomerID: "cust-1", Subscriptions: subs},
				},
			},
		},
	}
}

func TestNewPicker_ExcludesTerminalStates(t *testing.T) {
	mf := makeManifest([]scenario.SubscriptionState{
		scenario.StateActive,
		scenario.StateCancelled, // excluded
		scenario.StateExpired,   // excluded
		scenario.StatePaused,
	})
	p := NewPicker(mf, 1)
	subs := p.Subjects()
	if len(subs) != 2 {
		t.Fatalf("want 2 non-terminal subjects, got %d", len(subs))
	}
}

func TestPicker_PickFor_RespectsState(t *testing.T) {
	mf := makeManifest([]scenario.SubscriptionState{
		scenario.StateActive,
		scenario.StateTrialing,
		scenario.StatePaused,
	})
	p := NewPicker(mf, 42)

	// PAUSE picks should never return the already-PAUSED sub.
	for i := 0; i < 50; i++ {
		s, ok := p.PickFor(TransitionPause)
		if !ok {
			t.Fatal("PAUSE should be available")
		}
		if s.State == scenario.StatePaused {
			t.Fatalf("iter %d: PickFor(PAUSE) returned a PAUSED sub: %s", i, s.SubscriptionID)
		}
	}
	// RESUME should only return PAUSED subs.
	for i := 0; i < 50; i++ {
		s, ok := p.PickFor(TransitionResume)
		if !ok {
			t.Fatal("RESUME should be available (we have a PAUSED sub)")
		}
		if s.State != scenario.StatePaused {
			t.Fatalf("iter %d: PickFor(RESUME) returned non-PAUSED: %s", i, s.State)
		}
	}
}

func TestPicker_DeterministicWithSeed(t *testing.T) {
	mf := makeManifest([]scenario.SubscriptionState{
		scenario.StateActive, scenario.StateActive, scenario.StateActive,
		scenario.StateTrialing, scenario.StateTrialing,
	})
	a := NewPicker(mf, 7)
	b := NewPicker(mf, 7)
	for i := 0; i < 20; i++ {
		sa, oka := a.PickFor(TransitionUpgrade)
		sb, okb := b.PickFor(TransitionUpgrade)
		if oka != okb {
			t.Fatalf("iter %d: ok mismatch %v vs %v", i, oka, okb)
		}
		if sa.SubscriptionID != sb.SubscriptionID {
			t.Fatalf("iter %d: pick mismatch %s vs %s", i, sa.SubscriptionID, sb.SubscriptionID)
		}
	}
}

func TestPicker_SetLiveStateAffectsPicking(t *testing.T) {
	mf := makeManifest([]scenario.SubscriptionState{scenario.StateActive})
	p := NewPicker(mf, 1)
	if p.EligibleCount(TransitionPause) != 1 {
		t.Fatal("ACTIVE sub should be eligible for PAUSE")
	}
	p.SetLiveState("sub-a", scenario.StatePaused)
	if p.EligibleCount(TransitionPause) != 0 {
		t.Fatal("after SetLiveState(PAUSED), PAUSE eligibility should be zero")
	}
	if p.EligibleCount(TransitionResume) != 1 {
		t.Fatal("after SetLiveState(PAUSED), RESUME should be eligible")
	}
}

func TestPicker_SuspendedSubExcluded(t *testing.T) {
	mf := makeManifest([]scenario.SubscriptionState{
		scenario.StateActive, scenario.StateActive,
	})
	p := NewPicker(mf, 1)
	p.MarkSuspended("sub-a")
	if p.EligibleCount(TransitionUpgrade) != 1 {
		t.Fatalf("after suspend, eligible count should drop to 1, got %d", p.EligibleCount(TransitionUpgrade))
	}
	for i := 0; i < 20; i++ {
		s, ok := p.PickFor(TransitionUpgrade)
		if !ok {
			t.Fatal("expected sub-b to remain pickable")
		}
		if s.SubscriptionID == "sub-a" {
			t.Fatalf("suspended sub-a was picked at iter %d", i)
		}
	}
	p.MarkLive("sub-a")
	if p.EligibleCount(TransitionUpgrade) != 2 {
		t.Fatalf("after un-suspend, eligible count should be 2, got %d", p.EligibleCount(TransitionUpgrade))
	}
}

func TestPicker_PickMigrateTarget(t *testing.T) {
	mf := makeManifest([]scenario.SubscriptionState{scenario.StateActive})
	p := NewPicker(mf, 1)
	subs := p.Subjects()
	if len(subs) != 1 {
		t.Fatalf("want 1 sub, got %d", len(subs))
	}
	subs[0].CurrentOffer = "off-1"
	target := p.PickMigrateTarget(subs[0])
	if target != "off-2" {
		t.Fatalf("target should be off-2, got %q", target)
	}
}

func TestPicker_PickMigrateTarget_SingleOffering(t *testing.T) {
	mf := &seed.Manifest{
		Tenants: []seed.ManifestTenant{
			{
				TenantID: "ten",
				Offerings: []seed.ManifestOffering{
					{OfferingID: "only"},
				},
				Customers: []seed.ManifestCustomer{
					{Subscriptions: []seed.ManifestSubscription{
						{SubscriptionID: "s1", Status: scenario.StateActive},
					}},
				},
			},
		},
	}
	p := NewPicker(mf, 1)
	subs := p.Subjects()
	subs[0].CurrentOffer = "only"
	if got := p.PickMigrateTarget(subs[0]); got != "" {
		t.Fatalf("single-offering tenant must return empty target, got %q", got)
	}
}

func TestPicker_Probability_DeterministicAcrossCalls(t *testing.T) {
	mf := makeManifest([]scenario.SubscriptionState{scenario.StateActive})
	a := NewPicker(mf, 99)
	b := NewPicker(mf, 99)
	for i := 0; i < 100; i++ {
		if a.Probability(0.3) != b.Probability(0.3) {
			t.Fatalf("Probability output diverged at iter %d", i)
		}
	}
}
