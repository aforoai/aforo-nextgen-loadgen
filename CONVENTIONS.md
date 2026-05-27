# Loadgen ↔ Backend Conventions

This document defines the rules every contributor follows when adding or
modifying a loadgen ↔ backend interaction. Two principles drive everything
below:

1. **Loadgen invents no field names.** Every json tag, every Go variable
   that names an entity attribute, every manifest column maps 1:1 to a real
   backend column. The only loadgen-internal naming is `seed_key` (an
   opaque deterministic Idempotency-Key value), and even that is namespaced
   so it can never be confused with a backend identifier.
2. **Every drift fails CI loudly.** The contract test at
   `internal/seed/contract_test.go` reflects every loadgen request/response
   struct against a committed OpenAPI snapshot. A field rename that ships
   without a coordinated update breaks the build.

These principles eliminate the failure family the 2026-05-27 audit
uncovered (loadgen-invented `externalId` field that backend silently dropped
on 8 of 9 entities, breaking every cross-day idempotency lookup).

---

## Wire-format alignment

### Rule
Every `json:"..."` tag on every loadgen request or response struct MUST be
present as a column on the corresponding backend Java DTO (verified by
`internal/seed/contract_test.go`).

### Why
"Forward-compat" phantom fields (loadgen sending fields backend doesn't
declare yet, hoping it'll adopt them) silently degrade to no-ops. When the
backend never adopts them, debugging becomes a hunt for "why doesn't this
field do anything?". When the backend DOES adopt them with subtly
different semantics, behavior changes silently mid-deploy.

### How
For each loadgen request struct (`xxxCreateRequest`):
- Open the corresponding Java DTO (e.g.
  `aforo-nextgen-pricing-service/.../CreateRatePlanRequest.java`).
- Loadgen carries the intersection of fields it actually needs to send +
  fields the DTO declares. Everything else: drop.

For each response struct (`xxxResponse`):
- Loadgen carries the subset of fields the seed harness actually reads.
- Renaming a field on the backend DTO immediately breaks the snapshot
  via `make sync-openapi`; the contract test then fails the PR.

### Exception
**Tenants only.** `organization-service/internal/admin` ships a
`LoadgenTenantResponse` that genuinely persists `externalId` and exposes
`?externalId=` as a list filter. Other entities have no such column.

---

## Identity model

Three distinct kinds of identifier are used and must NOT be conflated:

| Identifier | Owner | Used for | Example |
|---|---|---|---|
| Backend primary key (`id`) | backend, server-assigned | unique row reference | `cus-550e8400-e29b...` |
| Backend natural identity (`name`, `email`, `code`) | backend, user-provided | deterministic cross-day lookup | `"Loadgen Customer enterprise 001"`, `ext-XXX@loadgen.aforo.test` |
| Loadgen seed key (`seedKey`) | loadgen, deterministic | HTTP Idempotency-Key header value | `loadgen-customer-mtx-quota-prepaid-seed-2026-05-27-16ef11-1003` |

### Backend primary key (`id`)
- Server-assigned UUID-or-prefixed-id. Stable per row.
- Loadgen reads it from the create response, stores it as
  `ManifestXxx.{entity}_id`, threads it through downstream calls
  (e.g. subscription create passes `customerId` = customer's `id`).
- Never invented by loadgen.

### Backend natural identity
- Whatever column backend exposes as a deterministic, user-provided
  unique field per (tenant, entity-type, scenario-context). Differs per
  entity:

  | Entity | Natural identity field | Lookup function |
  |---|---|---|
  | Product | `name` (per-tenant unique) | `lookupProductByName` |
  | Customer | `email` (per-tenant unique) | `lookupCustomerByEmail` |
  | Rate plan | `name` (per-tenant unique) | `lookupRatePlanByName` |
  | Offering | `code` (per-tenant UNIQUE constraint) | `lookupOfferingByCode` |
  | Subscription | `(customerId, offeringId)` composite | `lookupSubscriptionByCustomerAndOffering` |
  | Wallet | `customerId` (one wallet per customer) | `lookupWalletByCustomer` |
  | API key | `(customerId, accessorId)` composite | `lookupAPIKeyByAccessor` |
  | Payment method | `customerId` (single active method per customer) | `lookupPaymentMethodByCustomer` |
  | Tenant | `externalId` (real column on LoadgenTenantResponse) | `lookupTenantByExternalID` |

- The natural identity is what the lookup-before-create idempotency path
  queries. It must survive backend DB resets — loadgen-generated names
  are deterministic per (archetype, seq) so a fresh DB + same scenario
  produces the same names + finds the same logical rows.

### Loadgen seed key
- Loadgen-internal, deterministic string with shape
  `loadgen-{kind}-{archetype}-{run}-{seq:03d}` (see `seedKey()` in
  `internal/seed/seeder.go`).
- Sent ONLY as the HTTP `Idempotency-Key` header on POST.
- NEVER appears in a request body or response body (except tenants).
- Backend's `IdempotencyResponseService` caches the create response under
  `(tenant, seedKey)` for 24h. A retry within that window returns the
  cached body; the lookup path handles cross-day cases.
- Stored in the manifest as `seed_key` per entity for traceability
  (grep-debug: same string in loadgen seed logs + backend
  idempotency_responses table + manifest).

### What's NOT an identifier
- `external_id` on any entity other than tenant. Backend doesn't have
  such a column. Loadgen used to invent it; we don't anymore.
- Loadgen-side counters, scenario seeds, archetype names — these are
  *components* of the seedKey but not identifiers in their own right.

---

## Idempotency contract

Two layers protect against duplicate creates:

1. **Within 24h of the original create (server-side)**: backend's
   `IdempotencyResponseService` caches the response under
   `(tenant, idempotencyKey)`. A POST with the same `Idempotency-Key`
   header returns the cached response body — no duplicate row.
2. **Cross-day (loadgen-side)**: `provisionXxx` calls
   `lookupXxxByNaturalIdentity` BEFORE the POST. If a row matching the
   natural identity exists, return it. Skip the POST entirely.

Both layers MUST be wired for every new entity. The 24h-cache alone is
insufficient because:
- DB resets clear it.
- Scenario re-runs after the cache TTL expire create duplicates.

The cross-day lookup alone is insufficient because:
- Race conditions between two concurrent loadgen instances starting at
  the same time can both miss the lookup, both POST, both create.
- The Idempotency-Key header eliminates this race by serializing the
  competing POSTs server-side.

---

## Manifest schema

Each entity's manifest entry carries THREE identifier flavors:

```json
{
  "product_id": "prod-abc123",                                   // backend primary key
  "name": "Loadgen Product enterprise API",                      // backend natural identity
  "seed_key": "loadgen-product-API-enterprise-2026-05-27-001"    // loadgen Idempotency-Key value
}
```

- **`{entity}_id`**: always the backend primary key. Required.
- **Natural identity field(s)**: `name` / `email` / `code` / etc — whatever
  the lookup-by-natural-identity function queries against. Required for
  every entity that has a lookup path.
- **`seed_key`**: loadgen Idempotency-Key value. Required EXCEPT for
  entities created without an Idempotency-Key (rare).
- **Tenant exception**: tenant uses `external_id` instead of `seed_key`
  because backend really does store the column.

---

## Adding a new entity

When adding a new `provisionXxx` for a 10th entity type:

1. **Identify the backend create endpoint** (e.g.
   `POST /api/v1/foo` on `foo-service`).
2. **Find the request DTO** (e.g.
   `aforo-nextgen-foo-service/.../CreateFooRequest.java`). Mirror its
   field names exactly in your `fooCreateRequest` struct. No phantom
   fields. Run `make sync-openapi` to refresh the snapshot, then `make
   contract-test` — if any of your tags fail, the contract test tells
   you which.
3. **Find the natural identity field** (the column that's per-tenant
   unique and operator-supplied — usually `name`, `code`, or `email`).
   Write `lookupFooByXxx` that queries `?xxx=` (if backend supports
   server-side filter) or pages + filters client-side.
4. **Wire `provisionFoo(ctx, c, tenantID, seedKey, ...natural identity
   args...)`**:
   - Lookup-before-create using natural identity.
   - On 409, re-look-up + return.
   - Pass `Idempotency: seedKey` in `RequestOptions`.
5. **Extend `ManifestFoo`** with `{foo_id, <natural identity fields>,
   seed_key}` per the schema above.
6. **Register the struct in `contractEntries()`** in
   `internal/seed/contract_test.go` with `Expectation: PerfectMatch`
   for the response, and `PerfectMatch` for the request (no phantom
   fields allowed — that's the convention).
7. **Generate the seedKey in seeder.go** via `seedKey("foo", a.Name,
   s.cfg.RunID, seq)` and pass through.

---

## Maintenance workflow

| Backend change | Loadgen action |
|---|---|
| New field added to existing DTO | None required. Loadgen reads/writes a subset; new fields auto-tolerated by Jackson. Refresh snapshot via `make sync-openapi` if loadgen wants to start consuming the field. |
| Existing field renamed | `make sync-openapi` → `make contract-test` fails on the loadgen struct → rename the json tag → commit both changes together. |
| Existing field removed | Same as rename. Contract test fails until loadgen drops the tag. |
| New entity added (new DTO) | If loadgen needs to seed it, follow "Adding a new entity" above. Otherwise no action. |
| Backend adds `externalId` to an entity that didn't have one | Loadgen can adopt the column on the response struct + use it as the natural identity. Coordinate with a single PR that refreshes the snapshot + updates the lookup function. |

---

## Why this matters (the why-not-just-keep-things-the-old-way answer)

The pre-refactor architecture invented `externalId` as a loadgen concept
and sent it on every POST. Backend silently dropped it (Jackson ignores
unknown fields by default). The lookup-before-create path read `externalId`
back, found "" because the field wasn't on the response, decided the entity
didn't exist, and POSTed a duplicate. This silently fanned out duplicate
customers/subscriptions/etc on every cross-day rerun for the lifetime of
the codebase. The audit-of-record is in CLAUDE.md under "2026-05-27".

The post-refactor architecture has no invented fields. Every json tag is a
real backend column (verified by the contract test). The deterministic
identity for lookups is backend's natural identity (`name`, `email`,
`code`, etc.) which backend actually returns. The Idempotency-Key
contract is honored at the HTTP layer where it lives. Debugging is
straightforward: the same string identifying an entity in loadgen logs
also identifies it in backend logs.

Industry references for the conventions above:
- HTTP Idempotency-Key: IETF draft `draft-ietf-httpapi-idempotency-key-header` (Stripe-style).
- Lookup-by-natural-identity: REST resource design (Fielding §5.2.1.1, "Resource Identifiers").
- OpenAPI snapshot + contract test: Pact, Dredd, Schemathesis — established pattern.
- No-phantom-fields: Postel's Law inverted — "be conservative in what you
  send" applies; "be liberal in what you accept" applies to the receiver,
  not the sender.
