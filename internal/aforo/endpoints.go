// Package aforo contains the typed service map and shared error types for
// every Aforo REST surface the loadgen tool talks to.
//
// Two reasons it exists as its own package:
//
//  1. Sessions 4-12 will reuse the same Service enum and BaseURLs map. Keeping
//     them out of internal/seed/ avoids an import cycle when the run engine
//     and the seed harness both need to address services.
//  2. Errors returned from API calls (NotFound, Conflict, RateLimited)
//     are inspected by callers to decide whether to retry, treat-as-existing,
//     or bail. Defining them here keeps that contract close to the URL map.
package aforo

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// Service identifies one of Aforo's microservices. Each value maps to a base
// URL via Target.URLs.
type Service string

const (
	// ServiceOrganization owns tenants, internal admin endpoints, audit log.
	// Production: https://org.aforo.ai — local: http://localhost:8086.
	ServiceOrganization Service = "organization"

	// ServiceCatalog owns products, billable units (metrics), and (in the
	// renamed billing-platform configuration) some billing-domain entities.
	// Production: https://catalog.aforo.ai — local: http://localhost:8081.
	ServiceCatalog Service = "catalog"

	// ServicePricing owns rate plans, offerings, subscriptions, and API keys.
	// Production: https://pricing.aforo.ai — local: http://localhost:8083.
	ServicePricing Service = "pricing"

	// ServiceCustomer owns customers, teams, agents.
	// Production: https://customer.aforo.ai — local: http://localhost:8085.
	ServiceCustomer Service = "customer"

	// ServiceBilling owns wallets, escrow, payment methods, invoices.
	// Production: https://billing.aforo.ai — local: http://localhost:8090.
	ServiceBilling Service = "billing"

	// ServiceUsageIngestor — used only by the integration sanity check
	// (a stale key is rejected with 401/403).
	// Production: https://usage-ingestor.aforo.ai — local: http://localhost:8084.
	ServiceUsageIngestor Service = "usage-ingestor"

	// ServiceAnalytics owns ClickHouse-backed analytics, event log, system
	// health, and uptime monitoring. doctor probes /actuator/health.
	// Production: https://analytics.aforo.ai — local: http://localhost:8088.
	ServiceAnalytics Service = "analytics"

	// ServiceStorefront owns the customer portal BFF, headless API, and
	// storefront config publishing. doctor probes /actuator/health.
	// Production: https://storefront.aforo.ai — local: http://localhost:8089.
	ServiceStorefront Service = "storefront"

	// ServiceAIService owns Anthropic-backed content / section / component
	// generation + storefront AI runtime. doctor probes a lightweight HTTP
	// endpoint (no actuator on this service today).
	// Local: http://localhost:8091.
	ServiceAIService Service = "ai-service"
)

// AllServices is the iteration order used by the seed harness — only
// services it actively writes to. Doctor uses AllProbeServices instead.
var AllServices = []Service{
	ServiceOrganization, ServiceCatalog, ServicePricing,
	ServiceCustomer, ServiceBilling, ServiceUsageIngestor,
}

// AllProbeServices is the canonical health-check iteration order used by
// the doctor subcommand. Strictly a superset of AllServices: doctor
// verifies reachability of every service the e2e flow touches, including
// read-only consumers (analytics, storefront, ai-service) that the seed
// harness never writes to.
var AllProbeServices = []Service{
	ServiceOrganization, ServiceCatalog, ServicePricing,
	ServiceCustomer, ServiceUsageIngestor, ServiceAnalytics,
	ServiceBilling, ServiceStorefront, ServiceAIService,
}

// Target identifies a deployment environment plus its per-service URL map.
// Concrete targets are pre-defined (local, staging, prod) but callers can
// supply a custom Target via NewCustomTarget for ad-hoc environments.
type Target struct {
	Name string
	URLs map[Service]string
}

// String reports the target name.
func (t Target) String() string { return t.Name }

// URL returns the base URL for the given service in this target.
// Returns an error if the target wasn't configured with that service.
func (t Target) URL(svc Service) (string, error) {
	u, ok := t.URLs[svc]
	if !ok {
		return "", fmt.Errorf("target %q has no URL for service %q", t.Name, svc)
	}
	return strings.TrimRight(u, "/"), nil
}

// LocalTarget points every service at its dev port on localhost. Matches the
// docker-compose ports listed in CLAUDE.md.
var LocalTarget = Target{
	Name: "local",
	URLs: map[Service]string{
		ServiceOrganization:  "http://localhost:8086",
		ServiceCatalog:       "http://localhost:8081",
		ServicePricing:       "http://localhost:8083",
		ServiceCustomer:      "http://localhost:8085",
		ServiceBilling:       "http://localhost:8090",
		ServiceUsageIngestor: "http://localhost:8084",
		ServiceAnalytics:     "http://localhost:8088",
		ServiceStorefront:    "http://localhost:8089",
		ServiceAIService:     "http://localhost:8091",
	},
}

