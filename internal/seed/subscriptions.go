package seed

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// subscriptionCreateRequest mirrors pricing-service's V3
// CreateSubscriptionRequest.
//
// Field-name + shape contract (verified against pricing-service
// CreateSubscriptionRequest.java):
//   - customerId — @NotBlank.
//   - offeringId — @NotBlank.
//   - startDate — @NotNull, Java LocalDate. Jackson decodes this as
//     "YYYY-MM-DD"; sending a full RFC3339 timestamp (e.g.
//     "2026-05-27T14:23:00Z") yields a 400 deserialization error. Loadgen
//     uses a string of shape "2006-01-02" rather than time.Time so the
//     wire format matches LocalDate exactly.
//   - endDate — optional LocalDate.
//   - billingCycle — MONTHLY | QUARTERLY | ANNUAL (default MONTHLY on the
//     server but we send explicitly so the wire is self-describing).
//   - startTrial + trialEndsAt — V3 trial flow. startTrial=true enters the
//     subscription in TRIALING state; trialEndsAt is an Instant
//     ("2026-05-27T14:23:00Z") that overrides the offering's trialDays.
//
// CONVENTION (see CONVENTIONS.md "Wire-format alignment"): EVERY field on
// this struct maps to a real CreateSubscriptionRequest.java column.
// Deterministic identity for cross-day lookup is the (customerId, offeringId)
// pair (queried via lookupSubscriptionByCustomerAndOffering using the real
// `?customerId=` filter). Idempotency-Key is the loadgen-internal seedKey
// set by provisionSubscription.
//
// Drift-fix (rename pass): `walletId` / `paymentMethodId` previously sat on
// this struct as "forward-compat phantom fields hoping backend would adopt
// them"; both are NOT on CreateSubscriptionRequest (verified). Removed per
// the no-phantom-fields convention — the wallet binding is implicit (one
// wallet per customer + currency) and the payment method binding is
// implicit (the customer's default PaymentMethod), so loadgen's downstream
// flows still work without passing these on the create.
//
// `endDate` IS a real CreateSubscriptionRequest column (verified — it
// drives the SubscriptionExpiryJob's nightly scan when set in the past).
// Round-2 audit caught that the rename pass had wrongly dropped it.
// Restored.
type subscriptionCreateRequest struct {
	OfferingID   string     `json:"offeringId"`
	CustomerID   string     `json:"customerId"`
	BillingCycle string     `json:"billingCycle,omitempty"`
	StartDate    string     `json:"startDate"`
	EndDate      string     `json:"endDate,omitempty"`
	StartTrial   bool       `json:"startTrial,omitempty"`
	TrialEndsAt  *time.Time `json:"trialEndsAt,omitempty"`
}

// subscriptionResponse mirrors the subset of pricing-service's
// SubscriptionResponse that the seed harness consumes.
//
// Drift-fix (2026-05-27): the response no longer reads `externalId` —
// pricing-service has no such field on the entity or DTO (verified against
// aforo-nextgen-pricing-service/.../SubscriptionResponse.java; fields are
// id, tenantId, customerId, offeringId, status, ...; NO externalId). The
// previous loadgen response struct read `json:"externalId"` which always
// decoded to "" and made every lookupSubscriptionByExternalID call return
// "not found", silently fanning out duplicate creates on cross-day reruns.
// (customerId, offeringId) is the deterministic identity pair — loadgen
// creates one subscription per (customer, offering) — and drives
// lookupSubscriptionByCustomerAndOffering.
type subscriptionResponse struct {
	ID         string    `json:"id"`
	CustomerID string    `json:"customerId"`
	OfferingID string    `json:"offeringId"`
	Status     string    `json:"status"`
	CreatedAt  time.Time `json:"createdAt"`
}

type subscriptionCancelRequest struct {
	Reason string `json:"reason,omitempty"`
}

