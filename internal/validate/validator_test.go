package validate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// fakeBackend is the test-only BackendClient. Each method has a
// configurable response so tests can inject specific failure modes.
type fakeBackend struct {
	caps               Capabilities
	eventsByTenant     map[string]int64
	eventsByTenantErr  error
	crossTenantResults map[string]int64
	crossTenantErr     error
	nullCustomerCount  int64
	nullCustomerErr    error
	cacheRatio         float64
	cacheRatioErr      error
	eventsByKey        map[string]int64
	eventsByKeyErr     error
	billRunErr         error
	billRunResult      *BillRunResult
	billRunIDs         []string
	walletBalance      float64
	walletBalanceErr   error
	triggerCalls       int
}

func (f *fakeBackend) Capabilities() Capabilities { return f.caps }
func (f *fakeBackend) EventCountByTenant(_ context.Context, _ TimeWindow, tenants []string) (map[string]int64, error) {
	if f.eventsByTenantErr != nil {
		return nil, f.eventsByTenantErr
	}
	out := map[string]int64{}
	for _, t := range tenants {
		out[t] = f.eventsByTenant[t]
	}
	return out, nil
}
func (f *fakeBackend) CrossTenantQuery(_ context.Context, _ TimeWindow, _ []CrossTenantProbe) (map[string]int64, error) {
	if f.crossTenantErr != nil {
		return nil, f.crossTenantErr
	}
	return f.crossTenantResults, nil
}
func (f *fakeBackend) EventsWithNullCustomer(_ context.Context, _ TimeWindow) (int64, error) {
	return f.nullCustomerCount, f.nullCustomerErr
}
func (f *fakeBackend) CacheHitRatio(_ context.Context, _ TimeWindow) (float64, error) {
	return f.cacheRatio, f.cacheRatioErr
}
func (f *fakeBackend) EventsByAPIKey(_ context.Context, _ TimeWindow, _ []string) (map[string]int64, error) {
	if f.eventsByKeyErr != nil {
		return nil, f.eventsByKeyErr
	}
	return f.eventsByKey, nil
}
func (f *fakeBackend) TriggerBillRun(_ context.Context, _ string, _ string, _ TimeWindow) (string, error) {
	f.triggerCalls++
	if f.billRunErr != nil {
		return "", f.billRunErr
	}
	if len(f.billRunIDs) > 0 {
		id := f.billRunIDs[0]
		f.billRunIDs = f.billRunIDs[1:]
		return id, nil
	}
	return "br-1", nil
}
func (f *fakeBackend) WaitForBillRun(_ context.Context, _ string, _ string, _ time.Duration) (*BillRunResult, error) {
	if f.billRunResult == nil {
		return &BillRunResult{Status: "COMPLETED"}, nil
	}
	return f.billRunResult, nil
}
func (f *fakeBackend) GetWalletBalance(_ context.Context, _, _, _ string) (float64, error) {
	return f.walletBalance, f.walletBalanceErr
}
func (f *fakeBackend) GetSubscriptionState(_ context.Context, _, _ string) (SubscriptionSnapshot, error) {
	return SubscriptionSnapshot{}, ErrUnsupported{Op: "subscription_state"}
}
func (f *fakeBackend) MigrateSubscription(_ context.Context, _, _, _ string) (MigrateOutcome, error) {
	return MigrateOutcome{}, ErrUnsupported{Op: "migrate_subscription"}
}

// minimalScenario builds a valid scenario object for tests without YAML
// round-trip overhead.
func minimalScenario() *scenario.Scenario {
	return &scenario.Scenario{
		SchemaVersion: 1,
		Name:          "test",
		TargetTPS:     50,
		Duration:      scenario.Duration(60 * time.Second),
		Seed:          1,
		Tenants:       scenario.Tenants{Count: 1},
		Assertions: scenario.Assertions{
			EventsLostMax:         0,
			CrossTenantLeakageMax: 0,
		},
	}
}

