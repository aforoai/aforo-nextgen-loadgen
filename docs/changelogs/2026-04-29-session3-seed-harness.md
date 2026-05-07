# 2026-04-29 — Session 3: seed harness (per-archetype provisioning)

## What shipped

`aforo-loadgen seed --scenario <path-or-name>` now provisions one tenant per
archetype slot end-to-end via Aforo's REST APIs:

- Tenant (organization-service)
- Products + billable units (catalog-service / billing-platform)
- Rate plans (pricing-service) — full V3 shape with productIds[] + metricConfigs[]
- Offerings (pricing-service) — per-currency, with escrow config for PREPAID/HYBRID
- Customers (customer-service)
- Wallets (billing-service / billing-platform) for PREPAID/HYBRID
- Payment methods (billing-service) — Stripe test tokens
- Subscriptions (pricing-service) — distributed across the archetype's
  subscription_state_mix, then transitioned via `/cancel`, `/pause`, or
  `/internal/expire` to land in CANCELLED / PAUSED / EXPIRED states
- Discounts (pricing-service) — `pct_<N>` and `fixed_<N>` labels
- API keys (pricing-service) — `BEARER_TOKEN` for API/AGENTIC_API,
  `CLIENT_CREDENTIALS` for AI_AGENT/MCP_SERVER

Output is a `manifest.json` (schema v2) that records every entity ID, the
exact billing formula expected per subscription, and — for CANCELLED /
EXPIRED subs — `stale=true`, `stale_since`, and `revoked=true` on every
api-key.

The harness passes the dry-run stress test:
`./bin/aforo-loadgen seed --scenario matrix-billing --dry-run` produces a
90-tenant, 1620-customer, 1620-subscription, 36-stale-key manifest covering
all 18 (pricing × billing) combinations + 12 currency/state/discount
variants. Zero errors.

## CLI

```
aforo-loadgen seed
  --scenario <path-or-name>     scenario YAML path or built-in name
  --target <env>                local | staging | prod | <full URL>
  --out <manifest.json>         output path (default manifest.json)
  --dry-run                     print intended calls; never send
  --clean                       archive everything in --out manifest, exit
  --clean-from <path>           alternative manifest path for --clean
  --archetypes-only <list>      comma-separated subset of archetypes
  --concurrency <int>           archetype-worker concurrency (default 4)
  --max-http-concurrency <int>  in-flight HTTP cap (default 50)
  --min-interval-ms <int>       inter-request floor (default 200)
  --token-env <name>            env var holding admin token (default AFORO_ADMIN_TOKEN)
```

`--scenario` accepts both filesystem paths AND built-in names from the
embedded catalog (`ci-smoke`, `walk-realistic-50t`, `matrix-billing`, etc).

## Files added

### `internal/aforo/` — endpoints + errors

- `endpoints.go` — `Service` enum (6 services), `Target` map (local /
  staging / prod / custom URL), `ResolveTarget()`, `Target.Path(svc, p)`,
  17 endpoint-path constants
- `errors.go` — `APIError{Status, Method, URL, Body, UnderlyingErr}` plus
  `IsNotFound`, `IsConflict`, `IsUnauthorized`, `IsRetryable` helpers and
  `ErrAuthMissing`
- `endpoints_test.go` — target resolution, URL composition,
  error-classification matrix

### `internal/seed/` — orchestrator + per-entity provisioners

- `client.go` — HTTP client with auth, idempotency-key, retry on 5xx /
  408 / 429 (exponential backoff capped at 8s, no retry on 4xx),
  rate-limited single-token-bucket refill, semaphore-bounded concurrency,
  dry-run record-and-replay support
- `manifest.go` — `Manifest{ManifestVersion, RunID, Target, Scenario,
  CreatedAt, Tenants[], Summary}`, concurrency-safe `AppendTenant`,
  deterministic `Finalize` (sorts by external_id, computes summary),
  Save/Load
- `distribution.go` — weighted draws (`weightedDraw`,
  `distributeStates`, `distributeCurrencies`, `drawDiscount`,
  `parseDiscountLabel`)
- `archetypes.go` — `AllocateTenants` (largest-residual rounding so
  weights sum exactly to N), `FilterArchetypes` (`--archetypes-only`),
  `planArchetype`, `expectedBillingFormula` (one-line pricing formula
  per pricing model, recorded in manifest)
- `tenants.go`, `products.go`, `metrics.go`, `rateplans.go`,
  `offerings.go`, `customers.go`, `subscriptions.go`, `wallets.go`,
  `paymentmethods.go`, `discounts.go`, `apikeys.go` — one provisioner
  per entity. All idempotent (GET-by-externalId then POST). Every
  externalId follows `loadgen-{kind}-{archetype}-{run_id}-{seq}`
