package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// stripeTestTokens are deterministic Stripe test tokens that the platform
// accepts in test mode. We rotate them per-customer (mod len) so that a
// scenario covering 100 customers exercises a few distinct payment-method
// IDs without committing one-to-one mapping. Session 9 will replace this
// with full Stripe API integration.
var stripeTestTokens = []string{
	"pm_card_visa",       // 4242 — succeeds
	"pm_card_mastercard", // 5555 — succeeds
	"pm_card_amex",       // 3782 — succeeds
	"pm_card_visa_debit", // 4000 0566 5566 5556 — succeeds
}

// paymentMethodCreateRequest mirrors billing-service's CreatePaymentMethodRequest DTO.
// Field names MUST match the Java DTO exactly — `gatewayToken` + `methodType` are
// `@NotBlank` in CreatePaymentMethodRequest.java, so any rename here re-breaks 400.
// In test mode, gatewayToken is a Stripe pm_xxx token.
//
// CONVENTION (see CONVENTIONS.md): EVERY field on this struct maps to a
// real CreatePaymentMethodRequest column. Deterministic identity for
// cross-day lookup is `customerId` (one default method per customer,
// queried via dedicated /payment-methods/customer/{customerId} by
// lookupPaymentMethodByCustomer). Idempotency-Key is the loadgen-internal
// seedKey set by provisionPaymentMethod.
//
// Round-2 audit drop: `stripeMode` was on the previous request struct as
// `json:"stripeMode"`. Backend CreatePaymentMethodRequest.java has NO
// such column (verified — its fields are customerId, methodType,
// gatewayToken, gatewayCustomerId, displayName, lastFour, cardBrand,
// expiryMonth, expiryYear, setAsDefault). Test-vs-live mode is determined
// by the gateway token prefix (pm_card_visa vs pm_card_xxx_decline_card),
// not by an explicit field.
type paymentMethodCreateRequest struct {
	CustomerID   string `json:"customerId"`
	GatewayToken string `json:"gatewayToken"`
	MethodType   string `json:"methodType"`
}

// paymentMethodResponse mirrors the subset of billing-service's
// PaymentMethodResponse that the seed harness consumes.
//
// Drift-fix (2026-05-27):
//   - Removed `json:"externalId"` — billing-service has no such field on
//     the PaymentMethod entity or DTO (verified against
//     aforo-nextgen-billing-service/.../PaymentMethodResponse.java; fields
//     are id, customerId, methodType, displayName, lastFour, cardBrand,
//     gatewayType, isDefault, active, createdAt). The previous loadgen
//     response struct read `json:"externalId"` which always decoded to ""
//     and made every lookupPaymentMethodByExternalID call return
//     "not found".
//   - Added `customerId` + `gatewayType` for the deterministic identity
//     pair used by lookupPaymentMethodByCustomerAndToken. The Stripe
//     token itself isn't returned by the API (only lastFour + cardBrand)
//     so cross-day idempotency uses (customerId, lastFour, cardBrand)
//     to match the deterministic Stripe test tokens loadgen rotates
//     through.
type paymentMethodResponse struct {
	ID          string `json:"id"`
	CustomerID  string `json:"customerId"`
	MethodType  string `json:"methodType"`
	LastFour    string `json:"lastFour"`
	CardBrand   string `json:"cardBrand"`
	GatewayType string `json:"gatewayType"`
	Active      bool   `json:"active"`
}

// provisionPaymentMethod attaches a test-mode Stripe token as a payment
// method on a customer. POSTPAID and HYBRID subs require it; PREPAID-only
// customers don't (the wallet is the funding source).
//
// Idempotency strategy (drift-fix 2026-05-27):
//   - Within 24h: Idempotency-Key header on POST.
//   - Cross-day / DB-reset: lookupPaymentMethodByCustomer uses billing-
//     service's GET /api/v1/payment-methods/customer/{customerId}
//     dedicated subroute (verified PaymentMethodController exposes only
//     /customer/{customerId}; there's no list root). Returns the first
//     active method — loadgen rotates Stripe test tokens by seq, but only
//     the first call per customer matters for the seed flow.
//
// Parameters:
//   - seedKey: loadgen-internal opaque deterministic string sent as the
//     HTTP Idempotency-Key header. See CONVENTIONS.md.
func provisionPaymentMethod(ctx context.Context, c *Client, tenantID, seedKey, customerID string, seq int) (paymentMethodResponse, error) {
	if existing, ok, err := lookupPaymentMethodByCustomer(ctx, c, tenantID, customerID); err != nil {
		return paymentMethodResponse{}, fmt.Errorf("lookup payment method for customer %q: %w", customerID, err)
	} else if ok {
		return existing, nil
	}
	token := stripeTestTokens[seq%len(stripeTestTokens)]
	body := paymentMethodCreateRequest{
		CustomerID:   customerID,
		GatewayToken: token,
		MethodType:   "CARD",
	}
	createURL, err := c.Target().Path(aforo.ServiceBilling, aforo.PathPaymentMethods)
	if err != nil {
		return paymentMethodResponse{}, err
	}
	var resp paymentMethodResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: seedKey,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupPaymentMethodByCustomer(ctx, c, tenantID, customerID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return paymentMethodResponse{}, fmt.Errorf("create payment method (seedKey=%q): %w", seedKey, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-pm-" + seedKey
		resp.CustomerID = customerID
		resp.MethodType = "CARD"
		resp.Active = true
	}
	return resp, nil
}

// lookupPaymentMethodByCustomer uses billing-service's GET
// /api/v1/payment-methods/customer/{customerId} dedicated endpoint that
// returns the list of payment methods for a customer. Returns the first
// active one. ok=false means the customer has no active method yet.
func lookupPaymentMethodByCustomer(ctx context.Context, c *Client, tenantID, customerID string) (paymentMethodResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServiceBilling, aforo.PathPaymentMethodsByCustomer(customerID))
	if err != nil {
		return paymentMethodResponse{}, false, err
	}
	var methods []paymentMethodResponse
	if _, err := listAllOptional(ctx, c, listURL, RequestOptions{TenantID: tenantID}, &methods); err != nil {
		return paymentMethodResponse{}, false, err
	}
	for _, p := range methods {
		if p.Active {
			return p, true, nil
		}
	}
	return paymentMethodResponse{}, false, nil
}
