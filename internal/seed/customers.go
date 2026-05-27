package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// customerCreateRequest mirrors customer-service's CreateCustomerRequest.
//
// Field-name contract (verified against customer-service
// CreateCustomerRequest.java):
//   - name — @NotBlank, @Size(max=255).
//   - email — @NotBlank, @Email.
//   - plan — @NotBlank, one of STANDARD|BUSINESS|ENTERPRISE. Loadgen
//     previously omitted this and would have hit 400 "Plan is required"
//     once the product-creation blocker is past.
//   - defaultCurrency (NOT "currency") — ISO 4217. The DTO field is
//     `defaultCurrency`; the previous "currency" key was silently dropped.
//   - externalId is NOT a DTO field — silently dropped server-side.
//     Loadgen keeps it on the body for forward-compat.
type customerCreateRequest struct {
	ExternalID      string `json:"externalId,omitempty"`
	Name            string `json:"name"`
	Email           string `json:"email"`
	Plan            string `json:"plan"`
	DefaultCurrency string `json:"defaultCurrency,omitempty"`
}

type customerResponse struct {
	ID         string `json:"id"`
	ExternalID string `json:"externalId"`
}

// provisionCustomer creates one customer for a tenant. Currency is recorded
// on the customer (so the offering can be currency-matched).
func provisionCustomer(ctx context.Context, c *Client, tenantID, externalID, currency string, archetype string, seq int) (customerResponse, error) {
	if existing, ok, err := lookupCustomerByExternalID(ctx, c, tenantID, externalID); err != nil {
		return customerResponse{}, fmt.Errorf("lookup customer %q: %w", externalID, err)
	} else if ok {
		return existing, nil
	}
	// Name MUST NOT contain square brackets — same forward-compat
	// reasoning as products (see products.go).
	body := customerCreateRequest{
		ExternalID:      externalID,
		Name:            fmt.Sprintf("Loadgen Customer %s %03d", archetype, seq),
		Email:           fmt.Sprintf("%s@loadgen.aforo.test", externalID),
		Plan:            "STANDARD",
		DefaultCurrency: currency,
	}
	createURL, err := c.Target().Path(aforo.ServiceCustomer, aforo.PathCustomers)
	if err != nil {
		return customerResponse{}, err
	}
	var resp customerResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: externalID,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupCustomerByExternalID(ctx, c, tenantID, externalID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return customerResponse{}, fmt.Errorf("create customer %q: %w", externalID, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-customer-" + externalID
		resp.ExternalID = externalID
	}
	return resp, nil
}

func lookupCustomerByExternalID(ctx context.Context, c *Client, tenantID, externalID string) (customerResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServiceCustomer, aforo.PathCustomers)
	if err != nil {
		return customerResponse{}, false, err
	}
	var page struct {
		Data []customerResponse `json:"data"`
	}
	err = c.Do(ctx, http.MethodGet, listURL, nil, &page, RequestOptions{
		TenantID: tenantID,
		Query:    map[string][]string{"externalId": {externalID}},
	})
	if err != nil {
		if aforo.IsNotFound(err) {
			return customerResponse{}, false, nil
		}
		return customerResponse{}, false, err
	}
	for _, x := range page.Data {
		if x.ExternalID == externalID {
			return x, true, nil
		}
	}
	return customerResponse{}, false, nil
}

// archiveCustomer soft-archives a customer (closes their account) during clean.
func archiveCustomer(ctx context.Context, c *Client, tenantID, customerID string) error {
	if customerID == "" {
		return nil
	}
	delURL, err := c.Target().Path(aforo.ServiceCustomer, fmt.Sprintf(aforo.PathCustomerByID, customerID))
	if err != nil {
		return err
	}
	if err := c.Do(ctx, http.MethodDelete, delURL, nil, nil, RequestOptions{TenantID: tenantID}); err != nil {
		if aforo.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("archive customer %s: %w", customerID, err)
	}
	return nil
}
