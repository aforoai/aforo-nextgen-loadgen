# Scenario YAML Schema (v1)

**Status:** Session 2 contract. Every later component (seed harness, run
engine, fault injectors, lifecycle stress, payments, ERP, chaos, assertions)
reads its config from this schema.

This document is the human-facing reference. The Go types in
[`internal/scenario/types.go`](../internal/scenario/types.go) are the source
of truth — when the code and this document disagree, the code wins. File
bug reports against this doc when you spot drift.

---

## Top level

```yaml
schema_version: 2                # int, required, must equal 2 today (v1 files auto-migrate on load — see below)
name: <kebab-case>               # string, required, must match ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$
description: <free text>         # optional
target_tps: <int>                # required, > 0
duration: <go duration>          # required, > 0 — "60s", "5m", "24h", "168h"
seed: <int>                      # optional, default 0; >= 0 for reproducibility

tenants: { ... }                 # required — see Tenants
time_pattern: constant           # one of: constant, sine_24h, bursty (default constant)
product_mix: { ... }             # optional — see Product mix
ingestion_paths: { ... }         # optional — see Ingestion paths
payload_variation: { ... }       # optional — see Payload variation

negative_paths: { ... }          # optional — fault injection
lifecycle: { ... }               # optional — subscription state transitions
payments: { ... }                # optional — Stripe simulation
tax: { ... }                     # optional — tax engine config
erp: { ... }                     # optional — ERP sync
credit_notes: { ... }            # optional — refunds and partials
wallet: { ... }                  # optional — wallet auditing
chaos: { ... }                   # optional — infra fault injection

assertions: { ... }              # optional — post-run pass/fail thresholds
metadata: { ... }                # optional, free-form, never validated
```

### Strict decoding

Unknown fields are an error. A typo like `target_tps_` produces

```
yaml decode: yaml: unmarshal errors:
  line 5: field target_tps_ not found in type scenario.Scenario
```

This rule applies recursively — unknown fields anywhere in the document
are rejected. Use `metadata:` for free-form notes you want to preserve.

---

## Tenants

```yaml
tenants:
  count: <int>                    # required, > 0
  distribution: uniform           # one of: uniform, pareto_80_20, zipf
  archetypes:                     # required, len >= 1
    - { name, weight, pricing_model, billing_mode, product_types,
        customer_count, currency_mix, subscription_state_mix,
        discount_mix, rate_config }
```

`distribution` shapes traffic across the tenant population:

- **uniform** — every tenant gets equal share
- **pareto_80_20** — top 20% of tenants take 80% of traffic
- **zipf** — Zipfian; tenant rank N takes 1/N share

### TenantArchetype

A scenario provisions `count` tenants drawn from the weighted archetype
list. Archetypes are how a single scenario covers many pricing/billing/
state combinations realistically.

```yaml
archetypes:
  - name: walk-api-postpaid-perunit       # required, kebab-case, unique
    weight: 0.20                          # required, in [0, 1]
    pricing_model: PER_UNIT               # required — see Pricing models
    billing_mode: POSTPAID                # required — POSTPAID | PREPAID | HYBRID
    product_types: [API]                  # required, len >= 1
    customer_count: 100                   # required, > 0
    currency_mix:                         # optional — weights sum to 1.0
      USD: 0.7
      EUR: 0.2
      GBP: 0.1
    subscription_state_mix:               # optional — weights sum to 1.0
      ACTIVE: 0.85
      TRIALING: 0.05
      PAUSED: 0.03
      CANCELLED: 0.02
      EXPIRED: 0.05
    discount_mix:                         # optional — weights sum to 1.0
      none: 0.7
      pct_10: 0.2
      fixed_50: 0.1
    rate_config:                          # required shape varies by pricing_model
      per_unit_rate_usd: 0.001
```

**Archetype weight invariant:** weights of every archetype in `archetypes`
sum to `1.0 ± 0.001`.

### Multiple rate cards per archetype (v2, 2026-07-22)

An archetype can declare **N rate cards** — different pricing tiers on the same
product set — via the `rate_cards` array. Each card carries its own pricing
model + rate config + metric overrides + dimension pricing + customer share,
so a single archetype models the "Starter / Pro / Enterprise" pattern
without duplicating the entire archetype block.

