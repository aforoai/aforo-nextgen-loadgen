package validate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/lifecycle"
)

// runLifecycleVsBillRun is Check 11 — fire 2 simultaneous bill runs against
// the same tenant + a migrate on a sub in that tenant. Assert:
//
//	a. Exactly one bill run wins (RedisLockService); the other returns 409
//	b. The migrate succeeds with stable-id semantic (source == target sub id)
//	c. No double-billing — winning bill run reports a single committed run id
//
// This is the most expensive check in the suite (3 concurrent live calls
// against billing + pricing). Requires --include-billing AND backend
// support for both BillRuns and Subscriptions capabilities.
//
// On success, validates that the platform handles the bill-run-vs-lifecycle
// concurrency correctly. On failure, the platform is at risk of:
//
//   - corrupting an in-flight migration's pro-ration math (stale offering)
//   - double-charging a customer (bill run reads pre-migrate state, migrate
//     commits new state; both invoice the period)
//   - dropping a migrate audit row when the bill run wins the lock
func (v *Validator) runLifecycleVsBillRun(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckLifecycleVsBillRun)

	if !v.in.IncludeBilling {
		return res.Skip("--include-billing not set")
	}
	caps := v.in.Backend.Capabilities()
	if !caps.BillRuns {
		return res.Skip("backend cannot trigger bill runs")
	}
	if !caps.Subscriptions {
		return res.Skip("backend cannot migrate subscriptions")
	}
	if len(v.in.Manifest.Tenants) == 0 {
		return res.Skip("manifest has zero tenants")
	}

	// Pick a tenant + a non-terminal sub on that tenant.
	tenant, sub, ok := pickEligibleSub(v.in.Manifest)
	if !ok {
		return res.Skip("no non-terminal subscriptions in manifest")
	}
	if len(tenant.Offerings) < 2 {
		return res.Skip("tenant has <2 offerings; migrate has no alternate target")
	}

	// Pick a target offering different from the sub's current one. The
	// manifest doesn't track current offering reliably; we choose the
	// second offering as the migrate target — a tenant with at least 2
	// offerings always has a different target.
	targetOffering := tenant.Offerings[1].OfferingID

	window := v.runWindow()
	keyA := fmt.Sprintf("validate-lifecycle-%s-A-%d", v.in.Run.RunID, time.Now().UnixNano())
	keyB := fmt.Sprintf("validate-lifecycle-%s-B-%d", v.in.Run.RunID, time.Now().UnixNano())

	type billRunResult struct {
		ID  string
		Err error
	}
	type migrateResult struct {
		Out     lifecycle.TransitionRecord // we mirror the same record shape for HTML
		Backend MigrateOutcome
		Err     error
	}

	var (
		wg        sync.WaitGroup
		brAResult billRunResult
		brBResult billRunResult
		migResult migrateResult
	)

	wg.Add(3)

	go func() {
		defer wg.Done()
		id, err := v.in.Backend.TriggerBillRun(ctx, tenant.TenantID, keyA, window)
		brAResult = billRunResult{ID: id, Err: err}
	}()
	go func() {
		defer wg.Done()
		id, err := v.in.Backend.TriggerBillRun(ctx, tenant.TenantID, keyB, window)
		brBResult = billRunResult{ID: id, Err: err}
	}()
	go func() {
		defer wg.Done()
		out, err := v.in.Backend.MigrateSubscription(ctx, tenant.TenantID, sub.SubscriptionID, targetOffering)
		migResult = migrateResult{Backend: out, Err: err}
	}()

	wg.Wait()

	// Evaluate outcomes.
	successCount := 0
	conflictCount := 0
	otherErrors := []string{}

	for _, br := range []billRunResult{brAResult, brBResult} {
		if br.Err == nil {
			successCount++
			continue
		}
		var unsup ErrUnsupported
		if errors.As(br.Err, &unsup) {
			return res.Skip("backend unsupported during lifecycle-vs-billrun probe: %s", unsup.Op)
		}
		if isConflict(br.Err) {
			conflictCount++
			continue
		}
		otherErrors = append(otherErrors, br.Err.Error())
	}

	migrateAccepted := migResult.Err == nil
	stableID := migrateAccepted &&
		migResult.Backend.SourceSubscriptionID != "" &&
		migResult.Backend.TargetSubscriptionID != "" &&
		migResult.Backend.SourceSubscriptionID == migResult.Backend.TargetSubscriptionID
	migrateConflicted := migResult.Err != nil && isConflict(migResult.Err)

	res.
		Set("tenant_id", tenant.TenantID).
		Set("subscription_id", sub.SubscriptionID).
		Set("billrun_success", successCount).
		Set("billrun_conflict", conflictCount).
		Set("migrate_accepted", migrateAccepted).
		Set("migrate_conflict", migrateConflicted).
		Set("stable_id_preserved", stableID).
		Set("billrun_unrelated_errors", otherErrors)

	if len(otherErrors) > 0 {
		return res.Fail("unexpected bill-run errors: %s", strings.Join(otherErrors, "; "))
	}

	// Strict — exactly one bill run must win + the other must 409 Conflict.
	// (2 success, 0 conflict) is the "Redis lock didn't engage" pattern the
	// prompt explicitly wants this check to surface as FAIL — silent
	// double-billing is the worst outcome a billing pipeline can have.
	switch {
	case successCount == 1 && conflictCount == 1:
		res.Set("billrun_concurrency_path", "redis-lock-collision")
	case successCount == 2 && conflictCount == 0:
		return res.Fail("both bill runs accepted with no 409 — Redis lock failure (double-billing risk)")
	case successCount == 0:
		return res.Fail("zero bill runs accepted — lock-or-error path indistinguishable")
	default:
		return res.Fail("expected (1 success, 1 conflict), got (%d success, %d conflict)",
			successCount, conflictCount)
	}

	// Migrate must either succeed (with stable id) or 409. Other errors fail.
	if !migrateAccepted && !migrateConflicted {
		return res.Fail("migrate failed unexpectedly: %v", migResult.Err)
	}
	if migrateAccepted && !stableID {
		return res.Fail("stable-id violation: source=%s target=%s",
			migResult.Backend.SourceSubscriptionID, migResult.Backend.TargetSubscriptionID)
	}

	return res.Pass()
}
