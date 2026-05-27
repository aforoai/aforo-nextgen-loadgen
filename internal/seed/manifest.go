package seed

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/scenario"
)

// ManifestVersion is the on-disk schema version. v2 = Session 3 (archetype
// awareness, stale key tracking, summary). v1 was the Session-1 placeholder.
const ManifestVersion = 2

// Manifest is the durable record of everything a seed run created. The run
// engine (Session 4+) reads this to know which tenants, customers, and keys
// to drive traffic against, and which subscriptions are stale (revoked keys
// that should produce 401/403 when the engine sends events).
type Manifest struct {
	ManifestVersion int              `json:"manifest_version"`
	RunID           string           `json:"run_id"`
	Target          string           `json:"target"`
	Scenario        string           `json:"scenario"`
	CreatedAt       time.Time        `json:"created_at"`
	Tenants         []ManifestTenant `json:"tenants"`
	Summary         ManifestSummary  `json:"summary"`

	mu sync.Mutex // guards concurrent appends from per-archetype workers
}

// ManifestTenant is a single tenant slot — one archetype, one billing config,
// the products/rate plans/offerings created for it, and the customers (each
// of whom may have multiple subscriptions across different states).
//
// IDENTITY FIELDS (see CONVENTIONS.md "Manifest schema"):
//   - tenant_id: backend-assigned primary key (`id` on LoadgenTenantResponse).
//   - external_id: the ONE legitimate externalId on the platform —
//     organization-service's /internal/admin endpoint genuinely stores
//     and round-trips this column. Kept named external_id because that's
//     the backend column name; renaming to seed_key here would lose the
//     direct backend ↔ manifest mapping.
type ManifestTenant struct {
	TenantID     string                `json:"tenant_id"`
	ExternalID   string                `json:"external_id"`
	Archetype    string                `json:"archetype"`
	PricingModel scenario.PricingModel `json:"pricing_model"`
	BillingMode  scenario.BillingMode  `json:"billing_mode"`
	Products     []ManifestProduct     `json:"products"`
	RatePlans    []ManifestRatePlan    `json:"rate_plans"`
	Offerings    []ManifestOffering    `json:"offerings"`
	Customers    []ManifestCustomer    `json:"customers"`
}

// ManifestProduct mirrors a created product (catalog-service).
//
// IDENTITY FIELDS:
//   - product_id: backend `id`.
//   - name: backend `name` — the deterministic cross-day identity key for
//     lookupProductByName.
//   - seed_key: loadgen-internal Idempotency-Key value (= what we sent on
//     the HTTP Idempotency-Key header). Useful for grep-debugging across
//     loadgen logs but NOT a backend column on products.
type ManifestProduct struct {
	ProductID   string               `json:"product_id"`
	Name        string               `json:"name"`
	SeedKey     string               `json:"seed_key"`
	ProductType scenario.ProductType `json:"product_type"`
	MetricIDs   []string             `json:"metric_ids,omitempty"`
}

// ManifestRatePlan mirrors a created rate plan.
//
// IDENTITY FIELDS:
//   - rate_plan_id: backend `id`.
//   - name: backend `name` — deterministic key for lookupRatePlanByName.
//   - seed_key: loadgen-internal Idempotency-Key value.
type ManifestRatePlan struct {
	RatePlanID string         `json:"rate_plan_id"`
	Name       string         `json:"name"`
	SeedKey    string         `json:"seed_key"`
	Version    int            `json:"version"`
	Config     map[string]any `json:"config"`
}

// ManifestOffering mirrors a created offering.
//
// IDENTITY FIELDS:
//   - offering_id: backend `id`.
//   - code: backend `code` (per-tenant UNIQUE) — deterministic key for
//     lookupOfferingByCode. Loadgen reuses seedKey as the code value.
//   - seed_key: loadgen-internal Idempotency-Key value (= code).
type ManifestOffering struct {
	OfferingID  string               `json:"offering_id"`
	Code        string               `json:"code"`
	SeedKey     string               `json:"seed_key"`
	BillingMode scenario.BillingMode `json:"billing_mode"`
	Currency    string               `json:"currency"`
}

// ManifestCustomer is one customer, with the subscriptions (and their keys)
// the harness created for them.
//
// IDENTITY FIELDS:
//   - customer_id: backend `id`.
//   - email: backend `email` — deterministic key for lookupCustomerByEmail
//     (`{seed_key}@loadgen.aforo.test`).
//   - seed_key: loadgen-internal Idempotency-Key value.
type ManifestCustomer struct {
	CustomerID    string                 `json:"customer_id"`
	Email         string                 `json:"email"`
	SeedKey       string                 `json:"seed_key"`
	Currency      string                 `json:"currency"`
	Discount      *ManifestDiscount      `json:"discount,omitempty"`
	Subscriptions []ManifestSubscription `json:"subscriptions"`
}

// ManifestDiscount is one applied discount. Either type+value (PERCENTAGE,
// FIXED_AMOUNT) or both for stacked discounts (the platform supports the
// former; we never stack today).
type ManifestDiscount struct {
	Type  string  `json:"type"`
	Value float64 `json:"value"`
}