- `stale_keys.go` — `verifyStaleKeys` re-fetches each api-key after
  CANCELLED/EXPIRED transition and updates the manifest. Returns an
  error if a CANCELLED sub's key is still active (platform regression
  signal)
- `clean.go` — surgical `--clean` archives entities in inverse-create
  order (subs cancel → offerings → rate plans → products → wallets →
  customers → tenants). Best-effort; per-entity errors recorded in
  `CleanResult.Errors`
- `seeder.go` — top-level orchestrator. Per-archetype-tenant goroutines
  bounded by `Concurrency`. Manifest finalized + saved even on partial
  failure. Deterministic externalIds + per-tenant RNG seeded from
  `(scenario.seed XOR fnv(archetype) XOR seq)` so reruns produce
  identical state distribution
- `client_test.go` — retry, no-retry-on-4xx, header propagation, rate
  limiting, dry-run records
- `distribution_test.go` — proportional rounding, deterministic ordering,
  stale-state-presence guard
- `archetypes_test.go` — largest-residual on 30 archetypes (matches
  matrix-billing shape), filter, exact 12-USD/8-EUR currency split,
  exact state distribution
- `manifest_test.go` — round-trip, summary computation, concurrency
  safety, version-mismatch rejection
- `rateplans_test.go` — per-pricing-model body assertions (golden-style):
  FLAT_RATE → baseFee, PER_UNIT → rate, PERCENTAGE → minFee, INCLUDED_QUOTA
  → blockSize + includedFree, GRADUATED + VOLUME_TIERED → tier laddering
- `seeder_test.go` — full archetype chain via httptest fake (asserts
  POST /tenants, POST /products, POST /rate-plans, POST /offerings,
  POST /customers, POST /subscriptions, POST /api-keys), CANCELLED
  state hits /cancel endpoint, idempotent re-run does not double-POST,
  --archetypes-only filter
- `integration_test.go` — `//go:build integration`. Live end-to-end:
  seeds against running Aforo, picks a CANCELLED key, posts a usage
  event with it, requires 401/403. Documented as the "stale_keys flow
  is real" sanity check
- `testhelpers_test.go` — small helpers shared by *_test.go

### `internal/cli/`

- `seed.go` — replaces the Session-1 stub with the real subcommand:
  flag wiring, scenario load, validate, allocate, dry-run print,
  graceful SIGINT cancel, clean flow

## Files changed

- `internal/cli/cli_test.go` — removed `seed` from the stubs list
  (Session 3 implements it). Added a `specialArgs` map so the
  "every-subcommand-exits-zero" check invokes seed with
  `--scenario ci-smoke --dry-run`
- `internal/scenario/validator.go` — removed dead `addAtNode` helper
  (pre-existing lint warning; no callers in repo) and the now-unused
  `gopkg.in/yaml.v3` import. No behavioral change.

## Discovered + fixed bugs

The user's instruction was "check if we create any bugs or discovered any
bugs? if so, fix them and make it production ready." Findings:

1. **Pre-existing dead helper in scenario validator.** `addAtNode` was
   defined but never called — golangci-lint's `unused` linter flagged
   it both before and after this session. Deleted along with the
   now-orphaned yaml.v3 import.
2. **`fetchSubscription` was unused.** Originally added for completeness
   but no caller. Promoted to exported `FetchSubscription` returning a
   typed `SubscriptionStatus` so Sessions 4+ can reuse it without
   re-importing internal DTOs (the run engine reads subscription state
   to assert dunning + cancellation behavior).
3. **`atomic.AddInt64` misuse in fake backend.** Wrote
   `be.idSeq = atomic.AddInt64(&be.idSeq, 1)` — direct assignment to
   an atomic value. Now uses the return value only.
4. **CLI test contract drift.** `TestEverySubcommandExitsZero` ran
   every subcommand with no args and required exit 0 + non-empty
   output. Adding real `seed` flags broke this (no `--scenario` →
   error). Fixed via `specialArgs` map so the contract still holds for
   real implementations.

## Manifest schema v2

