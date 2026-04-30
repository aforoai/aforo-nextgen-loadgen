package validate

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// lifecycleBackend stubs every BackendClient method, returning configured
// values per call. Concurrency-safe.
type lifecycleBackend struct {
	caps              Capabilities
	subState          map[string]SubscriptionSnapshot // sub_id → snapshot
	migrateOut        MigrateOutcome
	migrateErr        error
	billRunErr        []error
	mu                sync.Mutex
	billRunCallIdx    int
	migrateCallCount  int
	getSubCallCount   int
	billRunStarted    chan struct{}
}

func (b *lifecycleBackend) Capabilities() Capabilities { return b.caps }
func (b *lifecycleBackend) EventCountByTenant(_ context.Context, _ TimeWindow, _ []string) (map[string]int64, error) {
	return nil, nil
}
func (b *lifecycleBackend) CrossTenantQuery(_ context.Context, _ TimeWindow, _ []CrossTenantProbe) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "cross_tenant"}
}
func (b *lifecycleBackend) EventsWithNullCustomer(_ context.Context, _ TimeWindow) (int64, error) {
	return 0, nil
}
func (b *lifecycleBackend) CacheHitRatio(_ context.Context, _ TimeWindow) (float64, error) {
	return 0, ErrUnsupported{Op: "cache"}
}
func (b *lifecycleBackend) EventsByAPIKey(_ context.Context, _ TimeWindow, _ []string) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "by_key"}
}
func (b *lifecycleBackend) TriggerBillRun(_ context.Context, _, key string, _ TimeWindow) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	idx := b.billRunCallIdx
	b.billRunCallIdx++
	if idx < len(b.billRunErr) && b.billRunErr[idx] != nil {
		return "", b.billRunErr[idx]
	}
	if b.billRunStarted != nil {
		select {
		case b.billRunStarted <- struct{}{}:
		default:
		}
	}
	return "br-" + key, nil
}
func (b *lifecycleBackend) WaitForBillRun(_ context.Context, _, _ string, _ time.Duration) (*BillRunResult, error) {
	return &BillRunResult{Status: "COMPLETED"}, nil
}
func (b *lifecycleBackend) GetWalletBalance(_ context.Context, _, _, _ string) (float64, error) {
	return 0, ErrUnsupported{Op: "wallet"}
}
func (b *lifecycleBackend) GetSubscriptionState(_ context.Context, _, subID string) (SubscriptionSnapshot, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.getSubCallCount++
	if snap, ok := b.subState[subID]; ok {
		return snap, nil
	}
	return SubscriptionSnapshot{}, ErrUnsupported{Op: "subscription_state"}
}
func (b *lifecycleBackend) MigrateSubscription(_ context.Context, _, _, target string) (MigrateOutcome, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.migrateCallCount++
	if b.migrateErr != nil {
		return MigrateOutcome{}, b.migrateErr
	}
	out := b.migrateOut
	if out.OfferingID == "" {
		out.OfferingID = target
	}
	return out, nil
}

func TestLifecycleCorrectness_NoTransitions_Skips(t *testing.T) {
	in := &Inputs{
		Run:      minimalRunResult(),
		Manifest: minimalManifest(),
		Scenario: minimalScenario(),
	}
	v, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := v.Run(context.Background())
	for _, c := range r.Checks {
		if c.Name == CheckLifecycleCorrectness {
			if c.Status != StatusSkip {
				t.Fatalf("want SKIP, got %s — reason=%s", c.Status, c.Reason)
			}
			return
		}
	}
	t.Fatalf("CheckLifecycleCorrectness not in report")
}

func TestLifecycleCorrectness_StableIDViolation_Fails(t *testing.T) {
	transitions := []lifecycle.TransitionRecord{
		{
			SubscriptionID:   "sub-1",
			TenantID:         "t-A",
			Transition:       lifecycle.TransitionMigrate,
			TransitionStatus: lifecycle.StatusFail,
			Error:            "stable-id violation: source=sub-1 target=sub-1-NEW",
		},
	}
	in := &Inputs{
		Run:         minimalRunResult(),
		Manifest:    minimalManifest(),
		Scenario:    minimalScenario(),
		Transitions: transitions,
		OnlyChecks:  []string{CheckLifecycleCorrectness},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckLifecycleCorrectness)
	if c.Status != StatusFail {
		t.Fatalf("want FAIL on stable-id violation, got %s — reason=%s", c.Status, c.Reason)
	}
	if !strings.Contains(c.Reason, "lifecycle correctness FAIL") {
		t.Errorf("reason should mention lifecycle correctness, got %q", c.Reason)
	}
}