// provisionSubscription creates one subscription. The platform's V3 state
// machine starts subscriptions in CREATED → TRIALING (if trial) → ACTIVE.
// We then follow up with state-transition calls for TRIALING/PAUSED/etc.
//
// CANCELLED and EXPIRED states are achieved POST-create via cancel/expire
// calls — the platform emits the atomic key-revocation cascade only when
// going through those endpoints.
// Drift-fix (2026-05-27): cross-day idempotency now uses
// lookupSubscriptionByCustomerAndOffering (the deterministic identity pair
// pricing-service actually exposes) rather than the previous lookup-by-
// externalId which never matched (SubscriptionResponse has no externalId
// field).
//
// Parameters:
//   - seedKey: loadgen-internal opaque deterministic string sent as the
//     HTTP Idempotency-Key header. See CONVENTIONS.md.
func provisionSubscription(ctx context.Context, c *Client, tenantID, seedKey, offeringID, customerID, walletID, paymentMethodID string, target scenario.SubscriptionState, archetype string) (subscriptionResponse, time.Time, error) {
	if existing, ok, err := lookupSubscriptionByCustomerAndOffering(ctx, c, tenantID, customerID, offeringID); err != nil {
		return subscriptionResponse{}, time.Time{}, fmt.Errorf("lookup sub for customer=%q offering=%q: %w", customerID, offeringID, err)
	} else if ok {
		// Existing subscription — re-applying state transitions if necessary
		// is handled by transitionSubscription below.
		return existing, time.Time{}, nil
	}

	now := time.Now().UTC()
	body := subscriptionCreateRequest{
		OfferingID:   offeringID,
		CustomerID:   customerID,
		BillingCycle: "MONTHLY",
		StartDate:    now.Format("2006-01-02"), // pricing-service LocalDate
	}
	// walletID + paymentMethodID are NO LONGER sent on the create body —
	// backend doesn't have those columns on CreateSubscriptionRequest
	// (per CONVENTIONS.md no-phantom-fields rule). The implicit wallet
	// binding (one company wallet per customer) + implicit default
	// payment method discovery is what backend already does. The
	// parameters remain in the function signature for back-compat with
	// callers that pass them; they're recorded into the manifest by the
	// caller (seeder.go) and used downstream for state-transition + key
	// flows, but not on the wire to /subscriptions.
	_ = walletID
	_ = paymentMethodID

	switch target {
	case scenario.StateTrialing:
		// V3 trial: server enters TRIALING when startTrial=true. trialEndsAt
		// is an Instant override; default would come from the offering's
		// trialDays config but we set 14d explicitly so the manifest
		// records a deterministic expiry.
		t := now.Add(14 * 24 * time.Hour)
		body.StartTrial = true
		body.TrialEndsAt = &t
	case scenario.StateExpired:
		// Make the subscription's end date in the past — the platform's
		// SubscriptionExpiryJob picks this up on its next 5-minute tick.
		// The transitionSubscription helper has a faster path that calls
		// /internal/subscriptions/{id}/expire to skip the wait; this
		// endDate-in-past is the fallback when that internal endpoint
		// isn't available.
		body.EndDate = now.Add(-1 * time.Hour).Format("2006-01-02")
	}

	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathSubscriptions)
	if err != nil {
		return subscriptionResponse{}, time.Time{}, err
	}
	var resp subscriptionResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: seedKey,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupSubscriptionByCustomerAndOffering(ctx, c, tenantID, customerID, offeringID)
			if lookupErr == nil && ok {
				return existing, time.Time{}, nil
			}
		}
		return subscriptionResponse{}, time.Time{}, fmt.Errorf("create sub (seedKey=%q target=%s): %w", seedKey, target, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-sub-" + seedKey
		resp.CustomerID = customerID
		resp.OfferingID = offeringID
		resp.Status = "ACTIVE"
		resp.CreatedAt = now
	}
	return resp, now, nil
}

// transitionSubscription drives a freshly-created (ACTIVE/TRIALING) subscription
// into its target end-state. Returns staleSince if the subscription was
// transitioned into a stale-key state (CANCELLED or EXPIRED).
//
// The user's prompt is explicit: cancel MUST go through the /cancel endpoint,
// not a status update. Only the cancel endpoint triggers Aforo's atomic
// key-revocation cascade.
func transitionSubscription(ctx context.Context, c *Client, tenantID, subID string, target scenario.SubscriptionState) (staleSince *time.Time, err error) {
	switch target {
	case scenario.StateActive, scenario.StateTrialing, scenario.StateCreated:
		// Nothing to do — these are the natural starting states.
		return nil, nil

	case scenario.StateCancelled:
		now := time.Now().UTC()
		cancelURL, urlErr := c.Target().Path(aforo.ServicePricing, fmt.Sprintf(aforo.PathSubscriptionCancel, subID))
		if urlErr != nil {
			return nil, urlErr
		}
		body := subscriptionCancelRequest{Reason: "loadgen-stale-key-test"}
		if err := c.Do(ctx, http.MethodPost, cancelURL, body, nil, RequestOptions{TenantID: tenantID}); err != nil {
			return nil, fmt.Errorf("cancel sub %s: %w", subID, err)
		}
		return &now, nil

	case scenario.StateExpired:
		// Try the internal expire endpoint first — this is the synchronous
		// path that returns immediately. If it's unavailable (404), the
		// subscription is already in EXPIRED via past end_date set at
		// creation, but the SubscriptionExpiryJob takes up to 5 minutes to
		// pick it up. We log and proceed.
		now := time.Now().UTC()
		expireURL, urlErr := c.Target().Path(aforo.ServicePricing, fmt.Sprintf(aforo.PathSubscriptionExpire, subID))
		if urlErr != nil {
			return nil, urlErr
		}
		err := c.Do(ctx, http.MethodPost, expireURL, nil, nil, RequestOptions{TenantID: tenantID})
		switch {
		case err == nil:
			return &now, nil
		case aforo.IsNotFound(err):
			// Internal endpoint missing → subscription already created with
			// past end_date, expiry-job will pick it up. Mark stale anyway —
			// the manifest documents the wait via stale_reason.
			return &now, nil
		default:
			return nil, fmt.Errorf("expire sub %s: %w", subID, err)
		}

	case scenario.StatePaused:
		pauseURL, urlErr := c.Target().Path(aforo.ServicePricing, fmt.Sprintf(aforo.PathSubscriptionPause, subID))
		if urlErr != nil {
			return nil, urlErr
		}
		if err := c.Do(ctx, http.MethodPost, pauseURL, nil, nil, RequestOptions{TenantID: tenantID}); err != nil {
			return nil, fmt.Errorf("pause sub %s: %w", subID, err)
		}
		return nil, nil

	case scenario.StatePastDue, scenario.StateSuspended, scenario.StateExpiringSoon:
		// These states are produced by the platform's payment/dunning logic.
		// Faking them via a direct status update would be a lie — Session 9
		// (payments) creates real PAST_DUE subscriptions by routing failed
		// charges through the dunning scheduler. For now, we leave the
		// subscription in ACTIVE and tag the manifest with the requested
		// state so Session 9 can take it from there.
		return nil, nil
	}
	return nil, nil
}

