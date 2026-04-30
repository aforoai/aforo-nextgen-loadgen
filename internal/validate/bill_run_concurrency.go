package validate

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// runBillRunConcurrency is Check 8 — fire two simultaneous bill runs against
// the same tenant for the same window and assert that exactly one succeeds
// while the other returns 409 Conflict (Aforo's RedisLockService enforces
// this; the lock key is per-tenant-per-window).
//
// This check requires --include-billing because it triggers real bill runs.
// When opt-in is off, SKIPs.
func (v *Validator) runBillRunConcurrency(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckBillRunConcurrency)

	if !v.in.IncludeBilling {
		return res.Skip("--include-billing not set")
	}
	if !v.in.Backend.Capabilities().BillRuns {
		return res.Skip("backend cannot trigger bill runs")
	}
	if len(v.in.Manifest.Tenants) == 0 {
		return res.Skip("manifest has zero tenants")
	}

	tenant := v.in.Manifest.Tenants[0]
	window := v.runWindow()

	// Two distinct idempotency keys so we genuinely race the lock — same
	// idempotency-key would resolve to the same logical run via Aforo's
	// idempotent-replay shortcut, defeating the test.
	keyA := fmt.Sprintf("validate-concurrency-%s-A-%d", v.in.Run.RunID, time.Now().UnixNano())
	keyB := fmt.Sprintf("validate-concurrency-%s-B-%d", v.in.Run.RunID, time.Now().UnixNano())

	type fireResult struct {
		BillRunID string
		Err       error
	}
	resCh := make(chan fireResult, 2)
	var wg sync.WaitGroup

	fire := func(key string) {
		defer wg.Done()
		id, err := v.in.Backend.TriggerBillRun(ctx, tenant.TenantID, key, window)
		resCh <- fireResult{BillRunID: id, Err: err}
	}

	wg.Add(2)
	go fire(keyA)
	go fire(keyB)
	wg.Wait()
	close(resCh)

	var outcomes []fireResult
	for r := range resCh {
		outcomes = append(outcomes, r)
	}

	successCount := 0
	conflictCount := 0
	otherErrors := []string{}
	for _, o := range outcomes {
		if o.Err == nil {
			successCount++
			continue
		}
		var unsup ErrUnsupported
		if errors.As(o.Err, &unsup) {
			return res.Skip("backend unsupported during concurrency probe: %s", unsup.Op)
		}
		if isConflict(o.Err) {
			conflictCount++
			continue
		}
		otherErrors = append(otherErrors, o.Err.Error())
	}

	res.
		Set("success_count", successCount).
		Set("conflict_count", conflictCount).
		Set("unrelated_errors", otherErrors)

	if successCount == 1 && conflictCount == 1 && len(otherErrors) == 0 {
		return res.Pass()
	}
	if len(otherErrors) > 0 {
		return res.Fail("unexpected errors: %s", strings.Join(otherErrors, "; "))
	}
	return res.Fail("expected (1 success, 1 conflict); got (%d success, %d conflict)",
		successCount, conflictCount)
}

// isConflict matches the platform's 409 Conflict shape returned by
// RedisLockService when a concurrent bill run is already running.
func isConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "409") ||
		strings.Contains(strings.ToLower(msg), "conflict") ||
		strings.Contains(strings.ToLower(msg), "lock held")
}
