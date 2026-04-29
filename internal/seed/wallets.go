package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// walletCreateRequest mirrors billing-platform's wallet create body. The
// initial balance funds escrow holds for PREPAID/HYBRID subscriptions.
type walletCreateRequest struct {
	ExternalID     string  `json:"externalId"`
	CustomerID     string  `json:"customerId"`
	OrganizationID string  `json:"organizationId,omitempty"`
	Currency       string  `json:"currency"`
	InitialBalance float64 `json:"initialBalance"`
}

type walletResponse struct {
	ID         string  `json:"id"`
	ExternalID string  `json:"externalId"`
	Balance    float64 `json:"balance"`
}

// provisionWallet creates a wallet for PREPAID/HYBRID customers. Wallets are
// created BEFORE the subscription so the platform's escrow logic finds an
// existing wallet at subscription create time.
//
// One wallet per (customer, currency) pair — billing-platform indexes the
// wallets table on (organizationId, customerId, currency).
func provisionWallet(ctx context.Context, c *Client, tenantID, externalID, customerID, currency string, initialBalance float64) (walletResponse, error) {
	if existing, ok, err := lookupWalletByExternalID(ctx, c, tenantID, externalID); err != nil {
		return walletResponse{}, fmt.Errorf("lookup wallet %q: %w", externalID, err)
	} else if ok {
		return existing, nil
	}
	body := walletCreateRequest{
		ExternalID:     externalID,
		CustomerID:     customerID,
		Currency:       currency,
		InitialBalance: initialBalance,
	}
	createURL, err := c.Target().Path(aforo.ServiceBilling, aforo.PathWallets)
	if err != nil {
		return walletResponse{}, err
	}
	var resp walletResponse
	err = c.Do(ctx, http.MethodPost, createURL, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: externalID,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupWalletByExternalID(ctx, c, tenantID, externalID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return walletResponse{}, fmt.Errorf("create wallet %q: %w", externalID, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-wallet-" + externalID
		resp.ExternalID = externalID
		resp.Balance = initialBalance
	}
	return resp, nil
}

func lookupWalletByExternalID(ctx context.Context, c *Client, tenantID, externalID string) (walletResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServiceBilling, aforo.PathWallets)
	if err != nil {
		return walletResponse{}, false, err
	}
	var page struct {
		Data []walletResponse `json:"data"`
	}
	err = c.Do(ctx, http.MethodGet, listURL, nil, &page, RequestOptions{
		TenantID: tenantID,
		Query:    map[string][]string{"externalId": {externalID}},
	})
	if err != nil {
		if aforo.IsNotFound(err) {
			return walletResponse{}, false, nil
		}
		return walletResponse{}, false, err
	}
	for _, w := range page.Data {
		if w.ExternalID == externalID {
			return w, true, nil
		}
	}
	return walletResponse{}, false, nil
}

func archiveWallet(ctx context.Context, c *Client, tenantID, walletID string) error {
	if walletID == "" {
		return nil
	}
	delURL, err := c.Target().Path(aforo.ServiceBilling, fmt.Sprintf(aforo.PathWalletByID, walletID))
	if err != nil {
		return err
	}
	if err := c.Do(ctx, http.MethodDelete, delURL, nil, nil, RequestOptions{TenantID: tenantID}); err != nil {
		if aforo.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("archive wallet %s: %w", walletID, err)
	}
	return nil
}
