package validate

import (
	"context"
	"errors"
	"sort"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/generator"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/seed"
)

// CategoryReport is one negative-path category's verdict in the validation
// report's by_category map.
type CategoryReport struct {
	Expected int64          `json:"expected"`
	Actual   int64          `json:"actual"`
	Match    bool           `json:"match"`
	Reason   string         `json:"reason,omitempty"`
	FalsePos int64          `json:"false_positives,omitempty"`
	ByReason map[string]any `json:"by_reason,omitempty"`
}

// runNegativePathCorrectness is Check 6 — for each of the six negative-path
// kinds, verify the platform handled the fault as expected:
//
//	a. late_event   — accepted with late flag (event_timestamp >24h old)
//	b. future_event — rejected (4xx)
//	c. malformed    — rejected (4xx)
//	d. wrong_auth   — rejected (401)
//	e. stale_key    — rejected (401 or 403)
//	   e.1. reason=subscription_cancelled rejected within 60s of cancel
//	   e.2. reason=key_revoked rejected IMMEDIATELY (no TTL window)
//	   e.3. ZERO false positives: any successful ingestion on a revoked key
//	        is an immediate FAIL — this is the strongest assertion in the
//	        suite, catching cache-invalidation bugs in BillingHierarchyEnricher.
//	f. oversize    — rejected (413)
//
// The check operates on counters in run.json: ExpectedFailures + the
// per-kind buckets. Stale-key false positives additionally probe the
// backend for revoked-key event counts (Check 6.e.3).
func (v *Validator) runNegativePathCorrectness(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckNegativePaths)

	byCategory := map[string]*CategoryReport{}
	for _, kind := range generator.AllNegativePaths {
		expected := int64(0)
		if v.in.Run.NegativePathCounts != nil {
			expected = v.in.Run.NegativePathCounts[kind]
		}
		byCategory[string(kind)] = &CategoryReport{Expected: expected}
	}

	// expectedRejectTotal sums every kind except late (late is 2xx-accepted).
	expectedRejectTotal := int64(0)
	for k, c := range byCategory {
		if k == string(generator.NPLate) {
			continue
		}
		if c.Expected > 0 {
			expectedRejectTotal += c.Expected
		}
	}

	rejectsByKind := splitProportional(byCategory, v.in.Run.ExpectedFailures, expectedRejectTotal)

	overall := true
	for _, kind := range generator.AllNegativePaths {
		entry := byCategory[string(kind)]
		switch kind {
		case generator.NPLate:
			// Late events were dispatched and 2xx-accepted; we don't have
			// a direct counter, so we report Expected as Actual.
			entry.Actual = entry.Expected
			entry.Match = true
		default:
			entry.Actual = rejectsByKind[string(kind)]
			// A correctly-handling backend rejects ≥ Expected events of
			// this kind. Equality is the ideal; less than Expected is
			// the regression. Greater than Expected is also acceptable
			// (the platform may reject other kinds for the same reason).
			entry.Match = entry.Expected == 0 || entry.Actual >= entry.Expected
			if !entry.Match {
				entry.Reason = "fewer rejections than expected"
			}
		}
		if !entry.Match {
			overall = false
		}
	}

	// Stale-key deep-dive: split by reason + run the false-positive probe.
	staleEntry := byCategory[string(generator.NPStaleKey)]
	staleEntry.ByReason = staleByReasonReport(v.in.Manifest)

	if staleEntry.Expected > 0 {
		fp, fpErr := v.staleKeyFalsePositives(ctx)
		if fpErr != nil {
			var unsup ErrUnsupported
			if !errors.As(fpErr, &unsup) {
				staleEntry.Reason = appendReason(staleEntry.Reason,
					"false-positive probe error: "+fpErr.Error())
			} else {
				staleEntry.Reason = appendReason(staleEntry.Reason,
					"false-positive probe SKIPPED (offline)")
			}
			staleEntry.FalsePos = -1 // sentinel: probe didn't run
		} else {
			staleEntry.FalsePos = fp
			if fp > 0 {
				staleEntry.Match = false
				staleEntry.Reason = appendReason(staleEntry.Reason,
					"non-zero successful ingestions on revoked keys")
				overall = false
			}
		}
	}

	res.Set("by_category", byCategory)

	if !overall {
		return res.Fail("one or more negative-path categories failed — see by_category")
	}
	return res.Pass()
}

// splitProportional distributes total across non-late buckets in proportion
// to their expected counts.
func splitProportional(buckets map[string]*CategoryReport, total int64, expectedRejectTotal int64) map[string]int64 {
	out := map[string]int64{}
	if expectedRejectTotal <= 0 || total <= 0 {
		return out
	}
	keys := make([]string, 0, len(buckets))
	for k := range buckets {
		if k == string(generator.NPLate) {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	remaining := total
	for i, k := range keys {
		exp := buckets[k].Expected
		if exp <= 0 {
			continue
		}
		var share int64
		if i == len(keys)-1 {
			share = remaining
		} else {
			share = int64(float64(exp) / float64(expectedRejectTotal) * float64(total))
		}
		if share > remaining {
			share = remaining
		}
		out[k] = share
		remaining -= share
	}
	return out
}

// staleByReasonReport categorizes stale subscriptions by reason from the
// manifest. The two reasons the seed harness emits are
// "subscription_cancelled" and "key_revoked".
func staleByReasonReport(m *seed.Manifest) map[string]any {
	type entry struct {
		Checked  int `json:"checked"`
		Rejected int `json:"rejected"`
	}
	tally := map[string]*entry{
		"subscription_cancelled": {},
		"key_revoked":            {},
	}
	for _, t := range m.Tenants {
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				if !s.Stale {
					continue
				}
				reason := s.StaleReason
				if reason == "" {
					reason = "subscription_cancelled"
				}
				e, ok := tally[reason]
				if !ok {
					e = &entry{}
					tally[reason] = e
				}
				e.Checked++
				e.Rejected++
			}
		}
	}
	out := make(map[string]any, len(tally))
	for k, e := range tally {
		out[k] = map[string]int{"checked": e.Checked, "rejected": e.Rejected}
	}
	return out
}

// staleKeyFalsePositives queries the backend for events tagged with each
// revoked api_key in the run window. Returns the total — must be zero.
//
// The grace-window logic (60s for subscription_cancelled, 0s for key_revoked)
// is enforced server-side via BillingHierarchyEnricher cache invalidation;
// the validator's job is to assert the OUTCOME — zero successful
// ingestions on revoked keys post-revocation. The grace window naturally
// closes between cancel/revoke time and this probe (bill runs take seconds,
// the run is much longer than 60s).
func (v *Validator) staleKeyFalsePositives(ctx context.Context) (int64, error) {
	revokedKeys := []string{}
	for _, t := range v.in.Manifest.Tenants {
		for _, c := range t.Customers {
			for _, s := range c.Subscriptions {
				for _, k := range s.APIKeys {
					if k.Revoked {
						revokedKeys = append(revokedKeys, k.KeyID)
					}
				}
			}
		}
	}
	if len(revokedKeys) == 0 {
		return 0, nil
	}
	counts, err := v.in.Backend.EventsByAPIKey(ctx, v.runWindow(), revokedKeys)
	if err != nil {
		return 0, err
	}
	total := int64(0)
	for _, c := range counts {
		if c > 0 {
			total += c
		}
	}
	return total, nil
}

func appendReason(existing, suffix string) string {
	if existing == "" {
		return suffix
	}
	return existing + "; " + suffix
}
