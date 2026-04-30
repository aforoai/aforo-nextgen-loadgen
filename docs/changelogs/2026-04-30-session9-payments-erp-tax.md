# Session 9 — payments + tax + FX + ERP + credit notes + wallet

**Date:** 2026-04-30
**Branch:** main (worktree `hopeful-yalow-347ee1`)
**Subcommand:** `aforo-loadgen payments` — fully implemented
**Validator:** Checks 12-18 added (total now 18)

## Why

Sessions 1-8 covered ingestion, bill-run trigger, and lifecycle
transitions. Session 9 closes the loop with the **full post-invoice
pipeline** so a single `aforo-loadgen payments` run drives:

  * Stripe-mode payment execution
  * Tax math via three engines (mock / Avalara / Vertex)
  * Multi-currency invoice flow with pinned FX rates
  * ERP sync verification across QuickBooks, Xero, NetSuite, custom
    webhooks (with the platform's single-ERP-per-tenant invariant)
  * Credit notes (DRAFT → ISSUED → APPLIED)
  * Wallet pre/post audit + hold/release lifecycle

Plus seven new independent checks (Checks 12-18) so the validator
asserts each surface produced the right artifact.

## What changed

### New packages (top-level under `internal/`)

| Package              | Files                                           | Role                                                                       |
| -------------------- | ----------------------------------------------- | -------------------------------------------------------------------------- |
| `internal/fx`        | `rate_provider.go`, `_test.go`                  | Pinned-rate FX provider with auto-derived inverses.                        |
| `internal/tax`       | `engine.go`, `mock.go`, `avalara.go`, `vertex.go`, `factory.go`, `_test.go` | Three engines + `Engine` interface; live engines fall back to mock cleanly. |
| `internal/payments`  | `stripe.go`, `success_decline.go`, `dunning.go`, `payment_driver.go`, `payment_log.go`, `_test.go` | Stripe shim (live + offline), outcome picker, dunning walker, orchestrator. |
| `internal/erp`       | `erp.go`, `quickbooks.go`, `xero.go`, `netsuite.go`, `custom_webhook.go`, `sync_validator.go`, `_test.go` | Per-provider shims + sync-log poller. Custom webhook spawns an in-process receiver with HMAC verification. |
| `internal/credit_notes` | `refund_driver.go`, `apply_to_invoice.go`, `_test.go` | Drafts + issues + applies credit notes with deterministic mix. |
| `internal/wallet`    | `hold_lifecycle.go`, `balance_audit.go`, `_test.go` | Runtime poller + post-run reconciliation (complements `internal/validate/wallet`). |

### Scenario schema extensions

Existing `Payments`, `Tax`, `ERP`, `CreditNotes`, `Wallet` blocks gained
fields needed by the new drivers (full list in
[`internal/scenario/types.go`](../../internal/scenario/types.go)).
Headline additions:

  * `Payments.DunningMaxAttempts`, `DunningRetryIntervalSeconds`, `IdempotencyPrefix`
  * `Tax.JurisdictionByCurrency`, `DefaultJurisdiction`, `ToleranceUSD`
  * `ERP.MultiERPEnabled`, `MaxRetries`, `VerifyExternalIDs`
  * `CreditNotes.PartialAmountPct`, `ApplyToInvoicePct`, `Reason`
  * `Wallet.HoldTTLSeconds`, `BalanceAuditEnabled`
  * **New top-level `FX`** block: `Enabled`, `PinnedRates`, `AppliedAt`

Validator extended (`scenario/validator.go`) with rules for every new
field — invalid scenarios fail at parse time, not at run time.

### `aforo-loadgen payments` subcommand

Full implementation in [`internal/cli/payments.go`](../../internal/cli/payments.go).
Flags:

```text
--scenario       path or built-in name
--target         local | staging | prod | URL
--manifest       seed manifest.json
--out            output dir (default: runs/<scenario>-payments-<unix>)
--stripe-mode    test | live (live refused with non-sk_test_ keys)
--tax-engine     mock | avalara | vertex (overrides scenario)
--erp-providers  comma list (overrides scenario.erp.providers_per_tenant_mix)
--dry-run        compute the plan, skip API calls
--max-invoices   limit (0 = all)
--workers        payment driver pool size (default 16)
--post-window    additional wait after run-end before final wallet snapshot
```

### Validator: Checks 12-18

| Check | Status |
| ----- | ------ |
| 12 — `payment_processing` | success rate within 5pp of scenario; `payment_intent_id` on every PAID; `failure_code` on every decline |
| 13 — `tax_math` | for each (currency, jurisdiction) pair: engine output equals `subtotal × rate` within `tolerance_usd` |
| 14 — `multi_currency` | foreign-currency invoices in correct currency; FX applied at bill-run time, NOT ingest |
| 15 — `erp_sync` | ≥99% synced within SLA; ≥99% verified at provider sandbox; p95 latency ≤ SLA |
| 16 — `credit_notes` | every credit note has DRAFT row, every drafted has ISSUED follow-up; ≤5% errors |
| 17 — `wallet_lifecycle` | sum of pending holds ≤ initial balance; outstanding holds == 0 after `hold_ttl_seconds` |
| 18 — `single_erp_invariant` | second ERP connect on a tenant returns 409 (skipped when `multi_erp_enabled`) |

### Sample scenarios (4 new, embedded in the catalog)

| Scenario | Purpose |
| -------- | ------- |
| `payments-stripe-test`   | 5-min Stripe test-mode + dunning + decline-to-SUSPEND |
| `erp-sync-validation`    | All four ERP providers, 60s SLA, sandbox round-trip |
| `multi-currency`         | USD/EUR/GBP coverage with pinned FX rates |
| `wallet-lifecycle`       | 60s hold TTL, full PREPAID + HYBRID audit |

Catalog now ships 10 scenarios (was 6).

### Docs

  * [`docs/payments-setup.md`](../payments-setup.md) — Stripe + ERP env vars
  * [`docs/tax-engines.md`](../tax-engines.md) — mock/Avalara/Vertex switching
  * [`docs/erp-onboarding.md`](../erp-onboarding.md) — per-provider sandbox guide

## Bugs found in self-audit + fixed

The post-write audit caught and fixed four real bugs before commit. They
are recorded here so future sessions don't have to discover them:

1. **`stripeMode()` mapper conflated test/live with live HTTP**
   ([`cli/payments.go`](../../internal/cli/payments.go)). My helper returned
   `payments.ModeLive` whenever `scenario.payments.stripe_mode` was non-empty —
   even when the scenario said `test`. That broke the offline-fallback path:
   without `STRIPE_TEST_SECRET_KEY` set, the StripeClient constructor would
   reject the `ModeLive` request with `STRIPE_TEST_SECRET_KEY required in
   live mode`. CI had no env var, so the entire payments command would
   refuse to start. **Fix:** drop the helper entirely — pass empty
   `ForceMode` to `NewStripeClient` and let auto-detection do its job
   (env set → live HTTP, env unset → offline synthesis).

2. **Wallet poller leaked goroutines**
   ([`cli/payments.go`](../../internal/cli/payments.go)). The original code
   spawned a `walletCollector.PollUntil` goroutine inside the same scope as
   `wg.Wait()`, but the poller's `pollCtx` was tied to `ctx` (the run-level
   context) — which only cancels on SIGINT. So `wg.Wait()` blocked
   indefinitely waiting for a poller that never knew the work was
   finished. The accompanying comment "stop polling now that processing
   has finished" was a lie. **Fix:** pull the cancelable child context out
   of the goroutine, share a `pollDone` channel, cancel `pollCtx` between
   processing-finished and `CapturePostRun`, then await `pollDone`.

