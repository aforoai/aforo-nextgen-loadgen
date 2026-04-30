package validate

import (
	"context"
	"errors"
)

// runBillingHierarchy is Check 3 — events ingested with customer_id IS NULL
// indicate that BillingHierarchyEnricher failed to resolve the api_key →
// agent → team → member chain at ingest time. This is a billing-correctness
// bug: events that don't resolve a customer can't be invoiced.
//
// Tolerance: zero. A single null-customer row is a regression.
func (v *Validator) runBillingHierarchy(ctx context.Context) *CheckResult {
	res := NewCheckResult(CheckHierarchy)

	if !v.in.Backend.Capabilities().EventQueries {
		return res.Skip("backend cannot answer event-count queries")
	}

	n, err := v.in.Backend.EventsWithNullCustomer(ctx, v.runWindow())
	if err != nil {
		var unsup ErrUnsupported
		if errors.As(err, &unsup) {
			return res.Skip("backend unsupported: %s", unsup.Op)
		}
		return res.Fail("backend EventsWithNullCustomer: %v", err)
	}

	res.Set("events_with_null_customer", n)
	if n > 0 {
		return res.Fail("%d event(s) ingested with NULL customer_id — BillingHierarchyEnricher regression", n)
	}
	return res.Pass()
}