```yaml
tenants:
  archetypes:
    - name: three-tier
      weight: 1.0
      # Top-level pricing_model / rate_config / metric_configs still work —
      # they become the DEFAULT that any per-card field inherits from when
      # left unset. Every existing v1 scenario keeps producing byte-
      # identical output via this back-compat path.
      pricing_model: PER_UNIT
      billing_mode: POSTPAID
      product_types: [API]
      customer_count: 100
      rate_config: { per_unit_rate_usd: 0.001 }

      rate_cards:
        - name: starter                      # required — unique within archetype
          pricing_model: PER_UNIT            # optional — inherits from top-level
          rate_config: { per_unit_rate_usd: 0.002 }
          customer_share: 0.6                # 60% of customer_count subscribe to this card

        - name: pro
          pricing_model: INCLUDED_QUOTA
          rate_config:
            included_free_units: 10000
            per_unit_rate_usd: 0.001
          customer_share: 0.3

        - name: enterprise
          pricing_model: FLAT_RATE
          rate_config: { flat_fee_usd: 499 }
          customer_share: 0.1
```

**`customer_share` invariant:** across every card in the archetype, shares
must sum to `1.0 ± 0.001`. When ALL cards have `customer_share` unset,
each card is assigned an equal share of `1.0 / N`.

**Backward compatibility:** when `rate_cards` is absent, `applyDefaults`
synthesizes a single card named `default` from the archetype's top-level
`pricing_model` / `billing_mode` / `rate_config` / `metric_configs` /
`dimension_pricing` fields. Existing v1 scenarios keep loading and produce
byte-identical output — the seed harness's behavior for a single-card
archetype is identical to v1.

**Optional per-card fields:**

- `billing_mode` — override the archetype's top-level billing mode.
- `product_filter: [ProductType, ...]` — bind this card only to products of
  the listed types from `archetype.product_types`. Empty = bind to every
  product (default).
- `offerings:` — explicit list of offerings that wrap this card. Each entry
  can carry its own `currency`, `billing_mode`, `trial_days`, and `name`.
  When omitted, the seeder generates one offering per currency in the
  archetype's `currency_mix` (preserves v1 fan-out behavior).

**v1 files auto-migrate:** loading a `schema_version: 1` file bumps it to
2 and normalizes it through the same `applyDefaults` path. No manual
edit is required; the golden test asserts every scenario in
`/scenarios/*.yaml` sits at `schema_version: 2` today.

### Pricing models and `rate_config`

| Pricing model     | Required `rate_config` fields                    |
| :---------------- | :----------------------------------------------- |
| `PER_UNIT`        | `per_unit_rate_usd > 0`                          |
| `FLAT_RATE`       | `flat_fee_usd > 0`                               |
| `PERCENTAGE`      | `percentage_rate > 0` (optional `min_fee_usd`, `charge_base_per_event_usd`) |
| `INCLUDED_QUOTA`  | `included_free_units > 0` and `per_unit_rate_usd > 0` (overage rate) |
| `GRADUATED`       | `graduated_tiers: [...]` (len >= 1)              |
| `VOLUME_TIERED`   | `volume_tiers: [...]` (len >= 1)                 |

A tier band is `{ up_to_units: <int>, unit_price_usd: <float>,
flat_fee_usd?: <float> }`. The last tier's `up_to_units: 0` means
"unbounded".

**`PERCENTAGE` charge base.** PERCENTAGE bills `events × charge_base × rate`.
Set `charge_base_per_event_usd` to the average per-event amount you want
the oracle to assume — e.g. `100.00` for "$100 average transaction".
Omitting it makes the oracle fall back to `1.0` per event, which reduces
the calculation to `events × rate`. That is fine for shape testing but
unrealistic for revenue assertions on payment-processing workloads.
`scenarios validate` prints a warning when PERCENTAGE archetypes omit
this field.

### Billing modes

`POSTPAID` archetypes have no extra requirements.

**`PREPAID` and `HYBRID` archetypes require
`rate_config.wallet_initial_balance_usd > 0`.** This is enforced — a
PREPAID tenant with a $0 wallet would fail at the very first event.

### Subscription states (9 total)

`CREATED, TRIALING, ACTIVE, PAST_DUE, PAUSED, EXPIRING_SOON, EXPIRED,
CANCELLED, SUSPENDED` — mirrors the platform state machine.

