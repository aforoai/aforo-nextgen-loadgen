# ERP onboarding — per-provider sandbox guide

The loadgen verifies that every issued invoice round-trips to the
configured ERP within `scenario.erp.sync_sla_seconds`. Each provider
has a thin shim (`internal/erp/<provider>.go`) that runs in two modes:

| Mode    | When                                  | What happens                       |
|---------|---------------------------------------|------------------------------------|
| live    | Provider env vars set                 | GETs the document at the sandbox   |
| shadow  | Env vars unset                        | Returns `Verified=true` blindly    |

Shadow mode keeps Check 15 green when sandbox creds aren't available
in CI — the platform's own `erp_sync_log` is still the source of truth
for "did the platform claim it synced".

## QuickBooks Online (sandbox)

1. Create a QuickBooks developer account: <https://developer.intuit.com>.
2. Create a "Sandbox Company".
3. Generate an OAuth 2.0 access token via Intuit's OAuth Playground, or use a long-lived one if you've negotiated it.
4. Find the **Realm ID** under the sandbox company → API Connector tab.

Export:

```bash
export QBO_ACCESS_TOKEN="..."
export QBO_COMPANY_ID="123456789012345"
# Optional override (default = sandbox URL):
export QBO_BASE_URL="https://sandbox-quickbooks.api.intuit.com"
```

The shim verifies invoices via `GET /v3/company/{realm}/invoice/{id}`.

## Xero (sandbox)

1. Create a Xero developer account: <https://developer.xero.com>.
2. Create an "App" in the developer portal — choose Web App.
3. Use the OAuth 2.0 + PKCE flow to obtain an access token.
4. Note your **Tenant ID** from the connections endpoint
   (`GET https://api.xero.com/connections`).

Export:

```bash
export XERO_ACCESS_TOKEN="..."
export XERO_TENANT_ID="..."
# Optional override (default = api.xero.com):
export XERO_BASE_URL="https://api.xero.com"
```

The shim sends `Xero-Tenant-Id` on every call as Xero requires, and
verifies invoices via `GET /api.xro/2.0/Invoices/{guid}`.

## NetSuite (sandbox)

NetSuite production setup uses Token-Based Authentication (TBA) over
OAuth 1.0a + HMAC-SHA256 — too heavy for a load test shim. The
loadgen's NetSuite shim uses a long-lived bearer token instead:

1. Open NetSuite Admin → Setup → Users/Roles → Access Tokens.
2. Generate an **Application Token**.
3. Note your **Account ID** (e.g. `1234567` or `1234567_SB1` for sandbox).

Export:

```bash
export NETSUITE_REST_TOKEN="..."
export NETSUITE_ACCOUNT_ID="1234567_SB1"
```

The shim composes `https://{NETSUITE_ACCOUNT_ID}.suitetalk.api.netsuite.com`
and verifies invoices via `GET /services/rest/record/v1/invoice/{id}`.

> **Note:** real production sync uses OAuth 1.0a TBA. The platform's
> own `NetSuiteAdapter` implements that; this shim is for load-test
> verification only and intentionally takes a simpler bearer-token
> path.

## Custom Webhook

The custom-webhook adapter has TWO submodes:

### Receiver mode (default)

The shim spawns an in-process `httptest.Server` and prints its URL.
Configure the platform's `CustomWebhookAdapter` to POST to that URL.
The shim then verifies HMAC-SHA256 signatures (header `X-Aforo-Signature`,
either bare hex or `sha256=hex` Stripe-style).

```bash
# default secret used by the in-process receiver:
export CUSTOM_WEBHOOK_SECRET="aforo-loadgen-default-secret"
```

### Verifier mode (pre-existing receiver)

If you already have a webhook target running (e.g. an inspector tool
like RequestBin), the shim attaches as a verifier:

```bash
export CUSTOM_WEBHOOK_URL="https://your-inspector.example.com/aforo"
export CUSTOM_WEBHOOK_SECRET="..."
```

The shim doesn't spin a server; instead, the loadgen orchestrator emits
the webhook payload directly using `internal/erp.SignBody` and asserts
your inspector captured it.

## Per-tenant provider routing

The platform stores ONE ERP per tenant (single-ERP invariant — Check 18
asserts a 2nd connect returns 409). The loadgen scenario picks the
highest-weight provider as the per-tenant default:

```yaml
erp:
  enabled: true
  providers_per_tenant_mix:
    quickbooks: 0.6      # 60% of tenants → QuickBooks
    xero: 0.4            # 40% of tenants → Xero
  sync_sla_seconds: 60
  multi_erp_enabled: false
  max_retries: 3
  verify_external_ids: true
```

When you want a load test that exercises ALL four providers, give them
equal weight (see `scenarios/erp-sync-validation.yaml`). The loadgen
won't actually wire 4 ERPs to a single tenant — it picks the highest
weight (or the first one for ties) and routes every invoice for that
tenant through that provider. Across the population, all providers
get exercised.

## Verifying the round-trip

```bash
aforo-loadgen payments \
    --scenario scenarios/erp-sync-validation.yaml \
    --target staging \
    --manifest manifest.json \
    --erp-providers quickbooks,xero,netsuite,custom_webhook
aforo-loadgen validate \
    --run-output runs/<dir> \
    --manifest manifest.json \
    --target staging
```

Check 15 (`erp_sync`) reports per-provider sync rate, verification rate,
and p95 latency. Failures look like:

```
erp_sync         FAIL  only 87.50% of invoices verified at provider sandbox (want >=99%)
```