// minimalManifest builds a 2-tenant manifest with one revoked key.
func minimalManifest() *seed.Manifest {
	return &seed.Manifest{
		ManifestVersion: seed.ManifestVersion,
		RunID:           "run-1",
		Target:          "local",
		Scenario:        "test",
		CreatedAt:       time.Now().UTC(),
		Tenants: []seed.ManifestTenant{
			{
				TenantID:     "t-A",
				ExternalID:   "ext-A",
				Archetype:    "ar-A",
				PricingModel: scenario.PricingPerUnit,
				BillingMode:  scenario.BillingPostpaid,
				Customers: []seed.ManifestCustomer{
					{
						CustomerID: "cust-A1",
						Subscriptions: []seed.ManifestSubscription{
							{
								SubscriptionID: "sub-A1",
								Status:         scenario.StateActive,
								APIKeys:        []seed.ManifestAPIKey{{KeyID: "k-A1"}},
							},
							{
								SubscriptionID: "sub-A2",
								Status:         scenario.StateCancelled,
								Stale:          true,
								StaleReason:    "subscription_cancelled",
								APIKeys:        []seed.ManifestAPIKey{{KeyID: "k-A2", Revoked: true}},
							},
						},
					},
				},
			},
			{
				TenantID:     "t-B",
				ExternalID:   "ext-B",
				Archetype:    "ar-B",
				PricingModel: scenario.PricingFlatRate,
				BillingMode:  scenario.BillingPostpaid,
				Customers:    []seed.ManifestCustomer{{CustomerID: "cust-B1"}},
			},
		},
	}
}

// minimalRunResult builds a typical RunResult with happy-path values.
func minimalRunResult() *runner.RunResult {
	return &runner.RunResult{
		RunID:              "run-1",
		ScenarioName:       "test",
		Target:             "local",
		StartedAt:          time.Now().UTC().Add(-1 * time.Minute),
		StoppedAt:          time.Now().UTC(),
		Duration:           time.Minute,
		EventsGenerated:    100,
		EventsSubmitted:    100,
		EventsSucceeded:    100,
		PerTenant:          map[string]int64{"t-A": 80, "t-B": 20},
		PerArchetype:       map[string]int64{"ar-A": 80, "ar-B": 20},
		NegativePathCounts: map[generator.NegativePathKind]int64{},
	}
}

func TestNew_RequiresAllInputs(t *testing.T) {
	if _, err := New(nil); err == nil {
		t.Fatal("expected error on nil inputs")
	}
	if _, err := New(&Inputs{}); err == nil {
		t.Fatal("expected error on missing run/manifest/scenario")
	}
	in := &Inputs{Run: minimalRunResult(), Manifest: minimalManifest(), Scenario: minimalScenario()}
	v, err := New(in)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if v == nil {
		t.Fatal("nil validator")
	}
	if in.Backend == nil {
		t.Fatal("offline backend not auto-installed")
	}
}

func TestRun_OfflineMode_ChecksThatNeedInfra_Skip(t *testing.T) {
	in := &Inputs{
		Run:      minimalRunResult(),
		Manifest: minimalManifest(),
		Scenario: minimalScenario(),
	}
	v, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := v.Run(context.Background())
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if r.Summary.Failed > 0 {
		for _, c := range r.Checks {
			if c.Status == StatusFail {
				t.Logf("FAIL: %s — %s", c.Name, c.Reason)
			}
		}
		t.Fatalf("offline run shouldn't FAIL; got %d FAILs", r.Summary.Failed)
	}
}

func TestRun_FilterByCheckName(t *testing.T) {
	in := &Inputs{
		Run:        minimalRunResult(),
		Manifest:   minimalManifest(),
		Scenario:   minimalScenario(),
		OnlyChecks: []string{CheckEventCount},
	}
	v, err := New(in)
	if err != nil {
		t.Fatal(err)
	}
	r, err := v.Run(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Checks) != 1 {
		t.Fatalf("expected 1 check, got %d", len(r.Checks))
	}
	if r.Checks[0].Name != CheckEventCount {
		t.Fatalf("expected %s, got %s", CheckEventCount, r.Checks[0].Name)
	}
}