**Stale-key rule:** if you set `negative_paths.stale_keys_pct > 0`, at
least one archetype must include `CANCELLED` or `EXPIRED` in its
`subscription_state_mix`. Otherwise the fault injector has no stale keys
to use.

---

## Time pattern

`constant | sine_24h | bursty`. The runner shapes overall TPS over time
according to this; `time_pattern: constant` keeps a flat line.

---

## Product mix

```yaml
product_mix:
  API: 0.4
  AI_AGENT: 0.25
  MCP_SERVER: 0.25
  AGENTIC_API: 0.10
```

Optional. When present, weights must sum to 1.0. When omitted, the
runner draws from each archetype's own `product_types`.

---

## Ingestion paths

The 16 supported channels:

| Channel             | YAML key             |
| :------------------ | :------------------- |
| Direct REST POST    | `rest_direct`        |
| SDK — Node.js       | `sdk_node`           |
| SDK — Python        | `sdk_python`         |
| SDK — Java          | `sdk_java`           |
| SDK — Go            | `sdk_go`             |
| Gateway — Kong      | `gateway_kong`       |
| Gateway — Apigee    | `gateway_apigee`     |
| Gateway — AWS APIGW | `gateway_aws`        |
| Gateway — Azure APIM| `gateway_azure`      |
| Gateway — MuleSoft  | `gateway_mulesoft`   |
| Gateway — APISIX    | `gateway_apisix`     |
| Gateway — Tyk       | `gateway_tyk`        |
| Gateway — Gravitee  | `gateway_gravitee`   |
| Gateway — Envoy     | `gateway_envoy`      |
| Webhook receiver    | `webhook_receiver`   |
| CSV upload          | `csv_upload`         |

When provided, weights must sum to 1.0. When omitted, the runner
defaults to `rest_direct: 1.0`.

---

## Payload variation

```yaml
payload_variation:
  small_pct: 0.7        # ~200 bytes
  medium_pct: 0.25      # ~2KB
  large_pct: 0.05       # ~20KB nested
```

When all three are zero (or the section is omitted), the defaults above
apply automatically. When any field is non-zero, all three must sum to 1.0.

---

## Negative paths (fault injection)

Each value is a fraction of total traffic in `[0, 1]`.

```yaml
negative_paths:
  late_events_pct: 0.01       # event_timestamp 2h in the past
  future_events_pct: 0.001    # >5min future (rejected)
  malformed_pct: 0.001        # invalid JSON
  wrong_auth_pct: 0.001       # fabricated bad credentials
  stale_keys_pct: 0.001       # keys from CANCELLED/EXPIRED subs
  oversize_pct: 0.0001        # >max body size
```

Setting all to zero (or omitting the section) leaves the run on the
happy path. Stale-key injection has a cross-field requirement — see the
TenantArchetype section.

---

## Lifecycle profile

```yaml
lifecycle:
  enabled: false                          # boolean — gate
  upgrades_per_hour_pct: 0.05             # in [0, 1]
  downgrades_per_hour_pct: 0.05
  pause_resume_per_hour_pct: 0.05
  trial_conversion_per_hour_pct: 0.10
  trial_cancel_per_hour_pct: 0.05
  migrate_per_hour_pct: 0.02
  retry_payment_per_hour_pct: 0.10
```

When `enabled: false`, the rates are ignored. When `enabled: true`, every
rate is validated to be in `[0, 1]`. Session 6 owns the actual
transitions.

---

## Payments

```yaml
payments:
  enabled: false                          # boolean — gate
  stripe_mode: test                       # test | live (auto-set to test when enabled)
  success_pct: 0.90
  decline_pct: 0.08
  insufficient_funds_pct: 0.02
```

When `payments.enabled: true`, three additional facts hold:

1. `stripe_mode` is required and must be `test` or `live`.
2. `success_pct + decline_pct + insufficient_funds_pct` must equal 1.0
   (or all be zero — runner default).
3. `stripe_mode: test` reads `AFORO_STRIPE_TEST_KEY` from the environment
   at run time. The validator does NOT verify the env var is set — that
   is enforced by the run engine in Session 9.

---

## Tax