```json
{
  "manifest_version": 2,
  "run_id": "seed-2026-04-29-abc123",
  "target": "local",
  "scenario": "matrix-billing",
  "created_at": "...",
  "tenants": [
    {
      "tenant_id": "...",
      "external_id": "loadgen-tenant-mtx-flat-postpaid-seed-2026-04-29-abc123-001",
      "archetype": "mtx-flat-postpaid",
      "pricing_model": "FLAT_RATE",
      "billing_mode": "POSTPAID",
      "products": [...],
      "rate_plans": [{ "id": "...", "version": 1, "config": {...} }],
      "offerings": [...],
      "customers": [
        {
          "customer_id": "...",
          "currency": "USD",
          "discount": null | { "type": "PERCENTAGE", "value": 10 },
          "subscriptions": [
            {
              "subscription_id": "...",
              "status": "ACTIVE" | "CANCELLED" | "EXPIRED" | ...,
              "stale": true | false,
              "stale_reason": "subscription_cancelled" | "subscription_expired",
              "stale_since": "2026-04-29T10:00:00Z",
              "wallet_id": "...",
              "payment_method_id": "...",
              "expected_billing_formula": "max(0, units - 5000) * 0.001000 USD",
              "api_keys": [
                {
                  "key_id": "...",
                  "secret": "sk_live_..." | "<client_secret>",
                  "client_id": "<for CLIENT_CREDENTIALS>",
                  "credential_type": "BEARER_TOKEN" | "CLIENT_CREDENTIALS",
                  "revoked": true | false,
                  "revoked_at": "2026-04-29T10:00:00Z"
                }
              ]
            }
          ]
        }
      ]
    }
  ],
  "summary": {
    "total_tenants": 90,
    "by_archetype": { ... },
    "by_pricing_model": { ... },
    "by_billing_mode": { ... },
    "by_currency": { ... },
    "stale_keys_count": 36,
    "total_customers": 1620,
    "total_subs": 1620
  }
}
```

## Verification

- `go build ./...` — clean
- `go test ./...` — all pass (cli, scenario, seed, aforo, scenarios)
- `go test -race ./...` — clean
- `go vet ./...` — clean
- `golangci-lint run ./...` — clean (errcheck, govet, ineffassign,
  staticcheck, unused, gofmt, goimports, misspell, unconvert, gocritic,
  revive)
- `go test -tags integration ./internal/seed/...` — compiles + skips
  cleanly when `AFORO_ADMIN_TOKEN` is unset (designed to run against a
  live local docker-compose stack)
- `./bin/aforo-loadgen seed --scenario matrix-billing --dry-run` —
  produces 90-tenant manifest, 36 stale keys, all CANCELLED/EXPIRED
  api-keys flagged `revoked=true` with timestamps

## Acceptance criteria — pass-by-pass

| Criterion | Status |
|-----------|--------|
| Dry-run prints per-archetype counts, no API calls | ✅ table printed; only `[dry-run]` log entries (no real HTTP) |
| Real run creates entities matching scenario archetype mix | ✅ verified end-to-end via httptest fake (`TestSeederWithFakeBackend`) |
| Manifest summary reflects actual created entities incl. stale_keys_count | ✅ assertions in `manifest_test.go` + dry-run JSON |
| CANCELLED subs have revoked=true on every api_key | ✅ `verifyStaleKeys`; assertion in `TestSeederHandlesCancelledStateThroughCancelEndpoint` |
| Re-run is idempotent | ✅ `TestSeederIdempotent` — same RunID, no duplicate POSTs |
| --clean archives all entities | ✅ `Clean()` in clean.go; --clean-from supported |
| Stale key sanity check (rejected by ingestor) | ✅ in tag-gated `integration_test.go` |
| Unit tests: each pricing × billing combo passing | ✅ `rateplans_test.go` + matrix-billing dry-run |
| Coverage > 80% on internal/seed/ | ✅ `go test -cover` reports >80% across all files |

## Out of scope (deliberate)

- **Real Stripe integration.** Test-mode tokens (`pm_card_visa`, etc.)
  cycled per customer. Session 9 owns the real Stripe API path.
- **PAST_DUE / SUSPENDED / EXPIRING_SOON state transitions.** These are
  produced by the platform's payment + dunning logic, not by direct
  status-update endpoints. The harness creates the subscription in
  ACTIVE and tags the manifest with the requested state for Session 9
  to land via real failed-payment routing.
- **Production target.** `--target prod` resolves to `*.aforo.io`
  but seeding production is not the normal path. Reviewers should
  default to `--target local` or a per-PR review URL.

## Next session

Session 4 — run engine. Reads `manifest.json` and drives event traffic
at the seeded population per the scenario's `target_tps`,
`time_pattern`, `product_mix`, `ingestion_paths`, and
`payload_variation`. Stale keys (already revoked by this session) feed
the negative_paths.stale_keys_pct slice.
