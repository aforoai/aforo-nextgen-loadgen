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
// Why phase-1 ships as no-op rather than removed entirely: removing
// would force a same-PR refactor of seeder.go's discount-call path,
// the ManifestDiscount type, and every scenario YAML with a discount
// block. Phase-1 preserves the manifest data + caller signature so
// phase-2 is a focused 1-file change.
//
// ────────────────────────────────────────────────────────────────────────
// PHASE-2 — NEXT-PR SCOPE (self-contained; not started yet)
// ────────────────────────────────────────────────────────────────────────
//
// Goal: replace the no-op with a real mint-then-redeem flow against the
// V49 Coupon Engine so scenarios carrying `cp.Discount != nil` actually
// produce discounted invoices.
//
// The next PR should land all four steps below atomically. Steps are
// listed in dependency order; each names the verified backend endpoint +
// request shape so the implementer doesn't have to re-discover.
//
// STEP 1 — mint a coupon  (call once per (tenant, discount-shape))
//
//	Endpoint: POST {pricing}/api/v1/coupons
//	Auth:     loadgen's BILLING_ADMIN bearer (already configured)
//	Body:     CreateCouponRequest
//	            code              — REQUIRED, UPPERCASE matching
//	                                ^[A-Z0-9_-]+$, max 64 chars.
//	                                Loadgen MUST uppercase the seedKey;
//	                                pricing-service rejects lowercase.
//	            name              — REQUIRED, human label
//	            discountType      — REQUIRED enum PERCENTAGE | FIXED_AMOUNT
//	                                (map from d.Type)
//	            discountValue     — REQUIRED BigDecimal (map from d.Value)
//	            currency          — Size 3-3 ISO 4217 (required when
//	                                discountType=FIXED_AMOUNT)
//	            duration          — REQUIRED enum ONCE | REPEATING | FOREVER
//	            durationInMonths  — REQUIRED when duration=REPEATING (1-60)
//	            maxRedemptions    — set to scenario-wide subscriber count
//	                                so one coupon serves the whole archetype
//	            maxRedemptionsPerCustomer — keep default 1
//	Response: CouponResponse, the load-bearing field is `couponId`
//	          (NOT `id` — confirmed via OpenAPI snapshot of pricing).
//	          Cache it keyed on (tenantID, type, value, duration) so a
//	          repeat call for the same archetype skips the mint POST.
//
// STEP 2 — redeem the coupon onto the subscription
//
//	Endpoint: POST {pricing}/api/v1/coupons/redeem-for-customer
//	          ?customerId=<subscription.customerId>
//	Auth:     loadgen's BILLING_ADMIN bearer (operator-side admin path)
//	Body:     RedeemCouponRequest
//	            code           — the value from STEP 1's request
//	            subscriptionId — subscription's id
//	Response: CouponRedemptionResponse, load-bearing field is
//	          `redemptionId` (NOT `id`)
//
//	Why the admin path, not the storefront BFF:
//	Loadgen carries an operator bearer (BILLING_ADMIN role) and has no
//	X-Storefront-Key or session cookie. The customer-facing storefront
//	endpoint (POST {storefront}/api/v1/portal/coupons/redeem) requires
//	both headers + JWT subject = customerId, which loadgen can't satisfy
//	without minting a portal session per customer. The admin endpoint is
//	the right shape for loadgen's auth model.
//
// STEP 3 — extend the manifest
//
//	Add to ManifestDiscount in internal/seed/manifest.go:
//	    CouponID     string `json:"coupon_id,omitempty"`
//	    CouponCode   string `json:"coupon_code,omitempty"`
//	    RedemptionID string `json:"redemption_id,omitempty"`
//	Populate from STEP 1's couponId + the uppercase code + STEP 2's
//	redemptionId before returning from applyDiscount. The run engine
//	uses CouponID to JOIN coupon_redemptions when asserting that
//	invoice line items carry the expected discount applications.
//
// STEP 4 — register the new types in the contract test
//
//	In internal/seed/contract_test.go contractEntries() add:
//	    - pricing/CreateCouponRequest  → couponCreateRequest (DirRequest)
//	    - pricing/CouponResponse        → couponResponse      (DirResponse)
//	    - pricing/RedeemCouponRequest   → couponRedeemRequest (DirRequest)
//	    - pricing/CouponRedemptionResponse → couponRedeemResponse (DirResponse)
//	Then delete the existing entry for pricing/ApplyDiscountRequest
//	(line 184-188 of contract_test.go) — that schema no longer exists in
//	the V49 backend, the contract test currently SKIPs on snapshot-missing
//	but will fail once snapshots are bootstrapped.
//
// Acceptance criteria for the phase-2 PR
//
//   - Scenarios with `cp.Discount != nil` complete without 404
//   - Manifest entries carry non-empty CouponID + RedemptionID
//   - `make contract-test` passes against bootstrapped snapshots
//   - Unit test in discounts_test.go covering: idempotent re-call returns
//     same CouponID (caches mint), 409 on duplicate redemption surfaces
//     as no-op success (idempotency contract per backend Javadoc)
//   - This package doc block updates to PHASE-2 SHIPPED with date +
//     commit hash
//
// Out of scope for phase-2 (file as phase-3)
//
//   - Coupon expiry / archive cleanup (loadgen seeds with high
//     maxRedemptions; expiry-window churn lives in the run engine)
//   - Per-redemption proration validation against billing-service
//     invoice line items (that's a billing-side assertion, not a seed
//     concern)
//   - Storefront-portal coupon redemption path (would require minting
//     portal sessions per customer — defer until a scenario actually
//     needs to exercise the customer-facing flow)
//
// Implementer can pick this up cold. No external dependencies on other
// in-flight work. Estimated ~150 LOC + ~80 LOC of tests.
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