func TestRun_EventCount_Mismatch_Fails(t *testing.T) {
	fb := &fakeBackend{
		caps:           Capabilities{EventQueries: true},
		eventsByTenant: map[string]int64{"t-A": 10, "t-B": 20}, // expected 80/20 → t-A drift 70
	}
	in := &Inputs{
		Run:      minimalRunResult(),
		Manifest: minimalManifest(),
		Scenario: minimalScenario(),
		Backend:  fb,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	var ec *CheckResult
	for _, c := range r.Checks {
		if c.Name == CheckEventCount {
			ec = c
			break
		}
	}
	if ec == nil || ec.Status != StatusFail {
		t.Fatalf("expected event_count to FAIL, got %v", ec)
	}
}

func TestRun_CrossTenantLeakage_NonZeroFails(t *testing.T) {
	fb := &fakeBackend{
		caps: Capabilities{CrossTenantProbe: true, EventQueries: true},
		crossTenantResults: map[string]int64{
			"t-A/t-B": 1, // wrong tenant returned data — leakage
		},
	}
	in := &Inputs{
		Run:      minimalRunResult(),
		Manifest: minimalManifest(),
		Scenario: minimalScenario(),
		Backend:  fb,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	var c *CheckResult
	for _, x := range r.Checks {
		if x.Name == CheckCrossTenant {
			c = x
		}
	}
	if c == nil || c.Status != StatusFail {
		t.Fatalf("expected cross_tenant FAIL on leakage, got %+v", c)
	}
}

func TestRun_Hierarchy_NullCustomerFails(t *testing.T) {
	fb := &fakeBackend{
		caps:              Capabilities{EventQueries: true},
		nullCustomerCount: 1,
	}
	in := &Inputs{
		Run:      minimalRunResult(),
		Manifest: minimalManifest(),
		Scenario: minimalScenario(),
		Backend:  fb,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())
	var c *CheckResult
	for _, x := range r.Checks {
		if x.Name == CheckHierarchy {
			c = x
		}
	}
	if c == nil || c.Status != StatusFail {
		t.Fatalf("expected hierarchy FAIL on NULL customer count > 0, got %+v", c)
	}
}

func TestRun_StaleKey_FalsePositive_Fails(t *testing.T) {
	// One revoked key (k-A2 from minimalManifest) ingested 3 events post-revoke.
	rr := minimalRunResult()
	rr.NegativePathCounts = map[generator.NegativePathKind]int64{
		generator.NPStaleKey: 5,
	}
	rr.ExpectedFailures = 5
	fb := &fakeBackend{
		caps:        Capabilities{EventQueries: true},
		eventsByKey: map[string]int64{"k-A2": 3},
	}
	in := &Inputs{
		Run:      rr,
		Manifest: minimalManifest(),
		Scenario: minimalScenario(),
		Backend:  fb,
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())

	// Negative-paths check must FAIL on the false positive.
	var negCheck *CheckResult
	for _, x := range r.Checks {
		if x.Name == CheckNegativePaths {
			negCheck = x
		}
	}
	if negCheck == nil || negCheck.Status != StatusFail {
		t.Fatalf("expected stale-key false-positive FAIL, got %+v", negCheck)
	}

	// Invariants check must also FAIL on the same signal (Check 7.g).
	var invCheck *CheckResult
	for _, x := range r.Checks {
		if x.Name == CheckInvariants {
			invCheck = x
		}
	}
	if invCheck == nil || invCheck.Status != StatusFail {
		t.Fatalf("expected invariants FAIL on stale-key, got %+v", invCheck)
	}
}

func TestRun_BillRunConcurrency_OneSuccessOneConflict_Pass(t *testing.T) {
	conflictErr := errors.New("HTTP 409 Conflict — bill run lock held")
	in := &Inputs{
		Run:            minimalRunResult(),
		Manifest:       minimalManifest(),
		Scenario:       minimalScenario(),
		Backend:        &concurrencyShim{first: "br-1", conflictErr: conflictErr},
		IncludeBilling: true,
		OnlyChecks:     []string{CheckBillRunConcurrency},
	}
	v, _ := New(in)
	r, _ := v.Run(context.Background())

	var c *CheckResult
	for _, x := range r.Checks {
		if x.Name == CheckBillRunConcurrency {
			c = x
		}
	}
	if c == nil || c.Status != StatusPass {
		t.Fatalf("expected bill-run-concurrency PASS, got %+v", c)
	}
}

// concurrencyShim is a tiny BackendClient that returns one success and one
// 409 conflict, exercising the bill-run concurrency check. The internal
// state (first, claimed) is guarded by a mutex because the orchestrator
// fires TriggerBillRun from two goroutines simultaneously — and that's
// the contract real BackendClient implementations must honor.
type concurrencyShim struct {
	mu          sync.Mutex
	first       string
	claimed     bool
	conflictErr error
}

func (s *concurrencyShim) Capabilities() Capabilities {
	return Capabilities{BillRuns: true}
}
func (s *concurrencyShim) EventCountByTenant(_ context.Context, _ TimeWindow, _ []string) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "event_count"}
}
func (s *concurrencyShim) CrossTenantQuery(_ context.Context, _ TimeWindow, _ []CrossTenantProbe) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "cross_tenant"}
}
func (s *concurrencyShim) EventsWithNullCustomer(_ context.Context, _ TimeWindow) (int64, error) {
	return 0, ErrUnsupported{Op: "null_customer"}
}
func (s *concurrencyShim) CacheHitRatio(_ context.Context, _ TimeWindow) (float64, error) {
	return 0, ErrUnsupported{Op: "cache"}
}
func (s *concurrencyShim) EventsByAPIKey(_ context.Context, _ TimeWindow, _ []string) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "events_by_key"}
}
func (s *concurrencyShim) TriggerBillRun(_ context.Context, _, _ string, _ TimeWindow) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.claimed {
		s.claimed = true
		return s.first, nil
	}
	return "", s.conflictErr
}
func (s *concurrencyShim) WaitForBillRun(_ context.Context, _, _ string, _ time.Duration) (*BillRunResult, error) {
	return &BillRunResult{Status: "COMPLETED"}, nil
}
func (s *concurrencyShim) GetWalletBalance(_ context.Context, _, _, _ string) (float64, error) {
	return 0, ErrUnsupported{Op: "wallet"}
}
func (s *concurrencyShim) GetSubscriptionState(_ context.Context, _, _ string) (SubscriptionSnapshot, error) {
	return SubscriptionSnapshot{}, ErrUnsupported{Op: "subscription_state"}
}
func (s *concurrencyShim) MigrateSubscription(_ context.Context, _, _, _ string) (MigrateOutcome, error) {
	return MigrateOutcome{}, ErrUnsupported{Op: "migrate_subscription"}
}

