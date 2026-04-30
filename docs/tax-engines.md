# Tax engines — mock / Avalara / Vertex

The loadgen ships three tax-engine implementations that all satisfy
`internal/tax.Engine`:

| Engine     | Default? | Live API | Fallback when creds missing |
|------------|----------|----------|-----------------------------|
| `mock`     | yes      | no       | n/a                          |
| `avalara`  | no       | yes      | mock with `Note` set        |
| `vertex`   | no       | yes      | mock with `Note` set        |

## Switching engines

In a scenario:

```yaml
tax:
  engine: avalara          # or vertex / mock
  jurisdictions:
    US-CA: 0.0925
    EU-DE: 0.19
```

Or override at the CLI:

```bash
aforo-loadgen payments --tax-engine avalara ...
```

## Mock

Deterministic local computation. No env vars. Used by every scenario
unless explicitly overridden. The validator's tax-math check (Check 13)
runs against the configured engine; with `mock` the check is the
strongest assertion possible (engine output equals `subtotal × rate`
exactly, modulo `tolerance_usd`).

## Avalara AvaTax v2

Lives at <https://sandbox-rest.avatax.com> by default. Required env vars:

| Var                    | Purpose                              |
|------------------------|--------------------------------------|
| `AVATAX_ACCOUNT_ID`    | numeric account id                   |
| `AVATAX_LICENSE_KEY`   | license key                          |
| `AVATAX_BASE_URL`      | optional override (production URL)   |
| `AVATAX_COMPANY_CODE`  | required by API; defaults to DEFAULT |

Auth is HTTP Basic — `account_id:license_key`, base64-encoded. Every
call POSTs `CreateTransactionModel` to `/api/v2/transactions/create`.

When the account or license key is missing, the engine falls back to
`mock` and stamps `Note: "AVATAX_ACCOUNT_ID/LICENSE_KEY missing — falling
back to mock"` on every Response. Check 13 still passes; the report
surfaces the engine name as `avalara-mock` so a human can see the
fallback fired.

## Vertex O Series

Lives at <https://restconnect.vertexsmb.com>. Required env vars:

| Var                  | Purpose                              |
|----------------------|--------------------------------------|
| `VERTEX_OAUTH_TOKEN` | bearer token (from Vertex OAuth flow)|
| `VERTEX_BASE_URL`    | optional override                    |
| `VERTEX_TRUST_ID`    | required by Vertex; defaults to "default" |

Vertex's full production setup uses cert-based auth + an OAuth flow with
quarterly cert renewal — that's out of scope for the loadgen, which
treats Vertex as a thin "I want a tax number for this transaction"
HTTP shim. Real Vertex calls happen inside the Aforo platform via the
platform's `TaxCalculationService`; the loadgen verifies the platform's
output independently.

When `VERTEX_OAUTH_TOKEN` is missing, the engine falls back to `mock`
with a `Note` explaining why.

## Per-currency jurisdictions

The mock engine resolves a (request, currency) pair into a jurisdiction:

```yaml
tax:
  engine: mock
  jurisdictions:
    US-CA: 0.0925
    EU-DE: 0.19
    UK:    0.20
  jurisdiction_by_currency:
    USD: US-CA
    EUR: EU-DE
    GBP: UK
  default_jurisdiction: US-CA
  tolerance_usd: 0.01
```

Resolution order:

1. `req.Jurisdiction` if non-empty
2. `jurisdiction_by_currency[req.Currency]`
3. `default_jurisdiction`

If still empty, the engine returns `Rate: 0` with a `Note` so the
validator can SKIP the row rather than fail it.

## Tolerance

Floating-point math at 6 decimal places means a 9.25% rate on $99.99
yields `9.249075` — the engine returns this, the platform might round
to two decimals (`9.25`). `tolerance_usd: 0.01` allows for one cent of
rounding noise; lower it (or raise it) per scenario.

## What the validator checks (Check 13)

For every (currency, jurisdiction) pair in the scenario:

1. Build the configured engine.
2. Compute `engine.Calculate(subtotal=100, currency)`.
3. Assert `|computed_tax − 100 × rate| ≤ tolerance_usd`.

This is a SHAPE check on the engine the loadgen will use — it does not
compare against the platform's recorded `tax_amount`. For that
comparison, run the validator with `--include-billing` against a live
backend (the live `BackendClient` reads invoice line items + their
`jurisdiction_code`).