func TestLifecycleCorrectness_LiveStateMatch_Pass(t *testing.T) {
	transitions := []lifecycle.TransitionRecord{
		{
			SubscriptionID:    "sub-A1",
			TenantID:          "t-A",
			Transition:        lifecycle.TransitionPause,
			TransitionStatus:  lifecycle.StatusOK,
			ExpectedPostState: "PAUSED",
		},
	}
	be := &lifecycleBackend{
		caps:     Capabilities{Subscriptions: true},
		subState: map[string]SubscriptionSnapshot{"sub-A1": {Status: "PAUSED", LastPhaseRecorded: true}},
	}
	in := &Inputs{
		Run:         minimalRunResult(),
		Manifest:    minimalManifest(),
		Scenario:    minimalScenario(),
		Transitions: transitions,
		Backend:     be,
		OnlyChecks:  []string{CheckLifecycleCorrectness},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckLifecycleCorrectness)
	if c.Status != StatusPass {
		t.Fatalf("want PASS, got %s — %s", c.Status, c.Reason)
	}
}

func TestLifecycleCorrectness_LiveStateMismatch_Fails(t *testing.T) {
	transitions := []lifecycle.TransitionRecord{
		{
			SubscriptionID:    "sub-A1",
			TenantID:          "t-A",
			Transition:        lifecycle.TransitionPause,
			TransitionStatus:  lifecycle.StatusOK,
			ExpectedPostState: "PAUSED",
		},
	}
	be := &lifecycleBackend{
		caps:     Capabilities{Subscriptions: true},
		subState: map[string]SubscriptionSnapshot{"sub-A1": {Status: "ACTIVE"}},
	}
	in := &Inputs{
		Run:         minimalRunResult(),
		Manifest:    minimalManifest(),
		Scenario:    minimalScenario(),
		Transitions: transitions,
		Backend:     be,
		OnlyChecks:  []string{CheckLifecycleCorrectness},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckLifecycleCorrectness)
	if c.Status != StatusFail {
		t.Fatalf("want FAIL on state mismatch, got %s", c.Status)
	}
}

func TestStateMachineInvariants_DetectsTerminalViolation(t *testing.T) {
	transitions := []lifecycle.TransitionRecord{
		{
			// CANCELLED → ACTIVE is a terminal violation.
			SubscriptionID:    "sub-A2",
			TenantID:          "t-A",
			Transition:        lifecycle.TransitionUpgrade,
			TransitionStatus:  lifecycle.StatusOK,
			FromState:         "CANCELLED",
			ExpectedPostState: "ACTIVE",
		},
	}
	in := &Inputs{
		Run:         minimalRunResult(),
		Manifest:    minimalManifest(),
		Scenario:    minimalScenario(),
		Transitions: transitions,
		OnlyChecks:  []string{CheckStateMachineInvariants},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckStateMachineInvariants)
	if c.Status != StatusFail {
		t.Fatalf("want FAIL, got %s", c.Status)
	}
}

func TestStateMachineInvariants_NoViolations_Pass(t *testing.T) {
	transitions := []lifecycle.TransitionRecord{
		{
			SubscriptionID:    "sub-A1",
			TenantID:          "t-A",
			Transition:        lifecycle.TransitionPause,
			TransitionStatus:  lifecycle.StatusOK,
			FromState:         "ACTIVE",
			ExpectedPostState: "PAUSED",
		},
		{
			SubscriptionID:    "sub-A1",
			TenantID:          "t-A",
			Transition:        lifecycle.TransitionResume,
			TransitionStatus:  lifecycle.StatusOK,
			FromState:         "PAUSED",
			ExpectedPostState: "ACTIVE",
		},
	}
	in := &Inputs{
		Run:         minimalRunResult(),
		Manifest:    minimalManifest(),
		Scenario:    minimalScenario(),
		Transitions: transitions,
		OnlyChecks:  []string{CheckStateMachineInvariants},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckStateMachineInvariants)
	if c.Status != StatusPass {
		t.Fatalf("want PASS, got %s — reason=%s", c.Status, c.Reason)
	}
}

func TestLifecycleVsBillRun_NoIncludeBilling_Skips(t *testing.T) {
	in := &Inputs{
		Run:        minimalRunResult(),
		Manifest:   minimalManifest(),
		Scenario:   minimalScenario(),
		OnlyChecks: []string{CheckLifecycleVsBillRun},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckLifecycleVsBillRun)
	if c.Status != StatusSkip {
		t.Fatalf("want SKIP, got %s", c.Status)
	}
}

