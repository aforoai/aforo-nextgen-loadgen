package seed

import (
	"context"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// walletCreateRequest mirrors billing-service's wallet create body. The
// initial balance funds escrow holds for PREPAID/HYBRID subscriptions.
//
// CONVENTION (see CONVENTIONS.md): EVERY field on this struct maps to a
// real CreateWalletRequest column. Deterministic identity for cross-day
// lookup is `customerId` (one company wallet per customer, queried via
// dedicated GET /wallets/by-customer/{customerId} by
// lookupWalletByCustomer). Idempotency-Key is the loadgen-internal
// seedKey set by provisionWallet.
//
// Round-2 audit drop: `organizationId` was previously declared as
// `json:"organizationId,omitempty"`. Backend CreateWalletRequest.java has
// NO such column (verified — its fields are customerId, currency,
// initialBalance, autoTopupEnabled, autoTopupAmount, autoTopupThreshold,
// lowBalanceThreshold). The field was also never set anywhere in
// loadgen — pure dead code. Removed.
type walletCreateRequest struct {
	CustomerID     string  `json:"customerId"`
	Currency       string  `json:"currency"`
	InitialBalance float64 `json:"initialBalance"`
}

// walletResponse mirrors the subset of billing-service's WalletResponse
// that the seed harness consumes.
//
// Drift-fix (2026-05-27): the response no longer reads `externalId` —
// billing-service has no such field on the entity or DTO (verified against
// aforo-nextgen-billing-service/.../WalletResponse.java). The previous
// loadgen response struct read `json:"externalId"` which always decoded to
// "" and made every lookupWalletByExternalID call return "not found".
// `customerId` is the deterministic identity key — billing-service enforces
// one company wallet per customer via the `/wallets/by-customer/{customerId}`
// endpoint, which is what lookupWalletByCustomer uses.
type walletResponse struct {
	ID         string  `json:"id"`
	CustomerID string  `json:"customerId"`
	Currency   string  `json:"currency"`
	Balance    float64 `json:"balance"`
}

// provisionWallet creates a wallet for PREPAID/HYBRID customers. Wallets are
// created BEFORE the subscription so the platform's escrow logic finds an
// existing wallet at subscription create time.
//
// One wallet per (customer, currency) pair — billing-service indexes the
// wallets table on (organizationId, customerId, currency).
//
// Idempotency strategy (drift-fix 2026-05-27):
//   - Within 24h: Idempotency-Key header on POST.
//   - Cross-day / DB-reset: lookupWalletByCustomer runs first and uses
//     billing-service's GET /api/v1/wallets/by-customer/{customerId}
//     dedicated single-result endpoint (verified WalletController exposes
//     it). No client-side paging needed.
//
// Parameters:
//   - seedKey: loadgen-internal opaque deterministic string sent as the
//     HTTP Idempotency-Key header. See CONVENTIONS.md.
func provisionWallet(ctx context.Context, c *Client, tenantID, seedKey, customerID, currency string, initialBalance float64) (walletResponse, error) {
	if existing, ok, err := lookupWalletByCustomer(ctx, c, tenantID, customerID); err != nil {
		return walletResponse{}, fmt.Errorf("lookup wallet for customer %q: %w", customerID, err)
	} else if ok && existing.Currency == currency {
		// Existing wallet matches the requested currency — reuse it.
		return existing, nil
	}
	body := walletCreateRequest{
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
		Idempotency: seedKey,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupWalletByCustomer(ctx, c, tenantID, customerID)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return walletResponse{}, fmt.Errorf("create wallet (seedKey=%q): %w", seedKey, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-wallet-" + seedKey
		resp.CustomerID = customerID
		resp.Currency = currency
		resp.Balance = initialBalance
	}
	return resp, nil
}

// lookupWalletByCustomer uses billing-service's GET
// /api/v1/wallets/by-customer/{customerId} dedicated endpoint which returns
// the company wallet (with department wallet info embedded) for the
// customer. Returns ok=false on 404 (no wallet yet) so the caller's
// create-on-miss branch fires.
func lookupWalletByCustomer(ctx context.Context, c *Client, tenantID, customerID string) (walletResponse, bool, error) {
	getURL, err := c.Target().Path(aforo.ServiceBilling, aforo.PathWalletByCustomer(customerID))
	if err != nil {
		return walletResponse{}, false, err
	}
	var resp walletResponse
	if err := c.Do(ctx, http.MethodGet, getURL, nil, &resp, RequestOptions{TenantID: tenantID}); err != nil {
		if aforo.IsNotFound(err) {
			return walletResponse{}, false, nil
		}
		return walletResponse{}, false, err
	}
	return resp, true, nil
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