// StagingTarget is a placeholder; concrete URLs will be set when staging
// infrastructure is provisioned. Today it mirrors prod.
var StagingTarget = Target{
	Name: "staging",
	URLs: map[Service]string{
		ServiceOrganization:  "https://org.aforo.ai",
		ServiceCatalog:       "https://catalog.aforo.ai",
		ServicePricing:       "https://pricing.aforo.ai",
		ServiceCustomer:      "https://customer.aforo.ai",
		ServiceBilling:       "https://billing.aforo.ai",
		ServiceUsageIngestor: "https://usage-ingestor.aforo.ai",
		ServiceAnalytics:     "https://analytics.aforo.ai",
		ServiceStorefront:    "https://storefront.aforo.ai",
		// ai-service does not have a public hostname today; staging runs
		// targeting external URLs skip the ai-service probe.
		ServiceAIService: "",
	},
}

// ProdTarget points at the public aforo.ai URLs documented in CLAUDE.md.
// Production seeding is not the normal path — guarded behind --target=prod
// in seed.go. Most runs target local or a per-PR review env.
var ProdTarget = Target{
	Name: "prod",
	URLs: map[Service]string{
		ServiceOrganization:  "https://org.aforo.ai",
		ServiceCatalog:       "https://catalog.aforo.ai",
		ServicePricing:       "https://pricing.aforo.ai",
		ServiceCustomer:      "https://customer.aforo.ai",
		ServiceBilling:       "https://billing.aforo.ai",
		ServiceUsageIngestor: "https://usage-ingestor.aforo.ai",
		ServiceAnalytics:     "https://analytics.aforo.ai",
		ServiceStorefront:    "https://storefront.aforo.ai",
		ServiceAIService:     "",
	},
}

// PredefinedTargets is the lookup table used by ResolveTarget.
//
// Note: "ci" is intentionally absent here because its URL map is computed at
// resolve time from environment variables. ResolveTarget short-circuits on
// the literal string "ci" before consulting this map.
var PredefinedTargets = map[string]Target{
	LocalTarget.Name:   LocalTarget,
	StagingTarget.Name: StagingTarget,
	ProdTarget.Name:    ProdTarget,
}

// CITargetName is the literal flag value `--target ci`. Kept as a const so
// callers (run, e2e, doctor, etc.) can compare without typing the string.
const CITargetName = "ci"

// CITarget builds the ci target by reading environment variables. URL
// resolution order, highest priority first:
//
//  1. AFORO_CI_BASE_URL — single ingress, fans every service to one URL.
//     This is the common case for per-PR review environments behind Kong
//     or an ALB.
//  2. AFORO_CI_<SERVICE>_URL — per-service override (e.g.
//     AFORO_CI_USAGE_INGESTOR_URL=https://my-pr-123.aforo.dev). Lets a CI
//     run pin a single service while leaving the rest at staging.
//  3. The staging URL — falls through unchanged for any service the env
//     does not override.
//
// The function never returns an error; if the env is empty the result
// is functionally identical to StagingTarget but renamed "ci" so logs,
// run manifests, and the doctor output reflect the actual flag the
// operator supplied.
//
// Why this is a builder, not a package-level var: env vars must be read
// at call time (CI workflows export them just before invoking loadgen),
// not at process start when the var would be initialized.
func CITarget() Target {
	t := Target{Name: CITargetName, URLs: map[Service]string{}}

	if base := strings.TrimSpace(os.Getenv("AFORO_CI_BASE_URL")); base != "" {
		base = strings.TrimRight(base, "/")
		for _, svc := range AllProbeServices {
			t.URLs[svc] = base
		}
		return t
	}

	for svc, url := range StagingTarget.URLs {
		envKey := "AFORO_CI_" + ciEnvSafeServiceName(svc) + "_URL"
		if override := strings.TrimSpace(os.Getenv(envKey)); override != "" {
			t.URLs[svc] = strings.TrimRight(override, "/")
			continue
		}
		t.URLs[svc] = url
	}
	return t
}

// ciEnvSafeServiceName converts a Service ("usage-ingestor", "ai-service") to
// the env-var-safe form ("USAGE_INGESTOR", "AI_SERVICE") used by the ci
// target's per-service overrides.
func ciEnvSafeServiceName(s Service) string {
	return strings.ToUpper(strings.ReplaceAll(string(s), "-", "_"))
}

// ResolveTarget returns the predefined target by name, or constructs a
// CI target from env vars when name == "ci". If name parses as a URL,
// every service is pointed at that URL — useful for review environments
// where all services live behind one ingress (Kong, ALB).
func ResolveTarget(name string) (Target, error) {
	if name == CITargetName {
		return CITarget(), nil
	}
	if t, ok := PredefinedTargets[name]; ok {
		return t, nil
	}
	if strings.Contains(name, "://") {
		u, err := url.Parse(name)
		if err != nil {
			return Target{}, fmt.Errorf("--target %q is not a valid URL: %w", name, err)
		}
		base := u.Scheme + "://" + u.Host
		t := Target{Name: name, URLs: map[Service]string{}}
		// Custom URL targets fan every probe service to the same ingress
		// (Kong, ALB, review env). Doctor will consequently probe the
		// same /actuator/health on the gateway 9 times — that's fine for
		// reachability; per-service health distinctions are only
		// meaningful in the multi-host predefined targets.
		for _, svc := range AllProbeServices {
			t.URLs[svc] = base
		}
		return t, nil
	}
	return Target{}, fmt.Errorf("unknown target %q (try local, staging, prod, ci, or a full URL)", name)
}

