package seed

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/aforo"
	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// billing_setup.go provisions the two backend prerequisites that every
// payment-method create requires on billing-service:
//
//  1. A default billing entity for the tenant
//     (POST /internal/v1/billing-entities/provision-default).
//     Backend's gateway-create path 404s with
//     "Gateway config creation requires a default billing entity" without
//     this. organization-service's TenantProvisioningService.provision()
//     calls this in normal user flows; loadgen bypasses that flow by
//     calling LoadgenInternalTenantController directly, so we have to
//     replicate the missing step ourselves.
//
//  2. A primary payment gateway for the tenant
//     (POST /api/v1/payment-gateways).
//     Backend's payment-method create path 400s with
//     "No primary payment gateway configured" without this. The gateway
//     row is tenant-scoped, so loadgen-created tenants don't inherit any
//     pre-seeded staging fixture — we have to register one per tenant.
//
// Drift-fix 2026-06-01: surfaced when sanity-check.sh ran against AWS
// staging — every POSTPAID/HYBRID archetype died at the payment-method
// step because neither prereq was in place. PREPAID archetypes skip
// gateway provisioning since they use wallets, not cards.

// Stripe credential env vars. Loadgen does NOT bake test keys into the
// binary — operator supplies them per-environment. Documented in
// scripts/sanity-check.sh and CONVENTIONS.md. The intentional default
// when env vars are missing is to fail at provision-time with an
// actionable error rather than to skip silently (skipping would let the
// downstream payment-method create still 400 with a confusing message).
const (
	envStripeAPIKey        = "AFORO_LOADGEN_STRIPE_API_KEY"
	envStripePublicKey     = "AFORO_LOADGEN_STRIPE_PUBLIC_KEY"
	envStripeWebhookSecret = "AFORO_LOADGEN_STRIPE_WEBHOOK_SECRET"
)

// billingEntityRequest mirrors billing-service's ProvisionDefaultEntityRequest.
// Address fields cap at the @Size limits enforced server-side.
type billingEntityRequest struct {
	TenantID     string            `json:"tenantId"`
	DisplayName  string            `json:"displayName"`
	BaseCurrency string            `json:"baseCurrency"`
	CountryCode  string            `json:"countryCode"`
	Address      billingEntityAddr `json:"address"`
}

type billingEntityAddr struct {
	Line1      string `json:"line1"`
	City       string `json:"city"`
	Region     string `json:"region,omitempty"`
	PostalCode string `json:"postalCode,omitempty"`
	Country    string `json:"country"`
}

type billingEntityResponse struct {
	EntityID  string `json:"entityId"`
	TenantID  string `json:"tenantId"`
	IsDefault bool   `json:"isDefault"`
}

// provisionDefaultBillingEntity ensures the tenant has a default
// billing entity. Idempotent: backend's provisionDefaultForTenant
// returns the existing row if one is already present (controller
// javadoc confirms this).
func provisionDefaultBillingEntity(ctx context.Context, c *Client, tenantID string) (billingEntityResponse, error) {
	body := billingEntityRequest{
		TenantID:     tenantID,
		DisplayName:  "Loadgen Test Entity " + tenantID,
		BaseCurrency: "USD",
		CountryCode:  "US",
		Address: billingEntityAddr{
			Line1:      "1 Loadgen Way",
			City:       "San Francisco",
			Region:     "CA",
			PostalCode: "94105",
			Country:    "US",
		},
	}

	url, err := c.Target().Path(aforo.ServiceBilling, aforo.PathInternalBillingEntitiesProvisionDefault)
	if err != nil {
		return billingEntityResponse{}, err
	}

	var resp billingEntityResponse
	if err := c.Do(ctx, http.MethodPost, url, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: "loadgen-billing-entity-" + tenantID,
	}); err != nil {
		return billingEntityResponse{}, fmt.Errorf("provision default billing entity for %s: %w", tenantID, err)
	}

	if c.DryRun() {
		resp.EntityID = "dryrun-billing-entity-" + tenantID
		resp.TenantID = tenantID
		resp.IsDefault = true
	}
	return resp, nil
}

// paymentGatewayRequest mirrors billing-service's CreatePaymentGatewayRequest.
type paymentGatewayRequest struct {
	GatewayType         string `json:"gatewayType"`
	DisplayName         string `json:"displayName"`
	APIKey              string `json:"apiKey"`
	PublicKey           string `json:"publicKey,omitempty"`
	WebhookSecret       string `json:"webhookSecret,omitempty"`
	Primary             bool   `json:"primary"`
	SandboxMode         bool   `json:"sandboxMode"`
	SupportedCurrencies string `json:"supportedCurrencies,omitempty"`
}

