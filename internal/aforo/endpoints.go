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
	"strings"
)

// Service identifies one of Aforo's microservices. Each value maps to a base
// URL via Target.URLs.
type Service string

const (
	// ServiceOrganization owns tenants, internal admin endpoints, audit log.
	// Production: https://org.aforo.space — local: http://localhost:8086.
	ServiceOrganization Service = "organization"

	// ServiceCatalog owns products, billable units (metrics), and (in the
	// renamed billing-platform configuration) some billing-domain entities.
	// Production: https://catalog.aforo.space — local: http://localhost:8081.
	ServiceCatalog Service = "catalog"

	// ServicePricing owns rate plans, offerings, subscriptions, and API keys.
	// Production: https://pricing.aforo.space — local: http://localhost:8083.
	ServicePricing Service = "pricing"

	// ServiceCustomer owns customers, teams, agents.
	// Production: https://customer.aforo.space — local: http://localhost:8085.
	ServiceCustomer Service = "customer"

	// ServiceBilling owns wallets, escrow, payment methods, invoices.
	// Production: https://billing.aforo.space — local: http://localhost:8090.
	ServiceBilling Service = "billing"

	// ServiceUsageIngestor — used only by the integration sanity check
	// (a stale key is rejected with 401/403).
	// Production: https://usage-ingestor.aforo.space — local: http://localhost:8084.
	ServiceUsageIngestor Service = "usage-ingestor"
)

// AllServices is the canonical iteration order — used by --doctor probes.
var AllServices = []Service{
	ServiceOrganization, ServiceCatalog, ServicePricing,
	ServiceCustomer, ServiceBilling, ServiceUsageIngestor,
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
	},
}

// StagingTarget is a placeholder; concrete URLs will be set when staging
// infrastructure is provisioned. Today it mirrors prod.
var StagingTarget = Target{
	Name: "staging",
	URLs: map[Service]string{
		ServiceOrganization:  "https://org.aforo.space",
		ServiceCatalog:       "https://catalog.aforo.space",
		ServicePricing:       "https://pricing.aforo.space",
		ServiceCustomer:      "https://customer.aforo.space",
		ServiceBilling:       "https://billing.aforo.space",
		ServiceUsageIngestor: "https://usage-ingestor.aforo.space",
	},
}

// ProdTarget points at the public aforo.space URLs documented in CLAUDE.md.
// Production seeding is not the normal path — guarded behind --target=prod
// in seed.go. Most runs target local or a per-PR review env.
var ProdTarget = Target{
	Name: "prod",
	URLs: map[Service]string{
		ServiceOrganization:  "https://org.aforo.space",
		ServiceCatalog:       "https://catalog.aforo.space",
		ServicePricing:       "https://pricing.aforo.space",
		ServiceCustomer:      "https://customer.aforo.space",
		ServiceBilling:       "https://billing.aforo.space",
		ServiceUsageIngestor: "https://usage-ingestor.aforo.space",
	},
}

// PredefinedTargets is the lookup table used by ResolveTarget.
var PredefinedTargets = map[string]Target{
	LocalTarget.Name:   LocalTarget,
	StagingTarget.Name: StagingTarget,
	ProdTarget.Name:    ProdTarget,
}

// ResolveTarget returns the predefined target by name. If name parses as a
// URL, every service is pointed at that URL — useful for review environments
// where all services live behind one ingress (Kong, ALB).
func ResolveTarget(name string) (Target, error) {
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
		for _, svc := range AllServices {
			t.URLs[svc] = base
		}
		return t, nil
	}
	return Target{}, fmt.Errorf("unknown target %q (try local, staging, prod, or a full URL)", name)
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
	PathAPIKeys            = "/api/v1/api-keys"
	PathAPIKeyByID         = "/api/v1/api-keys/%s"
	PathAPIKeyRevoke       = "/api/v1/api-keys/%s/revoke"
	PathDiscounts          = "/api/v1/discounts"

	// billing-service / billing-platform (billing domain)
	PathWallets        = "/api/v1/wallets"
	PathWalletByID     = "/api/v1/wallets/%s"
	PathPaymentMethods = "/api/v1/payment-methods"

	// usage-ingestor (sanity check only)
	PathUsageIngest = "/v1/ingest"
)
