package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// offeringCreateRequest mirrors pricing-service's OfferingCreateRequest.
// Escrow config fields are populated only for PREPAID/HYBRID modes.
type offeringCreateRequest struct {
	ExternalID       string  `json:"externalId"`
	Name             string  `json:"name"`
	Description      string  `json:"description,omitempty"`
	RatePlanID       string  `json:"ratePlanId"`
	BillingMode      string  `json:"billingMode"`
	Currency         string  `json:"currency"`
	TrialDays        int     `json:"trialDays,omitempty"`
	WalletInitialUSD float64 `json:"walletInitialBalanceUsd,omitempty"`
}

type offeringResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
}

// provisionOffering wraps a rate plan in a billing-mode offering. The
// validator already ensures wallet_initial_balance_usd > 0 for PREPAID/HYBRID
// archetypes — we surface it on the offering so the platform's escrow logic
// has the budget value at subscription create.
func provisionOffering(ctx context.Context, c *Client, tenantID, externalID string, a scenario.TenantArchetype, ratePlanID, currency string) (offeringResponse, error) {
	if existing, ok, err := lookupOfferingByExternalID(ctx, c, tenantID, externalID); err != nil {
		return offeringResponse{}, fmt.Errorf("lookup offering %q: %w", externalID, err)
	} else if ok {
		return existing, nil
	}
	body := offeringCreateRequest{
		ExternalID:  externalID,
		Name:        fmt.Sprintf("Loadgen Offering [%s]", a.Name),
		Description: fmt.Sprintf("auto-provisioned for archetype=%s", a.Name),
		RatePlanID:  ratePlanID,
		BillingMode: string(a.BillingMode),
		Currency:    currency,
		TrialDays:   a.RateConfig.TrialDays,
	}
	if a.BillingMode == scenario.BillingPrepaid || a.BillingMode == scenario.BillingHybrid {
		body.WalletInitialUSD = a.RateConfig.WalletInitialBalanceUSD
	}
	createURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathOfferings)
	if err != nil {
		return offeringResponse{}, err
	}
	var resp offeringResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: externalID,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupOfferingByExternalID(ctx, c, tenantID, externalID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return offeringResponse{}, fmt.Errorf("create offering %q: %w", externalID, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-offering-" + externalID
		resp.ExternalID = externalID
	}
	return resp, nil
}

func lookupOfferingByExternalID(ctx context.Context, c *Client, tenantID, externalID string) (offeringResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServicePricing, aforo.PathOfferings)
	if err != nil {
		return offeringResponse{}, false, err
	}
	var page struct {
		Data []offeringResponse `json:"data"`
	}
	err = c.Do(ctx, http.MethodGet, listURL, nil, &page, RequestOptions{
		TenantID: tenantID,
		Query:    map[string][]string{"externalId": {externalID}},
	})
	if err != nil {
		if aforo.IsNotFound(err) {
			return offeringResponse{}, false, nil
		}
		return offeringResponse{}, false, err
	}
	for _, o := range page.Data {
		if o.ExternalID == externalID {
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