func TestReport_FinalizeSetsOverall(t *testing.T) {
	r := &ValidationReport{
		Checks: []*CheckResult{
			NewCheckResult(CheckEventCount).Pass(),
			NewCheckResult(CheckCrossTenant).Skip("no infra"),
		},
	}
	r.Finalize()
	if r.Summary.Overall != StatusPass {
		t.Fatalf("expected PASS, got %s", r.Summary.Overall)
	}
	r.Checks = append(r.Checks, NewCheckResult(CheckHierarchy).Fail("synthetic"))
	r.Finalize()
	if r.Summary.Overall != StatusFail {
		t.Fatalf("any FAIL must yield overall FAIL, got %s", r.Summary.Overall)
	}
}

func TestReport_SaveAndLoad_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	r := &ValidationReport{
		ReportVersion: ReportVersion,
		RunID:         "rt-1",
		Scenario:      "rt",
		Target:        "local",
		Checks:        []*CheckResult{NewCheckResult(CheckEventCount).Pass()},
	}
	r.Finalize()
	path, err := r.Save(dir)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadValidationReport(path)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.RunID != r.RunID {
		t.Fatalf("round-trip lost RunID: %s", loaded.RunID)
	}
	if loaded.Summary.Overall != StatusPass {
		t.Fatalf("round-trip overall changed: %s", loaded.Summary.Overall)
	}
}
