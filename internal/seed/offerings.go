package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// offeringCreateRequest mirrors pricing-service's CreateOfferingRequest.
// Escrow config fields are populated only for PREPAID/HYBRID modes.
//
// Field-name contract (verified against pricing-service
// CreateOfferingRequest.java):
//   - code — @NotBlank, unique-per-tenant identifier. Omitting it returns
//     400 "Offering code is required". We reuse the loadgen externalId as
//     the code so re-runs hit the per-tenant UNIQUE constraint and the
//     server returns 409 (which loadgen's conflict path handles).
//   - primaryRatePlanId (NOT "ratePlanId") — Size 36, the rate plan this
//     offering wraps. Loadgen previously sent "ratePlanId" which the DTO
//     does not declare; the server silently dropped it and the offering
//     created with no rate plan, breaking downstream subscription create.
//   - type — STANDALONE | BUNDLE | ADDON. We always create STANDALONE.
//   - status — DRAFT | ACTIVE | ARCHIVED. We create ACTIVE so the offering
//     is immediately available for subscriptions.
//   - externalId is NOT a DTO field — silently dropped server-side.
//     Loadgen keeps it on the body for forward-compat.
//
// walletInitialBalanceUsd: NOT a DTO field on CreateOfferingRequest. Wallet
// initial balance lives on the wallet resource, not the offering. Loadgen
// keeps the field for forward-compat but it's currently a no-op.
// CONVENTION (see CONVENTIONS.md "Wire-format alignment"): EVERY field on
// this struct maps to a real CreateOfferingRequest.java column.
// Deterministic identity for cross-day lookup is `code` (per-tenant
// UNIQUE on the entity, driven by lookupOfferingByCode). Idempotency-Key
// is the loadgen-internal seedKey set by provisionOffering.
//
// Round-2 audit drop: `walletInitialBalanceUsd` was previously declared
// here as a "forward-compat phantom for the day backend lifts the wallet
// initial balance to the offering". Backend CreateOfferingRequest.java
// genuinely doesn't have it (verified — wallet initial balance lives on
// the wallet resource itself, not the offering). Per the no-phantom-
// fields convention, dropped. The wallet's initial balance is now passed
// directly to provisionWallet.
type offeringCreateRequest struct {
	Code              string `json:"code"`
	Name              string `json:"name"`
	Description       string `json:"description,omitempty"`
	Type              string `json:"type,omitempty"`
	Status            string `json:"status,omitempty"`
	PrimaryRatePlanID string `json:"primaryRatePlanId"`
	BillingMode       string `json:"billingMode"`
	Currency          string `json:"currency"`
	TrialDays         int    `json:"trialDays,omitempty"`
}

// offeringResponse mirrors the subset of pricing-service's OfferingResponse
// that the seed harness consumes.
//
// Drift-fix (2026-05-27): the response no longer reads `externalId` —
// pricing-service has no such field on the entity or DTO (verified against
// aforo-nextgen-pricing-service/.../OfferingResponse.java). The previous
// loadgen response struct read `json:"externalId"` which always decoded to
// "" and made every lookupOfferingByExternalID call return "not found".
// `code` is the per-tenant UNIQUE field (loadgen sets `Code: externalID`
// on the create request), so it's the right deterministic identity key.
type offeringResponse struct {
	ID   string `json:"id"`
	Code string `json:"code"`
	Name string `json:"name"`
}

// provisionOffering wraps a rate plan in a billing-mode offering. The
// validator already ensures wallet_initial_balance_usd > 0 for PREPAID/HYBRID
// archetypes — we surface it on the offering so the platform's escrow logic
// has the budget value at subscription create.
//
// Idempotency strategy (drift-fix 2026-05-27):
//   - Within 24h: Idempotency-Key header on POST.
//   - Cross-day / DB-reset: lookupOfferingByCode runs first and uses
//     pricing-service's /offerings/search?name= (substring) + client-side
//     filter by exact `code` match. `code` is per-tenant UNIQUE on the
//     entity, so an exact match guarantees the right row.
//
// Parameters:
//   - seedKey: loadgen-internal opaque deterministic string. Re-used as the
//     offering's `code` (which IS a real CreateOfferingRequest column — per-
//     tenant UNIQUE) because the seedKey shape already satisfies code's
//     uniqueness contract. So seedKey plays two roles: (a) HTTP
//     Idempotency-Key header value, (b) the `code` body field. The natural
//     identity used by lookupOfferingByCode is `code`. See CONVENTIONS.md.
func provisionOffering(ctx context.Context, c *Client, tenantID, seedKey string, a scenario.TenantArchetype, ratePlanID, currency string) (offeringResponse, error) {
	code := seedKey // the seedKey IS the offering code (real DTO column).

	if existing, ok, err := lookupOfferingByCode(ctx, c, tenantID, code); err != nil {
		return offeringResponse{}, fmt.Errorf("lookup offering %q: %w", code, err)
	} else if ok {
		return existing, nil
	}
	// Name MUST NOT contain square brackets — same forward-compat
	// reasoning as products + rate plans (see products.go).
	body := offeringCreateRequest{
		Code:              code,
		Name:              fmt.Sprintf("Loadgen Offering %s", a.Name),
		Description:       fmt.Sprintf("auto-provisioned for archetype=%s", a.Name),
		Type:              "STANDALONE",
		Status:            "ACTIVE",
		PrimaryRatePlanID: ratePlanID,
		BillingMode:       string(a.BillingMode),
		Currency:          currency,
		TrialDays:         a.RateConfig.TrialDays,
	}
	// Wallet initial balance is intentionally NOT set on the offering body
	// (round-2 audit removed walletInitialBalanceUsd as a phantom field).
	// Backend honors it via provisionWallet's body.InitialBalance instead.
	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathOfferings)
	if err != nil {
		return offeringResponse{}, err
	}
	var resp offeringResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: seedKey,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupOfferingByCode(ctx, c, tenantID, code)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return offeringResponse{}, fmt.Errorf("create offering (seedKey=%q): %w", seedKey, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-offering-" + seedKey
		resp.Code = code
		resp.Name = body.Name
	}
	return resp, nil
}

// lookupOfferingByCode pages through pricing-service's GET /api/v1/offerings
// (no server-side `code` filter on OfferingController list) and matches
// client-side by exact code. Loadgen's `code = externalID` convention means
// at most one row matches per tenant; the per-tenant UNIQUE constraint
// enforces this server-side.
func lookupOfferingByCode(ctx context.Context, c *Client, tenantID, code string) (offeringResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathOfferings)
	if err != nil {
		return offeringResponse{}, false, err
	}
	var offerings []offeringResponse
	if _, err := listAllOptional(ctx, c, listURL, RequestOptions{TenantID: tenantID}, &offerings); err != nil {
		return offeringResponse{}, false, err
	}
	for _, o := range offerings {
		if o.Code == code {
			return o, true, nil
		}
	}
	return offeringResponse{}, false, nil
}

// archiveOffering soft-archives via DELETE.
func archiveOffering(ctx context.Context, c *Client, tenantID, offeringID string) error {
	if offeringID == "" {
		return nil
	}
	delURL, err := c.Target().Path(aforo.ServicePricing, fmt.Sprintf(aforo.PathOfferingByID, offeringID))
	if err != nil {
		return err
	}
	if err := c.Do(ctx, http.MethodDelete, delURL, nil, nil, RequestOptions{TenantID: tenantID}); err != nil {
		if aforo.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("archive offering %s: %w", offeringID, err)
	}
	return nil
}
