package seed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
)

// customerEmailFor returns a deterministic, RFC 5321-compliant email
// derived from a seedKey. The previous form `${seedKey}@loadgen.aforo.test`
// blew through the 64-char local-part limit for longer archetype names
// (e.g. seedKey `loadgen-customer-ci-billing-percentage-seed-2026-06-01-
// xxxxxx-1001` is 65 chars before @), which customer-service's
// @Email Bean Validation rejected with HTTP 400 fieldErrors.email
// "Email must be a valid email address". Drift-fix 2026-06-01.
//
// Format: `lg-{12 hex chars of sha256(seedKey)}@loadgen.aforo.test`.
// Local part = 3 ("lg-") + 12 (hash prefix) = 15 chars. Well under
// the 64-char RFC limit and uniqueness collision odds are negligible
// at loadgen's tenant count (12 hex chars = 48 bits = 2^48 namespace).
// Email is deterministic per seedKey so cross-day lookupCustomerByEmail
// still works as the cross-DB-reset identity key.
//
// We DO keep the seedKey on the Idempotency-Key header so 24h-window
// dedup still works against the same response. The seedKey just no
// longer leaks into the email's local part.
func customerEmailFor(seedKey string) string {
	sum := sha256.Sum256([]byte(seedKey))
	return "lg-" + hex.EncodeToString(sum[:6]) + "@loadgen.aforo.test"
}

// customerCreateRequest mirrors customer-service's CreateCustomerRequest.
//
// Field-name contract (verified against customer-service
// CreateCustomerRequest.java; enforced by internal/seed/contract_test.go):
//   - name — @NotBlank, @Size(max=255).
//   - email — @NotBlank, @Email.
//   - plan — @NotBlank, one of STANDARD|BUSINESS|ENTERPRISE.
//   - defaultCurrency (NOT "currency") — ISO 4217. The DTO field is
//     `defaultCurrency`; "currency" would be silently dropped.
//
// CONVENTION (see CONVENTIONS.md "Wire-format alignment"): EVERY field on
// this struct maps to a real CreateCustomerRequest.java column.
// Deterministic identity for cross-day lookup is `email`
// (lookupCustomerByEmail). Idempotency-Key is the loadgen-internal
// seedKey set by provisionCustomer.
type customerCreateRequest struct {
	Name            string `json:"name"`
	Email           string `json:"email"`
	Plan            string `json:"plan"`
	DefaultCurrency string `json:"defaultCurrency,omitempty"`
}

// customerResponse mirrors the subset of customer-service's CustomerResponse
// that the seed harness consumes.
//
// Drift-fix (2026-05-27): the response no longer reads `externalId` —
// customer-service has no such field on the entity or DTO (verified against
// aforo-nextgen-customer-service/.../CustomerResponse.java). The previous
// loadgen response struct read `json:"externalId"` which always decoded to
// "" and made every lookupCustomerByExternalID call return "not found",
// silently fanning out duplicate creates on cross-day reruns.
// Name + email are the deterministic identity keys (email is unique per
// tenant: `${externalID}@loadgen.aforo.test`) and drive lookupCustomerByEmail.
type customerResponse struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// provisionCustomer creates one customer for a tenant. Currency is recorded
// on the customer (so the offering can be currency-matched).
//
// Idempotency strategy (drift-fix 2026-05-27):
//   - Within 24h: Idempotency-Key header on POST is honored by customer-
//     service so re-sending the same request returns the same response.
//   - Cross-day / DB-reset: lookupCustomerByEmail runs first. customer-
//     service's GET /api/v1/customers exposes no server-side filter, so the
//     lookup pages client-side and matches by exact email (the deterministic
//     key derived from externalID).
//
// Parameters:
//   - seedKey: loadgen-internal opaque deterministic string sent as the
//     HTTP Idempotency-Key header. Also embedded in the customer's
//     email ({seedKey}@loadgen.aforo.test) so the email itself remains a
//     deterministic cross-day identity key (driven by lookupCustomerByEmail).
//     See CONVENTIONS.md for the seedKey + Idempotency-Key contract.
func provisionCustomer(ctx context.Context, c *Client, tenantID, seedKey, currency string, archetype string, seq int) (customerResponse, error) {
	email := customerEmailFor(seedKey)

	if existing, ok, err := lookupCustomerByEmail(ctx, c, tenantID, email); err != nil {
		return customerResponse{}, fmt.Errorf("lookup customer %q: %w", email, err)
	} else if ok {
		return existing, nil
	}
	// Name MUST NOT contain square brackets — catalog-service's
	// ValidBusinessName validator (mirrored on customer.name) rejects
	// anything outside [a-zA-Z0-9\s\-_.()].
	body := customerCreateRequest{
		Name:            fmt.Sprintf("Loadgen Customer %s %03d", archetype, seq),
		Email:           email,
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
		Idempotency: seedKey,
	})
	if err != nil {
		if aforo.IsConflict(err) {
			existing, ok, lookupErr := lookupCustomerByEmail(ctx, c, tenantID, email)
			if lookupErr == nil && ok {
				return existing, nil
			}
		}
		return customerResponse{}, fmt.Errorf("create customer (seedKey=%q): %w", seedKey, err)
	}
	if c.DryRun() {
		resp.ID = "dryrun-customer-" + seedKey
		resp.Email = email
		resp.Name = body.Name
	}
	return resp, nil
}

// lookupCustomerByEmail pages through customer-service's GET /api/v1/customers
// and filters client-side by exact email match. customer-service's list
// endpoint has no server-side email filter (verified against
// CustomerController.listCustomers — accepts only Pageable). The seed
// harness's deterministic email scheme (`<externalId>@loadgen.aforo.test`)
// guarantees uniqueness, so an exact match is the right identity check.
func lookupCustomerByEmail(ctx context.Context, c *Client, tenantID, email string) (customerResponse, bool, error) {
	listURL, err := c.Target().Path(aforo.ServiceCustomer, aforo.PathCustomers)
	if err != nil {
		return customerResponse{}, false, err
	}
	var customers []customerResponse
	if _, err := listAllOptional(ctx, c, listURL, RequestOptions{TenantID: tenantID}, &customers); err != nil {
		return customerResponse{}, false, err
	}
	for _, x := range customers {
		if x.Email == email {
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