type paymentGatewayResponse struct {
	ID          string `json:"id"`
	GatewayType string `json:"gatewayType"`
	Primary     bool   `json:"primary"`
	Active      bool   `json:"active"`
}

// archetypeNeedsPaymentGateway reports whether a billing mode requires a
// primary payment gateway to be configured. POSTPAID and HYBRID both
// route at least some charges to invoice→card; PREPAID is wallet-only
// and skips this requirement.
func archetypeNeedsPaymentGateway(mode scenario.BillingMode) bool {
	return mode == scenario.BillingPostpaid || mode == scenario.BillingHybrid
}

// provisionPaymentGatewayIfNeeded registers a primary Stripe-test gateway
// for the tenant when the archetype's billing mode requires one. Reads
// credentials from AFORO_LOADGEN_STRIPE_* env vars — fails fast with an
// actionable error if they're missing, since the alternative is letting
// the downstream payment-method create 400 with a less informative
// "No primary payment gateway configured" body.
//
// Idempotency: a fresh tenant has no gateways, so the POST will create.
// If loadgen ever runs --clean-then-rerun against the same tenant (it
// doesn't today; --clean archives the whole tenant), a 409 would be
// possible and we'd handle it the same way as the rest of the entity
// chain. Skipping that complexity now — fresh tenant per run.
func provisionPaymentGatewayIfNeeded(ctx context.Context, c *Client, tenantID string, mode scenario.BillingMode) error {
	if !archetypeNeedsPaymentGateway(mode) {
		return nil
	}

	apiKey := strings.TrimSpace(os.Getenv(envStripeAPIKey))
	publicKey := strings.TrimSpace(os.Getenv(envStripePublicKey))
	webhookSecret := strings.TrimSpace(os.Getenv(envStripeWebhookSecret))

	// Dry-run mode never actually sends the gateway POST, so we don't
	// need real credentials. Substitute a recognizable placeholder so
	// the recorded request body is valid JSON for offline inspection,
	// and so tests that exercise the dry-run path don't need to set up
	// env vars.
	if c.DryRun() && apiKey == "" {
		apiKey = "sk_test_dryrun_placeholder"
		publicKey = "pk_test_dryrun_placeholder"
		webhookSecret = "whsec_dryrun_placeholder"
	}

	if apiKey == "" {
		return missingStripeCredsError(mode, envStripeAPIKey)
	}
	// publicKey + webhookSecret are required by some downstream flows
	// (3DS confirmation, webhook signature verification) but not by the
	// payment-method create itself. We pass them through when supplied
	// but don't hard-fail if they're missing — the user can iterate.

	body := paymentGatewayRequest{
		GatewayType:         "STRIPE",
		DisplayName:         "Loadgen Stripe Test - " + tenantID,
		APIKey:              apiKey,
		PublicKey:           publicKey,
		WebhookSecret:       webhookSecret,
		Primary:             true,
		SandboxMode:         true,
		SupportedCurrencies: "USD",
	}

	url, err := c.Target().Path(aforo.ServiceBilling, aforo.PathPaymentGateways)
	if err != nil {
		return err
	}

	var resp paymentGatewayResponse
	if err := c.Do(ctx, http.MethodPost, url, body, &resp, RequestOptions{
		TenantID:    tenantID,
		Idempotency: "loadgen-gateway-" + tenantID,
	}); err != nil {
		return fmt.Errorf("provision payment gateway for %s: %w", tenantID, err)
	}
	return nil
}

// missingStripeCredsError produces an actionable error rather than the
// useless "No primary payment gateway configured" downstream 400. The
// message lists the env vars the operator needs to export, mirroring the
// pattern in scripts/sanity-check.sh's prereq gate.
func missingStripeCredsError(mode scenario.BillingMode, missingVar string) error {
	return fmt.Errorf(
		"%s archetype needs a payment gateway but %s is not set — "+
			"export AFORO_LOADGEN_STRIPE_API_KEY (and optionally "+
			"_PUBLIC_KEY + _WEBHOOK_SECRET) before running, or pick a "+
			"PREPAID-only scenario",
		mode, missingVar,
	)
}

