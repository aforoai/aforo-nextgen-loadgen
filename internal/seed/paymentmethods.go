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

// paymentMethodCreateRequest mirrors billing-platform's payment-method DTO.
// In test mode, paymentMethodToken is a Stripe pm_xxx token.
type paymentMethodCreateRequest struct {
	ExternalID         string `json:"externalId"`
	CustomerID         string `json:"customerId"`
	PaymentMethodToken string `json:"paymentMethodToken"`
	Type               string `json:"type"`
	StripeMode         string `json:"stripeMode"`
}

type paymentMethodResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
}

// provisionPaymentMethod attaches a test-mode Stripe token as a payment
// method on a customer. POSTPAID and HYBRID subs require it; PREPAID-only
// customers don't (the wallet is the funding source).
func provisionPaymentMethod(ctx context.Context, c *Client, tenantID, externalID, customerID string, seq int) (paymentMethodResponse, error) {
	if existing, ok, err := lookupPaymentMethodByExternalID(ctx, c, tenantID, externalID); err != nil {
		return paymentMethodResponse{}, fmt.Errorf("lookup payment method %q: %w", externalID, err)
	} else if ok {
		return existing, nil
	}
	token := stripeTestTokens[seq%len(stripeTestTokens)]
	body := paymentMethodCreateRequest{
		ExternalID:         externalID,
		CustomerID:         customerID,
		PaymentMethodToken: token,
		Type:               "CARD",
		StripeMode:         "test",
	}
	createURL, err := c.Target().Path(aforo.ServiceBilling, aforo.PathPaymentMethods)
	if err != nil {
		return paymentMethodResponse{}, err
	}
	var resp paymentMethodResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: externalID,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupPaymentMethodByExternalID(ctx, c, tenantID, externalID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return paymentMethodResponse{}, fmt.Errorf("create payment method %q: %w", externalID, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-pm-" + externalID
		resp.ExternalID = externalID
	}
	return resp, nil
}

func lookupPaymentMethodByExternalID(ctx context.Context, c *Client, tenantID, externalID string) (paymentMethodResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServiceBilling, aforo.PathPaymentMethods)
	if err != nil {
		return paymentMethodResponse{}, false, err
	}
	var page struct {
		Data []paymentMethodResponse `json:"data"`
	}
	err = c.Do(ctx, http.MethodGet, listURL, nil, &page, RequestOptions{
		TenantID: tenantID,
		Query:    map[string][]string{"externalId": {externalID}},
	})
	if err != nil {
		if aforo.IsNotFound(err) {
			return paymentMethodResponse{}, false, nil
		}
		return paymentMethodResponse{}, false, err
	}
	for _, p := range page.Data {
		if p.ExternalID == externalID {
			return p, true, nil
		}
	}
	return paymentMethodResponse{}, false, nil
}
