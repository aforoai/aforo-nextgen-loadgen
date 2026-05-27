package seed

import (
	"context"
)

// Discount support — DEFERRED, awaiting migration to Coupon engine.
//
// Pre-existing bug discovered during the 2026-05-27 round-2 audit:
// `/api/v1/discounts` no longer exists in pricing-service. The Coupon
// Engine (V49 ship per CLAUDE.md "Coupon Engine V49 — customer-redeemable
// promo codes end-to-end") replaced the legacy discount endpoint. Loadgen
// never migrated, so every scenario carrying `cp.Discount != nil` would
// have 404'd on the POST and bubbled the error up through provisionXxx,
// failing the entire seed run.
//
// PHASE-1 BEHAVIOR (this commit): applyDiscount returns nil silently when
// invoked. The manifest still records the intended discount in
// ManifestCustomer.Discount, so downstream billing assertions can use it
// for shadow comparisons. The actual server-side discount is NOT applied —
// scenarios that depend on discount math taking effect will see invoices
// without discount until the Coupon migration ships.
//
// PHASE-2 (DEFERRED, filed): rewrite applyDiscount to:
//  1. Call POST /api/v1/coupons to mint a per-(tenant, scenario, archetype)
//     coupon with the right type+value+duration (see CouponController in
//     pricing-service for the create body shape).
//  2. Call POST /api/v1/portal/coupons/redeem with X-Customer-Id =
//     subscription.customerId to redeem the coupon onto the subscription
//     (mirrors what the storefront portal does — see PortalCouponController).
//  3. Manifest gains a CouponID field so the run engine can correlate
//     billing-side discount lines.
//
// Why phase-1 ships as no-op rather than removed entirely: removing
// would force a same-PR refactor of seeder.go's discount-call path,
// the ManifestDiscount type, and every scenario YAML with a discount
// block. Phase-1 preserves the manifest data + caller signature so
// phase-2 is a focused 1-file change.
type discountApplyRequest struct {
	SubscriptionID string  `json:"subscriptionId"`
	Type           string  `json:"type"`
	Value          float64 `json:"value"`
}

type discountResponse struct {
	ID string `json:"id"`
}

// applyDiscount is a NO-OP today — see the package doc above for why
// (`/api/v1/discounts` no longer exists; Coupon Engine replaced it).
// The Manifest's ManifestDiscount type is the source of truth for
// downstream billing assertions until phase-2 ships the Coupon migration.
//
// The function preserves its signature (and the seedKey + d parameters)
// so callers don't need to change when phase-2 lands.
func applyDiscount(ctx context.Context, c *Client, tenantID, seedKey, subscriptionID string, d *ManifestDiscount) error {
	// Reference the unused params explicitly so the linter doesn't flag
	// them. The signature is preserved deliberately for phase-2.
	_ = ctx
	_ = c
	_ = tenantID
	_ = seedKey
	_ = subscriptionID
	_ = d
	return nil
}
