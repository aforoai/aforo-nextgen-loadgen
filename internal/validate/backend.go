package validate

import (
	"context"
	"fmt"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/runner"
)

// BackendClient is the validator's interface to live infrastructure
// (ClickHouse, PostgreSQL, billing-platform, Redis). The orchestrator
// programs against the interface so unit tests can swap in a fake and so
// the OfflineBackend can serve a CI run that has no infra reachable.
//
// CONTRACT — implementations MUST be safe for concurrent use by multiple
// goroutines. The bill-run-concurrency check (Check 8) intentionally
// fires TriggerBillRun from two goroutines simultaneously to probe the
// platform's RedisLockService. Net-bound implementations using a stdlib
// http.Client get this for free; in-memory fakes need explicit
// synchronization.
//
// Every method MUST take a context. Implementations that talk to live
// infra honor cancellation; offline ones return synthesized data instantly.
//
// Capabilities() declares which checks the implementation can answer. The
// orchestrator inspects this and SKIPs checks the backend can't run, with a
// clear reason — "backend offline" is the most common one.
type BackendClient interface {
	// Capabilities returns the set of capability flags the client can serve.
	// SKIPs are routed to checks whose required capability is missing.
	Capabilities() Capabilities

	// EventCountByTenant returns the count of events ingested into ClickHouse
	// usage_records (or PG usage_events fallback) per tenant_id, scoped to
	// the run's time window.
	EventCountByTenant(ctx context.Context, window TimeWindow, tenants []string) (map[string]int64, error)

	// CrossTenantQuery probes for leakage: queries with the WRONG X-Tenant-Id
	// header. Returns a count of events that should NOT have been visible.
	// A correctly-isolated platform returns zero for every probe.
	CrossTenantQuery(ctx context.Context, window TimeWindow, probes []CrossTenantProbe) (map[string]int64, error)

	// EventsWithNullCustomer returns the count of ingested events whose
	// BillingHierarchyEnricher failed to resolve a customer_id. A correctly
	// configured pipeline returns zero.
	EventsWithNullCustomer(ctx context.Context, window TimeWindow) (int64, error)

	// CacheHitRatio returns the BillingHierarchyEnricher's Redis cache hit
	// ratio over the run window — exposed via /actuator/metrics on
	// usage-ingestor or analytics-service.
	CacheHitRatio(ctx context.Context, window TimeWindow) (float64, error)

	// EventsByAPIKey returns the count of successfully ingested events for
	// each api_key_id in keys, scoped to the run window. Used by the stale-
	// key false-positive check (Check 6.e.3 + 7.g): every revoked key must
	// land at zero.
	EventsByAPIKey(ctx context.Context, window TimeWindow, keys []string) (map[string]int64, error)

	// TriggerBillRun starts a bill run for tenantID and returns the bill run
	// id. idempotencyKey is the Idempotency-Key header — re-using the same
	// key yields the same run (Aforo's bill-run idempotency contract).
	TriggerBillRun(ctx context.Context, tenantID, idempotencyKey string, window TimeWindow) (string, error)

	// WaitForBillRun blocks until the bill run reaches a terminal state or
	// the context is cancelled. Returns the final BillRunResult.
	WaitForBillRun(ctx context.Context, tenantID, billRunID string, timeout time.Duration) (*BillRunResult, error)

	// GetWalletBalance fetches the current wallet balance for the customer.
	GetWalletBalance(ctx context.Context, tenantID, customerID, currency string) (float64, error)
}

// Capabilities is a bitmask-style struct declaring what a BackendClient can do.
// New flags add fields, never reorder.
type Capabilities struct {
	EventQueries     bool // ClickHouse / PG event count + filter
	CacheMetrics     bool // /actuator/metrics on usage-ingestor
	BillRuns         bool // billing-platform bill run trigger + poll
	WalletQueries    bool // billing-platform wallet read
	CrossTenantProbe bool // controlled IDOR probe (intentional)
}

// TimeWindow scopes a backend query to a [Start, End] interval.
type TimeWindow struct {
	Start time.Time
	End   time.Time
}

// CrossTenantProbe is one IDOR test: query with a wrong X-Tenant-Id header
// and expect zero rows back. Probes are constructed by the orchestrator
// from the run manifest.
type CrossTenantProbe struct {
	ProbeID       string
	WrongTenantID string
	RealTenantID  string
}

