package seed

import (
	"context"
	"fmt"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// CleanResult summarizes the surgical archive pass.
type CleanResult struct {
	TenantsArchived       int
	CustomersArchived     int
	SubscriptionsCanceled int
	OfferingsArchived     int
	RatePlansArchived     int
	ProductsArchived      int
	WalletsArchived       int
	Errors                []error
}

// Clean archives every entity that a prior run wrote. It is best-effort —
// individual failures are recorded in CleanResult.Errors and the pass
// continues. The user-visible promise is "no hard deletes": every entity is
// status-flipped to ARCHIVED or CANCELLED so audit history is preserved.
//
// Order of operations is the inverse of provisioning: subscriptions are
// canceled before offerings (cancellation cascades through the platform's
// integrity rules), products and rate plans are archived after offerings,
// then customers, then wallets, then tenants.
//
// scenarioName + runIDPrefix are provided so we can scope the clean to a
// single run if requested. If both are empty, the manifest is the only
// source of truth (full clean of whatever the manifest references).
func Clean(ctx context.Context, c *Client, m *Manifest) CleanResult {
	res := CleanResult{}
	if m == nil {
		return res
	}

	for _, t := range m.Tenants {
		// Cancel subscriptions first so the platform's integrity guards on
		// rate plans / offerings don't reject the archive.
		for _, cust := range t.Customers {
			for _, sub := range cust.Subscriptions {
				if sub.Status == scenario.StateCancelled || sub.Status == scenario.StateExpired {
					// Already in a terminal state — no-op.
					continue
				}
				if err := archiveSubscription(ctx, c, t.TenantID, sub.SubscriptionID); err != nil {
					res.Errors = append(res.Errors, fmt.Errorf("tenant %s sub %s: %w", t.TenantID, sub.SubscriptionID, err))
					continue
				}
				res.SubscriptionsCanceled++
			}
		}

		for _, off := range t.Offerings {
			if err := archiveOffering(ctx, c, t.TenantID, off.OfferingID); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("tenant %s offering %s: %w", t.TenantID, off.OfferingID, err))
				continue
			}
			res.OfferingsArchived++
		}

		for _, rp := range t.RatePlans {
			if err := archiveRatePlan(ctx, c, t.TenantID, rp.RatePlanID); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("tenant %s rate plan %s: %w", t.TenantID, rp.RatePlanID, err))
				continue
			}
			res.RatePlansArchived++
		}

		for _, p := range t.Products {
			if err := archiveProduct(ctx, c, t.TenantID, p.ProductID); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("tenant %s product %s: %w", t.TenantID, p.ProductID, err))
				continue
			}
			res.ProductsArchived++
		}

		for _, cust := range t.Customers {
			for _, sub := range cust.Subscriptions {
				if sub.WalletID == "" {
					continue
				}
				if err := archiveWallet(ctx, c, t.TenantID, sub.WalletID); err != nil {
					res.Errors = append(res.Errors, fmt.Errorf("tenant %s wallet %s: %w", t.TenantID, sub.WalletID, err))
					continue
				}
				res.WalletsArchived++
			}
			if err := archiveCustomer(ctx, c, t.TenantID, cust.CustomerID); err != nil {
				res.Errors = append(res.Errors, fmt.Errorf("tenant %s customer %s: %w", t.TenantID, cust.CustomerID, err))
				continue
			}
			res.CustomersArchived++
		}

		if err := archiveTenant(ctx, c, t.TenantID); err != nil {
			res.Errors = append(res.Errors, fmt.Errorf("tenant %s: %w", t.TenantID, err))
			continue
		}
		res.TenantsArchived++
	}
	return res
}