// ManifestSubscription is one subscription row. Stale=true means the
// subscription is in CANCELLED or EXPIRED — keys here are revoked and
// negative-path traffic uses them to test stale-key rejection.
//
// IDENTITY FIELDS:
//   - subscription_id: backend `id`.
//   - customer_id + offering_id: the deterministic identity pair for
//     lookupSubscriptionByCustomerAndOffering (backend has no
//     externalId column on subscriptions).
//   - seed_key: loadgen-internal Idempotency-Key value.
type ManifestSubscription struct {
	SubscriptionID         string                     `json:"subscription_id"`
	CustomerID             string                     `json:"customer_id"`
	OfferingID             string                     `json:"offering_id"`
	SeedKey                string                     `json:"seed_key"`
	Status                 scenario.SubscriptionState `json:"status"`
	Stale                  bool                       `json:"stale"`
	StaleReason            string                     `json:"stale_reason,omitempty"`
	StaleSince             *time.Time                 `json:"stale_since,omitempty"`
	WalletID               string                     `json:"wallet_id,omitempty"`
	PaymentMethodID        string                     `json:"payment_method_id,omitempty"`
	ExpectedBillingFormula string                     `json:"expected_billing_formula"`
	APIKeys                []ManifestAPIKey           `json:"api_keys"`
}

// ManifestAPIKey is one credential. Secret is the bearer token (BEARER_TOKEN)
// or client_secret (CLIENT_CREDENTIALS) — kept in the manifest because the
// run engine and the integration sanity check both need it.
type ManifestAPIKey struct {
	KeyID          string     `json:"key_id"`
	Secret         string     `json:"secret"`
	ClientID       string     `json:"client_id,omitempty"`
	CredentialType string     `json:"credential_type"`
	Revoked        bool       `json:"revoked"`
	RevokedAt      *time.Time `json:"revoked_at,omitempty"`
}

// ManifestSummary is a compact roll-up of the manifest — printed at the end
// of a seed run and committed alongside the manifest for offline inspection.
type ManifestSummary struct {
	TotalTenants   int            `json:"total_tenants"`
	ByArchetype    map[string]int `json:"by_archetype"`
	ByPricingModel map[string]int `json:"by_pricing_model"`
	ByBillingMode  map[string]int `json:"by_billing_mode"`
	ByCurrency     map[string]int `json:"by_currency"`
	StaleKeysCount int            `json:"stale_keys_count"`
	TotalCustomers int            `json:"total_customers"`
	TotalSubs      int            `json:"total_subs"`
}

// NewManifest constructs an empty manifest with the run header populated.
func NewManifest(runID, target, scenarioName string, now time.Time) *Manifest {
	return &Manifest{
		ManifestVersion: ManifestVersion,
		RunID:           runID,
		Target:          target,
		Scenario:        scenarioName,
		CreatedAt:       now,
		Tenants:         make([]ManifestTenant, 0, 16),
	}
}

// AppendTenant is concurrency-safe — multiple archetype workers may seed
// simultaneously. Index ordering of tenants in the final manifest is not
// deterministic across runs (acceptable; the downstream tools key by ID).
func (m *Manifest) AppendTenant(t ManifestTenant) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Tenants = append(m.Tenants, t)
}

// Finalize sorts tenants by external_id (deterministic for golden tests) and
// computes the summary. Call once after all workers have returned.
func (m *Manifest) Finalize() {
	m.mu.Lock()
	defer m.mu.Unlock()
	sort.Slice(m.Tenants, func(i, j int) bool {
		return m.Tenants[i].ExternalID < m.Tenants[j].ExternalID
	})
	m.Summary = computeSummary(m.Tenants)
}

func computeSummary(tenants []ManifestTenant) ManifestSummary {
	s := ManifestSummary{
		TotalTenants:   len(tenants),
		ByArchetype:    map[string]int{},
		ByPricingModel: map[string]int{},
		ByBillingMode:  map[string]int{},
		ByCurrency:     map[string]int{},
	}
	for _, t := range tenants {
		s.ByArchetype[t.Archetype]++
		s.ByPricingModel[string(t.PricingModel)]++
		s.ByBillingMode[string(t.BillingMode)]++
		s.TotalCustomers += len(t.Customers)
		for _, c := range t.Customers {
			s.ByCurrency[c.Currency]++
			s.TotalSubs += len(c.Subscriptions)
			for _, sub := range c.Subscriptions {
				for _, k := range sub.APIKeys {
					if k.Revoked {
						s.StaleKeysCount++
					}
				}
			}
		}
	}
	return s
}

// Save writes the manifest to disk as pretty-printed JSON. Returns the absolute
// number of bytes written for log output.
func (m *Manifest) Save(path string) (int, error) {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("marshal manifest: %w", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return 0, fmt.Errorf("write %s: %w", path, err)
	}
	return len(data), nil
}

// LoadManifest reads a manifest JSON file. Used by --clean to surgically
// archive a prior run's entities and by Session 4+ run engine entry.
func LoadManifest(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return LoadManifestFromBytes(data)
}

// LoadManifestFromBytes parses a manifest from an in-memory byte slice.
// Session 11 — used by the multi-machine worker to deserialize the
// manifest the coordinator dispatched in the Assignment payload.
func LoadManifestFromBytes(data []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse manifest: %w", err)
	}
	if m.ManifestVersion != ManifestVersion {
		return nil, fmt.Errorf("manifest has version %d, expected %d (rerun seed to upgrade)",
			m.ManifestVersion, ManifestVersion)
	}
	return &m, nil
}

// MarshalManifest serializes the manifest to JSON. Symmetric helper used
// by the Session 11 coordinator to build the bytes it dispatches in
// each Assignment.
func (m *Manifest) MarshalManifest() ([]byte, error) {
	return json.MarshalIndent(m, "", "  ")
}