func TestLifecycleVsBillRun_StableIDPreserved_Pass(t *testing.T) {
	be := &lifecycleBackend{
		caps: Capabilities{BillRuns: true, Subscriptions: true},
		// First call succeeds, second returns 409 Conflict.
		billRunErr: []error{nil, errors.New("HTTP 409 Conflict — bill run lock held")},
		migrateOut: MigrateOutcome{
			SourceSubscriptionID: "sub-A1",
			TargetSubscriptionID: "sub-A1", // stable id
			OfferingID:           "off-2",
		},
	}
	mf := minimalManifest()
	// Add a 2nd offering on the first tenant so migrate has a target.
	mf.Tenants[0].Offerings = []seed.ManifestOffering{
		{OfferingID: "off-1"},
		{OfferingID: "off-2"},
	}
	in := &Inputs{
		Run:            minimalRunResult(),
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        be,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckLifecycleVsBillRun},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckLifecycleVsBillRun)
	if c.Status != StatusPass {
		t.Fatalf("want PASS, got %s — reason=%s", c.Status, c.Reason)
	}
}

func TestLifecycleVsBillRun_StableIDViolation_Fails(t *testing.T) {
	be := &lifecycleBackend{
		caps:       Capabilities{BillRuns: true, Subscriptions: true},
		billRunErr: []error{nil, errors.New("HTTP 409 Conflict")},
		migrateOut: MigrateOutcome{
			SourceSubscriptionID: "sub-A1",
			TargetSubscriptionID: "sub-A1-NEW", // VIOLATION
		},
	}
	mf := minimalManifest()
	mf.Tenants[0].Offerings = []seed.ManifestOffering{
		{OfferingID: "off-1"}, {OfferingID: "off-2"},
	}
	in := &Inputs{
		Run:            minimalRunResult(),
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        be,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckLifecycleVsBillRun},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckLifecycleVsBillRun)
	if c.Status != StatusFail {
		t.Fatalf("want FAIL, got %s — reason=%s", c.Status, c.Reason)
	}
	if !strings.Contains(c.Reason, "stable-id") {
		t.Errorf("FAIL reason should mention stable-id, got %q", c.Reason)
	}
}

func TestLifecycleVsBillRun_DoubleBilling_RedisLockFailure_Fails(t *testing.T) {
	// Both bill runs accepted (no 409) — simulates the Redis lock failing
	// to engage. This is the "double-billing risk" path the prompt requires
	// Check 11 to flag as FAIL.
	be := &lifecycleBackend{
		caps:       Capabilities{BillRuns: true, Subscriptions: true},
		billRunErr: []error{nil, nil}, // both succeed
		migrateOut: MigrateOutcome{
			SourceSubscriptionID: "sub-A1",
			TargetSubscriptionID: "sub-A1",
		},
	}
	mf := minimalManifest()
	mf.Tenants[0].Offerings = []seed.ManifestOffering{
		{OfferingID: "off-1"}, {OfferingID: "off-2"},
	}
	in := &Inputs{
		Run:            minimalRunResult(),
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        be,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckLifecycleVsBillRun},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckLifecycleVsBillRun)
	if c.Status != StatusFail {
		t.Fatalf("double-billing scenario should FAIL, got %s — reason=%s", c.Status, c.Reason)
	}
	if !strings.Contains(c.Reason, "Redis lock failure") {
		t.Errorf("FAIL reason should mention Redis lock failure, got %q", c.Reason)
	}
}

func TestLifecycleVsBillRun_BothBillRunsFail_Fails(t *testing.T) {
	be := &lifecycleBackend{
		caps: Capabilities{BillRuns: true, Subscriptions: true},
		billRunErr: []error{
			errors.New("HTTP 500 Internal"),
			errors.New("HTTP 500 Internal"),
		},
		migrateOut: MigrateOutcome{
			SourceSubscriptionID: "sub-A1",
			TargetSubscriptionID: "sub-A1",
		},
	}
	mf := minimalManifest()
	mf.Tenants[0].Offerings = []seed.ManifestOffering{
		{OfferingID: "off-1"}, {OfferingID: "off-2"},
	}
	in := &Inputs{
		Run:            minimalRunResult(),
		Manifest:       mf,
		Scenario:       minimalScenario(),
		Backend:        be,
		IncludeBilling: true,
		OnlyChecks:     []string{CheckLifecycleVsBillRun},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	c := findCheck(r, CheckLifecycleVsBillRun)
	if c.Status != StatusFail {
		t.Fatalf("zero successes should FAIL, got %s", c.Status)
	}
}

func findCheck(r *ValidationReport, name string) *CheckResult {
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	return nil
}
