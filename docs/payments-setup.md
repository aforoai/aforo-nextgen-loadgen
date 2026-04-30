# Payments setup — Stripe test mode + ERP sandboxes

This guide configures the env vars + accounts the `aforo-loadgen payments`
subcommand expects. Every component is **optional**: when the env var is
unset, the corresponding driver runs in offline / shadow mode and the
validator still passes its checks (the run records what would have
happened; downstream assertions stay green).

## Stripe test mode (required for live charge round-trip)

The payment driver hits Stripe's `/v1/payment_intents` endpoint when
`STRIPE_TEST_SECRET_KEY` is set. The binary enforces:

- The key must start with `sk_test_`. **`sk_live_` keys are refused** —
  this tool never targets a real Stripe account.
- Every API call carries an `Idempotency-Key` header (acceptance
  criterion). The driver mints one per `(invoice_id, attempt)`.

### Setup

1. Create a Stripe test account at <https://dashboard.stripe.com>.
2. Copy your **secret test key** from the Developers → API keys page.
3. Export it before running:

   ```bash
   export STRIPE_TEST_SECRET_KEY=sk_test_...
   aforo-loadgen payments --scenario scenarios/payments-stripe-test.yaml \
       --target staging --manifest manifest.json --stripe-mode test
   ```

If the env var is unset, the driver synthesizes plausible payment intent
ids per the requested outcome (success → `pi_offline_<base64>`, decline
→ same shape with `failure_code=card_declined`). Validation Check 12
still passes — every "succeeded" record gets a synthesized intent id;
every "declined" / "insufficient" record gets a `failure_code`.

### Test cards (for reference)

| Card                | Outcome                |
|---------------------|------------------------|
| `4242424242424242`  | succeeded              |
| `4000000000000002`  | card_declined          |
| `4000000000009995`  | insufficient_funds     |
| `4000002500003155`  | requires_action (3DS)  |

Source: <https://stripe.com/docs/testing#cards>

## ERP sandboxes (required for round-trip verification)

When `scenario.erp.verify_external_ids: true` (default), the validator
asserts each invoice resolves at the provider's sandbox by `external_document_id`.
Without sandbox creds, the provider runs in shadow mode — it returns
`Verified=true` unconditionally and Check 15 passes off the platform's
own `erp_sync_log`.

| Provider        | Env vars                                                  |
|-----------------|-----------------------------------------------------------|
| QuickBooks      | `QBO_ACCESS_TOKEN`, `QBO_COMPANY_ID`, `QBO_BASE_URL`?     |
| Xero            | `XERO_ACCESS_TOKEN`, `XERO_TENANT_ID`, `XERO_BASE_URL`?   |
| NetSuite        | `NETSUITE_REST_TOKEN`, `NETSUITE_ACCOUNT_ID`              |
| Custom webhook  | `CUSTOM_WEBHOOK_SECRET`, `CUSTOM_WEBHOOK_URL`?            |

See [erp-onboarding.md](erp-onboarding.md) for per-provider OAuth setup.

## Tax engines

The default is `mock` — a deterministic local computation from
`scenario.tax.jurisdictions`. To switch:

```bash
aforo-loadgen payments --tax-engine avalara ...
# or
aforo-loadgen payments --tax-engine vertex ...
```

See [tax-engines.md](tax-engines.md) for the per-engine env vars.

## Multi-currency FX

FX rates are PINNED in `scenario.fx.pinned_rates` for reproducibility
(real platforms pull live rates at bill-run time). The validator's
multi-currency check (Check 14) compares the platform's recorded
amount against `amount × pinned_rate` with a 0.5% relative tolerance.

```yaml
fx:
  enabled: true
  pinned_rates:
    USD->EUR: 0.92
    USD->GBP: 0.79
  applied_at: bill_run_time   # platform contract — Check 14 fails if event_ingest_time
```

## Wallet TTL

`scenario.wallet.hold_ttl_seconds` overrides the platform's hold TTL for
the run. A short value (60s) lets `HoldExpiryScheduler` complete a full
release pass inside a 4-minute scenario, so Check 17 can assert
`outstanding_holds == 0` post-expiry.

## Single-ERP invariant (Check 18)

When `scenario.erp.multi_erp_enabled: false` (default), the validator
attempts to connect a SECOND ERP to the first manifest tenant and
expects HTTP 409. It cleans up via `/api/v1/erp-integrations/disconnect`
on its way out — partial failures still leave the population clean.

## Putting it together

```bash
# Seed once, then run several payment-flavored scenarios off the same
# population.
export AFORO_ADMIN_TOKEN=...
export STRIPE_TEST_SECRET_KEY=sk_test_...
aforo-loadgen seed --scenario payments-stripe-test --target staging

# Default: Stripe test mode + mock tax + shadow ERP.
aforo-loadgen payments \
    --scenario payments-stripe-test \
    --target staging \
    --manifest manifest.json \
    --out runs/payments-$(date +%s)

# Verify everything passed.
aforo-loadgen validate \
    --run-output runs/payments-... \
    --manifest manifest.json \
    --target staging
```