// lookupSubscriptionByCustomerAndOffering queries pricing-service's GET
// /api/v1/subscriptions with the server-side `?customerId=` filter (verified
// SubscriptionController.list accepts ?customerId= + ?status=) and filters
// client-side by exact `offeringId` match. The pair is unique per active
// subscription (a customer can hold at most one active subscription per
// offering in loadgen's seed scenarios).
func lookupSubscriptionByCustomerAndOffering(ctx context.Context, c *Client, tenantID, customerID, offeringID string) (subscriptionResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathSubscriptions)
	if err != nil {
		return subscriptionResponse{}, false, err
	}
	var subs []subscriptionResponse
	if _, err := listAllOptional(ctx, c, listURL, RequestOptions{
		TenantID: tenantID,
		Query:    map[string][]string{"customerId": {customerID}},
	}, &subs); err != nil {
		return subscriptionResponse{}, false, err
	}
	for _, s := range subs {
		if s.CustomerID == customerID && s.OfferingID == offeringID {
			return s, true, nil
		}
	}
	return subscriptionResponse{}, false, nil
}

// FetchSubscription returns the current state of a subscription. Sessions
// 4+ use this to read back invoice / dunning state during run-engine
// assertions; the seed harness itself doesn't call it but the symbol is
// exported so the run engine can reuse the typed transport.
func FetchSubscription(ctx context.Context, c *Client, tenantID, subID string) (SubscriptionStatus, error) {
	getURL, err := c.Target().Path(aforo.ServicePricing, fmt.Sprintf(aforo.PathSubscriptionByID, subID))
	if err != nil {
		return SubscriptionStatus{}, err
	}
	var resp subscriptionResponse
	if err := c.Do(ctx, http.MethodGet, getURL, nil, &resp, RequestOptions{TenantID: tenantID}); err != nil {
		return SubscriptionStatus{}, err
	}
	if c.DryRun() {
		resp.ID = subID
		resp.Status = "CANCELLED"
	}
	return SubscriptionStatus{ID: resp.ID, Status: resp.Status}, nil
}

// SubscriptionStatus is the snapshot returned by FetchSubscription. Exported
// so run-engine code in Sessions 4+ can read the value without re-importing
// internal DTOs.
//
// Drift-fix (2026-05-27): the public ExternalID field was removed because
// pricing-service's SubscriptionResponse has no externalId field — it always
// resolved to "". Callers that need cross-day idempotency should look up
// subscriptions by (customerId, offeringId) via the manifest, not by
// externalId via the API.
type SubscriptionStatus struct {
	ID     string
	Status string
}

// archiveSubscription cancels (= soft-archives) a subscription during clean.
func archiveSubscription(ctx context.Context, c *Client, tenantID, subID string) error {
	if subID == "" {
		return nil
	}
	cancelURL, err := c.Target().Path(aforo.ServicePricing, fmt.Sprintf(aforo.PathSubscriptionCancel, subID))
	if err != nil {
		return err
	}
	body := subscriptionCancelRequest{Reason: "loadgen-clean"}
	if err := c.Do(ctx, http.MethodPost, cancelURL, body, nil, RequestOptions{TenantID: tenantID}); err != nil {
		if aforo.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("cancel sub %s: %w", subID, err)
	}
	return nil
}