// Path joins a service base URL with a request path. Returns an absolute URL.
func (t Target) Path(svc Service, p string) (string, error) {
	base, err := t.URL(svc)
	if err != nil {
		return "", err
	}
	return base + ensureLeadingSlash(p), nil
}

func ensureLeadingSlash(p string) string {
	if strings.HasPrefix(p, "/") {
		return p
	}
	return "/" + p
}

// Endpoint paths used by the seed harness. Centralized so a typo in a path
// surfaces at compile time rather than as a 404 mid-run.
//
// Paths are intentionally relative — combine with Target.Path(svc, path).
const (
	// organization-service
	PathInternalTenants = "/api/v1/internal/tenants"
	PathInternalTenant  = "/api/v1/internal/tenants/%s" // %s = tenant id

	// catalog-service / billing-platform (catalog domain)
	PathProducts        = "/api/v1/products"
	PathProductByID     = "/api/v1/products/%s"
	PathMetrics         = "/api/v1/metrics"
	PathMetricsBulk     = "/api/v1/metrics/bulk"
	PathMetricsTemplate = "/api/v1/metrics/templates/%s" // %s = product type

	// customer-service
	PathCustomers    = "/api/v1/customers"
	PathCustomerByID = "/api/v1/customers/%s"

	// pricing-service
	PathRatePlans          = "/api/v1/rate-plans"
	PathRatePlanByID       = "/api/v1/rate-plans/%s"
	PathOfferings          = "/api/v1/offerings"
	PathOfferingByID       = "/api/v1/offerings/%s"
	PathSubscriptions      = "/api/v1/subscriptions"
	PathSubscriptionByID   = "/api/v1/subscriptions/%s"
	PathSubscriptionCancel = "/api/v1/subscriptions/%s/cancel"
	PathSubscriptionPause  = "/api/v1/subscriptions/%s/pause"
	PathSubscriptionExpire = "/api/v1/internal/subscriptions/%s/expire" // internal-only test hook
	// Session 6 — lifecycle transitions (pricing-service v3 endpoints).
	PathSubscriptionUpgrade           = "/api/v1/subscriptions/%s/upgrade"
	PathSubscriptionDowngrade         = "/api/v1/subscriptions/%s/downgrade"
	PathSubscriptionResume            = "/api/v1/subscriptions/%s/resume"
	PathSubscriptionConvertTrial      = "/api/v1/subscriptions/%s/convert-trial"
	PathSubscriptionRetryPayment      = "/api/v1/subscriptions/%s/retry-payment"
	PathSubscriptionMigrateProration  = "/api/v1/subscriptions/%s/migrate-with-proration"
	PathSubscriptionPhases            = "/api/v1/subscriptions/%s/phases" // audit trail
	PathInternalSubscriptionPastDue   = "/internal/v1/subscriptions/past-due"
	PathInternalSubscriptionDunningUp = "/internal/v1/subscriptions/%s/dunning-update"

	PathAPIKeys      = "/api/v1/api-keys"
	PathAPIKeyByID   = "/api/v1/api-keys/%s"
	PathAPIKeyRevoke = "/api/v1/api-keys/%s/revoke"
	PathDiscounts    = "/api/v1/discounts"

	// billing-service / billing-platform (billing domain)
	PathWallets        = "/api/v1/wallets"
	PathWalletByID     = "/api/v1/wallets/%s"
	PathPaymentMethods = "/api/v1/payment-methods"
	// Session 6 — bill run trigger for Check 11 concurrency probe.
	PathBillRuns = "/api/v1/bill-runs"

	// usage-ingestor (sanity check only)
	PathUsageIngest = "/v1/ingest"
)

// PathWalletByCustomer is the URL for billing-service's
// GET /api/v1/wallets/by-customer/{customerId} dedicated endpoint that
// returns the single company wallet (with department wallet info embedded)
// for a customer. Used by loadgen's lookup-before-create idempotency path
// in place of the previous broken `?externalId=` filter on the list root.
//
// Kept as a helper function rather than a fmt-style constant because the
// path segment is interpolated (mirrors how a fmt.Sprintf pattern would
// behave) without giving up compile-time call-site discoverability via
// `find references`.
func PathWalletByCustomer(customerID string) string {
	return "/api/v1/wallets/by-customer/" + customerID
}

// PathPaymentMethodsByCustomer is the URL for billing-service's
// GET /api/v1/payment-methods/customer/{customerId} dedicated endpoint that
// returns the list of payment methods for a customer. Used in place of the
// previous broken `?externalId=` filter on the list root, which doesn't
// exist server-side (the list root is not exposed at all; only the
// per-customer subroute is).
func PathPaymentMethodsByCustomer(customerID string) string {
	return "/api/v1/payment-methods/customer/" + customerID
}
