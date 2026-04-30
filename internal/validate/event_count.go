package validate

import (
	"context"
	"errors"
	"sort"
)

// runEventCount is Check 1 — events_sent (run.json) MUST equal events stored
// in ClickHouse usage_records (or PG fallback) for every tenant.
//
// Negative-path traffic that should NOT have been stored (future_event,
// malformed, wrong_auth, stale_key, oversize) is excluded from the
// "expected stored" total: events_succeeded already excludes these because
// the Driver only counts 2xx as succeeded.
//
// Tolerance: scenario.assertions.events_lost_max (default 0). If the live
// backend reports fewer events than were sent, the difference must be ≤
// the tolerance, otherwise FAIL.
func (v *Validator) runEventCount(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckEventCount)

	if !v.in.Backend.Capabilities().EventQueries {
		return res.Skip("backend cannot answer event-count queries")
	}

	expected := v.expectedStoredByTenant()
	tenantIDs := v.allTenantIDs()
	actual, err := v.in.Backend.EventCountByTenant(ctx, v.runWindow(), tenantIDs)
	if err != nil {
		var unsup ErrUnsupported
		if errors.As(err, &unsup) {
			return res.Skip("backend unsupported: %s", unsup.Op)
		}
		return res.Fail("backend EventCountByTenant: %v", err)
	}

	tolerance := int64(v.in.Scenario.Assertions.EventsLostMax)

	// per_tenant detail — include every tenant we asked for, even those
	// at zero, so the report is exhaustive.
	type row struct {
		TenantID string `json:"tenant_id"`
		Expected int64  `json:"expected"`
		Actual   int64  `json:"actual"`
		Drift    int64  `json:"drift"` // expected - actual; positive = events lost
	}
	rows := make([]row, 0, len(tenantIDs))
	var totalExpected, totalActual int64
	mismatches := 0
	for _, id := range tenantIDs {
		exp, act := expected[id], actual[id]
		drift := exp - act
		rows = append(rows, row{TenantID: id, Expected: exp, Actual: act, Drift: drift})
		totalExpected += exp
		totalActual += act
		if drift > tolerance || drift < -tolerance {
			mismatches++
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].TenantID < rows[j].TenantID })

	res.
		Set("tenants_checked", len(tenantIDs)).
		Set("total_expected", totalExpected).
		Set("total_actual", totalActual).
		Set("total_drift", totalExpected-totalActual).
		Set("tolerance_per_tenant", tolerance).
		Set("mismatched_tenants", mismatches).
		Set("by_tenant", rows)

	if mismatches > 0 {
		return res.Fail("%d tenant(s) outside tolerance ±%d events; total drift = %d",
			mismatches, tolerance, totalExpected-totalActual)
	}
	return res.Pass()
}

// expectedStoredByTenant returns, per tenant, the count of events that
// SHOULD have landed in ClickHouse — i.e. happy-path successes plus
// late_event (which is accepted with a late flag).
//
// The runner's PerTenant counter measures generated events. We need to
// reconstruct the "successfully ingested" subset per tenant. RunResult
// doesn't carry a per-tenant breakdown of (success, neg-path-by-kind) so
// we approximate as PerTenant × global_success_ratio. This is good enough
// for ci-smoke (one tenant, no neg paths) and for matrix runs where neg
// paths are sparse and uniformly distributed.
//
// When per-tenant per-status counters become available (Session 6+
// telemetry hardening), this function will switch to exact arithmetic.
func (v *Validator) expectedStoredByTenant() map[string]int64 {
	out := make(map[string]int64, len(v.in.Run.PerTenant))
	if len(v.in.Run.PerTenant) == 0 {
		return out
	}
	gen := v.in.Run.EventsGenerated
	if gen <= 0 {
		// Defensive: if the run wrote zero counters, report zeros and let
		// the count check pass when actual is also zero.
		for k := range v.in.Run.PerTenant {
			out[k] = 0
		}
		return out
	}
	// "expected stored" counts every event that wasn't actively rejected.
	// late_event is intentionally accepted; everything else in the negative-
	// path bucket is rejected. So: succeeded + (late_event count, if any).
	stored := v.in.Run.EventsSucceeded
	if late, ok := v.in.Run.NegativePathCounts["late_event"]; ok {
		// late_events that were dispatched as 2xx are already in
		// EventsSucceeded; do not double-count. The bucket records
		// fault-injection planning, not response codes.
		_ = late
	}
	ratio := float64(stored) / float64(gen)
	for tenant, sent := range v.in.Run.PerTenant {
		out[tenant] = int64(float64(sent) * ratio)
	}
	return out
}