```yaml
tax:
  engine: mock                            # mock | avalara | vertex (default mock)
  jurisdictions:
    "US-CA": 0.0925                       # 9.25% — express percentages as decimals
    "EU-DE": 0.19
```

Every rate must be in `[0, 1]`.

---

## ERP

```yaml
erp:
  enabled: false
  providers_per_tenant_mix:               # required when enabled — sum to 1.0
    quickbooks: 0.4
    xero: 0.3
    netsuite: 0.2
    custom_webhook: 0.1
  sync_sla_seconds: 60                    # required when enabled, > 0
```

Provider names are restricted to the four above.

---

## Credit notes

```yaml
credit_notes:
  enabled: false
  refund_pct: 0.05                        # in [0, 1]
  partial_pct: 0.50                       # in [0, 1]
```

---

## Wallet

```yaml
wallet:
  hold_expiry_audit: true                 # boolean
```

When `true`, post-run assertions verify that no orphan wallet hold
records remain.

---

## Chaos

```yaml
chaos:
  enabled: false
  events:
    - at: 30m                             # required, >= 0 — duration from run start
      type: kill_pod                      # required, non-empty (Session 11 enumerates)
      duration: 5m                        # required, > 0
      params:                             # optional, free-form
        target: usage-ingestor
        replicas: 1
```

When `enabled: true`, `events` must contain at least one event.

---

## Assertions

```yaml
assertions:
  events_lost_max: 0                                  # >= 0
  invoice_revenue_drift_pct_max: 0.005                # in [0, 1]
  p99_latency_ms_max: 1000                            # >= 0
  per_tenant_p99_fairness_max_stddev_pct: 0.20        # in [0, 1]
  redis_cache_hit_ratio_min: 0.85                     # in [0, 1]
  cross_tenant_leakage_max: 0                         # MUST equal 0 (hard rule)
  per_archetype_billing_match: true                   # boolean
  stale_key_zero_false_positives: true                # boolean
```

**`cross_tenant_leakage_max` MUST be `0`.** Aforo is a multi-tenant SaaS
billing platform; even one leaked event is a data-isolation breach.
Setting it `> 0` is a validation error.

---

## Validation errors

The validator collects every error it finds in one pass and reports them
in stable, sorted order:

```
matrix-billing.yaml:42:7: tenants.archetypes[3].weight: weight 1.5 must be in [0, 1]
matrix-billing.yaml:60:5: payment_variation: small+medium+large = 0.95; must be 1.0 ± 0.001
```

The format is `<file>:<line>:<column>: <path>: <message>` — IDE/editor
parseable. Sources loaded from memory (e.g. embedded built-ins) format
without the file prefix as `<scenario>:<line>:<col>: ...`.

---

## Schema migration

```go
const CurrentSchemaVersion = 1
```

`schema_version` must be present and must equal a known version. A
loaded scenario from a future version (`schema_version: 2` against a
v1 tool) is rejected with a clear "upgrade aforo-loadgen" message. When
v2 ships, `Migrate()` will gain an upgrade chain that walks v1 → v2
in-place.

---

## Reference: built-in scenarios

Every release ships these six. They live in [`scenarios/`](../scenarios)
and are bundled into the binary via `embed.FS`.

| Name                  | TPS    | Duration | Tenants | Archetypes | Purpose |
| :-------------------- | :----- | :------- | :------ | :--------- | :------ |
| `ci-smoke`            | 50     | 60s      | 1       | 1          | CI gate — happy path only |
| `crawl-e2e`           | 50     | 5m       | 4       | 4          | One archetype per product type |
| `walk-realistic-50t`  | 2 000  | 24h      | 50      | 8          | Mid-scale 24h sine pattern |
| `run-15k-7day`        | 15 000 | 168h     | 500     | 12         | Headline endurance run |
| `matrix-billing`      | 1 000  | 2h       | 90      | 30         | Every (pricing × billing) combo + variants |
| `lifecycle-stress`    | 1 000  | 4h       | 20      | 5          | Saturate the subscription state machine |

List them at run time:

```sh
aforo-loadgen scenarios list
aforo-loadgen scenarios show ci-smoke
aforo-loadgen scenarios archetypes walk-realistic-50t
aforo-loadgen scenarios validate path/to/my-scenario.yaml
```