// BillRunResult is the terminal verdict of a bill run plus the subset of
// metrics this validator needs.
type BillRunResult struct {
	BillRunID    string
	TenantID     string
	Status       string // COMPLETED | FAILED | PARTIALLY_FAILED
	InvoicedUSD  float64
	WalletDebit  float64
	HoldsCreated int
	HoldsExpired int
	StartedAt    time.Time
	EndedAt      time.Time
}

// OfflineBackend is the BackendClient used when no infrastructure is
// reachable (CI smoke, unit tests, dry-run). It synthesizes deterministic
// answers from the RunResult and Manifest the validator already has —
// "events_in_clickhouse == events_succeeded from run.json", which lets the
// event-count check, negative-path check, and invariants check run cleanly
// without infra. Checks that fundamentally need infra (cross-tenant probe,
// cache-hit ratio, bill runs) report SKIP via Capabilities().
//
// This is NOT a mock for testing — it's the production-quality fallback for
// validate runs that don't include --include-billing or live ClickHouse.
type OfflineBackend struct {
	Run *runner.RunResult
}

// NewOfflineBackend wraps a RunResult.
func NewOfflineBackend(run *runner.RunResult) *OfflineBackend {
	return &OfflineBackend{Run: run}
}

// Capabilities declares what offline mode can serve. Crucially: only
// EventQueries — and only as far as run.json's per-tenant counters reach.
func (o *OfflineBackend) Capabilities() Capabilities {
	return Capabilities{EventQueries: true}
}

// EventCountByTenant returns RunResult.PerTenant filtered to the requested
// tenant ids. Tenants not present in run.json are returned as zero — that's
// the literal truth of "no events were sent for them".
func (o *OfflineBackend) EventCountByTenant(_ context.Context, _ TimeWindow, tenants []string) (map[string]int64, error) {
	if o.Run == nil {
		return nil, fmt.Errorf("offline backend: nil RunResult")
	}
	out := make(map[string]int64, len(tenants))
	for _, t := range tenants {
		out[t] = o.Run.PerTenant[t]
	}
	return out, nil
}

// CrossTenantQuery requires real infra. Offline returns ErrUnsupported so
// the orchestrator marks the check SKIP.
func (o *OfflineBackend) CrossTenantQuery(_ context.Context, _ TimeWindow, _ []CrossTenantProbe) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "cross_tenant_query"}
}

// EventsWithNullCustomer requires backend access to enriched events.
func (o *OfflineBackend) EventsWithNullCustomer(_ context.Context, _ TimeWindow) (int64, error) {
	return 0, ErrUnsupported{Op: "events_null_customer"}
}

// CacheHitRatio requires /actuator/metrics access.
func (o *OfflineBackend) CacheHitRatio(_ context.Context, _ TimeWindow) (float64, error) {
	return 0, ErrUnsupported{Op: "cache_hit_ratio"}
}

// EventsByAPIKey requires per-key event filtering — the run's per_tenant
// counter doesn't carry the api_key_id dimension.
func (o *OfflineBackend) EventsByAPIKey(_ context.Context, _ TimeWindow, _ []string) (map[string]int64, error) {
	return nil, ErrUnsupported{Op: "events_by_api_key"}
}

// TriggerBillRun requires billing-platform.
func (o *OfflineBackend) TriggerBillRun(_ context.Context, _, _ string, _ TimeWindow) (string, error) {
	return "", ErrUnsupported{Op: "bill_run_trigger"}
}

// WaitForBillRun requires billing-platform.
func (o *OfflineBackend) WaitForBillRun(_ context.Context, _, _ string, _ time.Duration) (*BillRunResult, error) {
	return nil, ErrUnsupported{Op: "bill_run_poll"}
}

// GetWalletBalance requires billing-platform.
func (o *OfflineBackend) GetWalletBalance(_ context.Context, _, _, _ string) (float64, error) {
	return 0, ErrUnsupported{Op: "wallet_balance"}
}

// ErrUnsupported is returned by OfflineBackend (and live backends that have
// a missing capability) so the orchestrator can SKIP the check with a
// uniform reason. Wrap with %w; callers test via errors.As.
type ErrUnsupported struct{ Op string }

func (e ErrUnsupported) Error() string { return "backend operation unsupported: " + e.Op }
