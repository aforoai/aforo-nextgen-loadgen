package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// discountApplyRequest mirrors pricing-service's discount apply body.
// Discounts attach to a subscription (not a customer) so per-subscription
// granularity matches the manifest layout.
type discountApplyRequest struct {
	ExternalID     string  `json:"externalId"`
	SubscriptionID string  `json:"subscriptionId"`
	Type           string  `json:"type"`
	Value          float64 `json:"value"`
}

type discountResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
}

// applyDiscount attaches the discount to the subscription. The Manifest's
// ManifestDiscount type is the source of truth — this is just the side-effect
// to make the discount real on the platform.
func applyDiscount(ctx context.Context, c *Client, tenantID, externalID, subscriptionID string, d *ManifestDiscount) error {
	if d == nil {
		return nil
	}
	body := discountApplyRequest{
		ExternalID:     externalID,
		SubscriptionID: subscriptionID,
		Type:           d.Type,
		Value:          d.Value,
	}
	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathDiscounts)
	if err != nil {
		return err
	}
	var resp discountResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: externalID,
	})
	if err != nil {
		// 409 → discount already applied (idempotent re-run).
		if aforo.IsConflict(err) {
			return nil
		}
		return fmt.Errorf("apply discount %q: %w", externalID, err)
	}
	return nil
}
