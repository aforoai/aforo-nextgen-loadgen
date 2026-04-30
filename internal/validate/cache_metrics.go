package validate

import (
	"context"
	"errors"
)

// runCacheHitRatio is Check 4 — BillingHierarchyEnricher's Redis cache hit
// ratio over the run window. Below the scenario threshold (default 0.95)
// indicates either too much cache thrash (TTL too short) or per-event
// cache eviction (a regression).
//
// Hit ratio is exposed via /actuator/metrics on usage-ingestor. Offline
// mode SKIPs because run.json doesn't carry cache metrics.
func (v *Validator) runCacheHitRatio(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckCacheHitRatio)

	if !v.in.Backend.Capabilities().CacheMetrics {
		return res.Skip("backend does not expose cache metrics")
	}

	threshold := v.in.Scenario.Assertions.RedisCacheHitRatioMin
	if threshold == 0 {
		// Scenario didn't set a threshold — record the value but don't gate
		// on it. The check is informational rather than mandatory.
		ratio, err := v.in.Backend.CacheHitRatio(ctx, v.runWindow())
		if err != nil {
			var unsup ErrUnsupported
			if errors.As(err, &unsup) {
				return res.Skip("backend unsupported: %s", unsup.Op)
			}
			return res.Fail("backend CacheHitRatio: %v", err)
		}
		return res.Set("hit_ratio", ratio).Set("threshold", threshold).Pass()
	}

	ratio, err := v.in.Backend.CacheHitRatio(ctx, v.runWindow())
	if err != nil {
		var unsup ErrUnsupported
		if errors.As(err, &unsup) {
			return res.Skip("backend unsupported: %s", unsup.Op)
		}
		return res.Fail("backend CacheHitRatio: %v", err)
	}

	res.Set("hit_ratio", ratio).Set("threshold", threshold)
	if ratio < threshold {
		return res.Fail("cache hit ratio %.4f < threshold %.4f", ratio, threshold)
	}
	return res.Pass()
}