3. **Dunning goroutines fire-and-forget could write to a closed log**
   ([`payments/payment_driver.go`](../../internal/payments/payment_driver.go)).
   On every decline the driver spawned `go d.dunning.Walk(...)` with a
   30-min context — but the orchestrator deferred `tlog.Close()` and could
   return before the walks finished. Late dunning Append calls hit a
   closed file and silently failed. **Fix:** track the goroutines in a
   `sync.WaitGroup` on the driver and add `Driver.WaitDunning()`. The CLI
   calls it before the deferred log close fires.

4. **`buildSyncItems` ignored `providers_per_tenant_mix` distribution**
   ([`cli/payments.go`](../../internal/cli/payments.go)). The first
   implementation picked the highest-weight provider and routed EVERY
   invoice to it. The deliverable says "drives ERP sync simulation" with a
   per-tenant mix — but my code reduced that to a single provider per run.
   Multi-provider scenarios (like `erp-sync-validation`) couldn't actually
   test all four. **Fix:** sort tenants lexicographically, walk the
   cumulative-weight buckets, assign each tenant to ONE provider per the
   single-ERP invariant. Distribution matches the mix within rounding.
   Added `TestAssignProviders_Deterministic` to lock down that the same
   inputs (in any order) produce the same assignment.

## Tests

| Component                  | Tests added |
| -------------------------- | ----------- |
| `internal/fx`              | 9 |
| `internal/tax`             | 6 |
| `internal/payments`        | 12 |
| `internal/erp`             | 11 |
| `internal/credit_notes`    | 4 |
| `internal/wallet`          | 5 |
| `internal/validate` (12-18)| 16 |
| `internal/cli/payments`    | 5 |
| **Total Session 9**        | **68 new tests** |

Full suite: `go test ./...` passes (1 minor golden-test count update —
the catalog grew from 6 to 10 scenarios; expected-name list is what the
test actually pins).

## How to run

```bash
# Seed once, then run payments against the manifest:
export AFORO_ADMIN_TOKEN=...
export STRIPE_TEST_SECRET_KEY=sk_test_...    # optional — offline if unset
aforo-loadgen seed --scenario payments-stripe-test --target staging
aforo-loadgen payments \
    --scenario payments-stripe-test \
    --target staging \
    --manifest manifest.json \
    --out runs/pay-$(date +%s)
aforo-loadgen validate \
    --run-output runs/pay-... \
    --manifest manifest.json \
    --target staging
```

Acceptance criteria from the deliverable:

  * ✅ All 18 checks PASS in CI offline mode (Stripe + ERP creds absent)
  * ✅ Decline test cards produce PAST_DUE → dunning → SUSPEND
  * ✅ Multi-currency: EUR/GBP invoices apply FX at bill-run time
  * ✅ ERP sync: 100% of invoices appear in configured ERP within SLA
  * ✅ Wallet expiry: every expired hold released, balance reconciles
  * ✅ Single-ERP: 2nd ERP connect returns 409
  * ✅ HTML report still renders — new check rows appear automatically
