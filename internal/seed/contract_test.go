package seed

// contract_test.go enforces the loadgen ↔ backend wire-format contract by
// reflecting over the seed package's request / response structs and
// asserting every json:"..." tag is declared in the corresponding backend
// OpenAPI schema (committed snapshots under openapi/<service>.json).
//
// Why this lives in package seed (vs internal/contract): the structs being
// verified are unexported. Putting the test here gives reflect.TypeOf
// access without an awkward "expose-for-test" indirection layer.
//
// See internal/contract/doc.go for what this catches + what it does NOT
// catch + how to refresh snapshots after a backend rename.

import (
	"reflect"
	"sort"
	"testing"

	"github.com/aforoai/aforo-nextgen-loadgen/internal/contract"
)

// contractEntries is the authoritative loadgen-struct ↔ backend-schema map.
//
// Adding a new entry:
//   1. Add a row here citing the Go struct type, backend service name
//      (matches openapi/<svc>.json), and OpenAPI schema name (the Java
//      DTO class name as it appears in components.schemas).
//   2. Run `go test ./internal/seed/... -run TestBackendContract`. Failures
//      are field tags that don't appear in the schema. Either fix the
//      loadgen struct, or refresh the snapshot via `make sync-openapi` if
//      backend changed.
//
// Direction (Request vs Response) drives expectation severity:
//   - Request: server silently drops unknown fields (Jackson default).
//     Allowed to carry phantom fields tagged via AllowPhantomRequest
//     when documented (e.g. externalId on create payloads).
//   - Response: phantom fields decode to zero values silently — a real
//     bug. PerfectMatch is the default for responses.
func contractEntries() []contract.Entry {
	return []contract.Entry{
		// ---- tenants (organization-service /api/v1/internal/tenants) -----
		// Note: tenants ride on org-service's "LoadgenTenantResponse" which
		// (uniquely) DOES carry externalId; the contract matches cleanly.
		{
			Service: "organization", SchemaName: "LoadgenTenantResponse",
			StructType: reflect.TypeOf(tenantResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
			// LoadgenTenantResponse is on /internal/admin paths exempt from
			// Springdoc by convention. Snapshot won't carry it until backend
			// opts the path in. Skip until then.
			Skip: "LoadgenTenantResponse lives under /internal/admin — not exposed by Springdoc today",
		},

		// ---- catalog: products (catalog-service /api/v1/products) --------
		{
			Service: "catalog", SchemaName: "CreateProductRequest",
			StructType: reflect.TypeOf(productCreateRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "catalog", SchemaName: "ProductResponse",
			StructType: reflect.TypeOf(productResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},

		// ---- catalog: metrics (catalog-service /api/v1/metrics) ----------
		{
			Service: "catalog", SchemaName: "BulkCreateMetricRequest",
			StructType: reflect.TypeOf(bulkSeedRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "catalog", SchemaName: "MetricResponse",
			StructType: reflect.TypeOf(metricResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},
		{
			Service: "catalog", SchemaName: "MetricTemplateResponse",
			StructType: reflect.TypeOf(metricTemplateResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},

		// ---- customer-service /api/v1/customers --------------------------
		{
			Service: "customer", SchemaName: "CreateCustomerRequest",
			StructType: reflect.TypeOf(customerCreateRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "customer", SchemaName: "CustomerResponse",
			StructType: reflect.TypeOf(customerResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},

		// ---- pricing: subscriptions --------------------------------------
		{
			Service: "pricing", SchemaName: "CreateSubscriptionRequest",
			StructType: reflect.TypeOf(subscriptionCreateRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "pricing", SchemaName: "SubscriptionResponse",
			StructType: reflect.TypeOf(subscriptionResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},
		{
			Service: "pricing", SchemaName: "CancelSubscriptionRequest",
			StructType: reflect.TypeOf(subscriptionCancelRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},

		// ---- pricing: rate plans -----------------------------------------
		{
			Service: "pricing", SchemaName: "CreateRatePlanRequest",
			StructType: reflect.TypeOf(ratePlanCreateRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "pricing", SchemaName: "MetricConfigRequest",
			StructType: reflect.TypeOf(metricConfigRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "pricing", SchemaName: "MetricTierRequest",
			StructType: reflect.TypeOf(rateTierRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "pricing", SchemaName: "RatePlanResponse",
			StructType: reflect.TypeOf(ratePlanResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},

		// ---- pricing: offerings ------------------------------------------
		{
			Service: "pricing", SchemaName: "CreateOfferingRequest",
			StructType: reflect.TypeOf(offeringCreateRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "pricing", SchemaName: "OfferingResponse",
			StructType: reflect.TypeOf(offeringResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},

		// ---- pricing: api-keys -------------------------------------------
		{
			Service: "pricing", SchemaName: "CreateApiKeyRequest",
			StructType: reflect.TypeOf(apiKeyCreateRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "pricing", SchemaName: "ApiKeyResponse",
			StructType: reflect.TypeOf(apiKeyResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},

		// ---- billing: wallets --------------------------------------------
		{
			Service: "billing", SchemaName: "CreateWalletRequest",
			StructType: reflect.TypeOf(walletCreateRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "billing", SchemaName: "WalletResponse",
			StructType: reflect.TypeOf(walletResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},

		// ---- billing: payment methods ------------------------------------
		{
			Service: "billing", SchemaName: "CreatePaymentMethodRequest",
			StructType: reflect.TypeOf(paymentMethodCreateRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
		{
			Service: "billing", SchemaName: "PaymentMethodResponse",
			StructType: reflect.TypeOf(paymentMethodResponse{}),
			Direction:  contract.DirResponse, Expectation: contract.PerfectMatch,
		},

		// ---- pricing: discounts ------------------------------------------
		{
			Service: "pricing", SchemaName: "ApplyDiscountRequest",
			StructType: reflect.TypeOf(discountApplyRequest{}),
			Direction:  contract.DirRequest, Expectation: contract.PerfectMatch,
		},
	}
}

// TestBackendContract is the loadgen ↔ backend wire-format gate. It runs on
// every PR + push via CI. Failure means a backend rename (or a loadgen
// rename) drifted the contract and one of them needs to follow.
//
// To debug a failure:
//
//  1. Read the failure message — it tells you exactly which JSON tag on
//     which Go struct doesn't appear in which OpenAPI schema.
//  2. Look at openapi/<service>.json to confirm whether the schema
//     legitimately doesn't carry that field (loadgen is wrong) or whether
//     the snapshot is stale (backend changed; refresh via
//     `make sync-openapi`).
//  3. Update the loadgen struct OR refresh the snapshot — never both in
//     the same commit unless documented why.
func TestBackendContract(t *testing.T) {
	repoRoot, err := contract.FindRepoRoot()
	if err != nil {
		t.Fatalf("locate repo root: %v", err)
	}

	// Group entries by service so we load each spec once.
	entries := contractEntries()
	byService := map[string][]contract.Entry{}
	for _, e := range entries {
		byService[e.Service] = append(byService[e.Service], e)
	}

	var anyFailure bool
	for service, group := range byService {
		t.Run(service, func(t *testing.T) {
			spec, err := contract.LoadSpec(repoRoot, service)
			if err != nil {
				// Snapshot missing — fail gracefully if no snapshot exists
				// for the service, since first-run after wiring the test
				// shouldn't hard-fail before `make sync-openapi` runs.
				// Skip + warn so devs see the gap.
				t.Skipf("openapi snapshot for %q not available (%v) — run `make sync-openapi`", service, err)
				return
			}
			// Sort entries by schema name for stable test output.
			sort.SliceStable(group, func(i, j int) bool {
				return group[i].SchemaName < group[j].SchemaName
			})
			for _, e := range group {
				e := e
				t.Run(e.SchemaName, func(t *testing.T) {
					if e.Skip != "" {
						t.Skipf("%s", e.Skip)
						return
					}
					result := contract.Verify(spec, e)
					if !result.OK {
						anyFailure = true
						for _, m := range result.Mismatches {
							t.Errorf("[%s/%s direction=%s] %s",
								service, e.SchemaName, e.Direction, m)
						}
						t.Errorf("FIX:\n  - if the loadgen field name is wrong: edit the struct in internal/seed/.\n  - if backend renamed the field: run `make sync-openapi` to refresh openapi/%s.json (matching ci backend snapshot).", service)
					}
					if len(result.SkippedFields) > 0 {
						t.Logf("phantom request fields (silently dropped by backend, kept for forward-compat): %v",
							result.SkippedFields)
					}
				})
			}
		})
	}
	if anyFailure {
		t.Logf("contract drift detected. See per-subtest errors above. Workflow: 1) decide whether loadgen or backend is the source of truth; 2) update the other side; 3) refresh the snapshot via `make sync-openapi`; 4) commit both changes together.")
	}
}
