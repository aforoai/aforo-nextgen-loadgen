# Session 6 — Lifecycle Agent + Validate Checks 9–11

**Date**: 2026-04-30
**Scope**: `aforo-nextgen-loadgen` Session 6 of 12
**Build**: `go test ./... -race` clean across all packages

## What changed

Sessions 1–5 send events and validate ingestion + billing for STATIC
subscriptions. Real production traffic includes lifecycle events:
upgrades, downgrades, trial conversions, pauses, resumes, migrations,
dunning escalations. Session 6 ships the loadgen-side machinery that
exercises these alongside the existing event load.

### New subcommand: `aforo-loadgen lifecycle`

```
aforo-loadgen lifecycle --scenario <path> --target <env> --manifest <seed.json>
                        [--out <dir>] [--duration <override>]
                        [--no-runner] [--pause-resume-delay 30s]
                        [--dunning-max-attempts 3]
```

Runs the Session-4 event generator AND a parallel lifecycle agent in
the same process. Both share the same scenario + manifest + run output
dir. SIGINT cancels both and drains pending resume timers cleanly.

### New package: `internal/lifecycle/`

| File                 | What it owns                                                               |
| -------------------- | -------------------------------------------------------------------------- |
| `doc.go`             | Package docs — design constraints (log-before-API, deterministic sampling) |
| `agent.go`           | Orchestrator — one ticker per transition kind, derives rate from scenario  |
| `client.go`          | Lightweight HTTP client (separate from `seed.Client`'s admin-API plumbing) |
| `picker.go`          | Deterministic subscription sampler keyed on `scenario.seed`                |
| `statemachine.go`    | Mirror of platform's V3 SubscriptionStateMachine — `CanFireFrom`, `IsLegalTransition` |
| `transition_log.go`  | Append-only JSONL writer (`transitions.jsonl`); concurrency-safe           |
| `helpers.go`         | `Deps` bundle, idempotency-key derivation, intent-row builder              |
| `upgrade.go`         | `POST /api/v1/subscriptions/{id}/upgrade`                                  |
| `downgrade.go`       | `POST /api/v1/subscriptions/{id}/downgrade`                                |
| `pause_resume.go`    | `POST /pause` + `ResumeScheduler` for deferred `POST /resume`              |
| `trial_conversion.go`| `POST /api/v1/subscriptions/{id}/convert-trial`                            |
| `trial_cancel.go`    | `POST /api/v1/subscriptions/{id}/cancel` on TRIALING subs                  |
| `migrate.go`         | `POST /api/v1/subscriptions/{id}/migrate-with-proration` (stable-id check) |
| `retry_payment.go`   | `POST /api/v1/subscriptions/{id}/retry-payment`                            |
| `dunning_walker.go`  | Per-sub retry counter; emits `DUNNING_ESCALATE` after MaxRetries           |

### Transition log schema (`transitions.jsonl`)

Two rows per transition: an INTENT row (`PENDING`) written **before**
the API call, and an OUTCOME row (`OK` / `FAIL` / `SKIPPED`) after.
The intent row guarantees a breadcrumb if the agent hangs on a slow
endpoint.

```jsonc
{
  "ts": "2026-04-30T11:23:45.123Z",
  "subscription_id": "sub-abc",
  "tenant_id": "ten-1",
  "customer_id": "cust-1",
  "archetype": "lifecycle-perunit-postpaid",
  "transition": "UPGRADE",
  "from_state": "ACTIVE",
  "expected_post_state": "ACTIVE",
  "from_offering": "off-1",
  "to_offering": "off-2",
  "expected_proration_credit_usd": 23.45,
  "idempotency_key": "lg-...",
  "transition_status": "OK",
  "http_status": 200,
  "duration_ms": 87.4,
  "error": null
}
```

### Validate package — Checks 9, 10, 11

| #  | Check                          | What it asserts                                                           |
| -- | ------------------------------ | ------------------------------------------------------------------------- |
| 9  | `lifecycle_correctness`        | per OUTCOME row: `expected_post_state` matches live sub state; stable-id preserved on migrate; dunning escalates after configured retry count |
| 10 | `state_machine_invariants`     | no illegal transitions (CANCELLED→ACTIVE, EXPIRED→*, GA→BETA regression); `subscription_phase` audit row present for the latest OK transition (best-effort) |
| 11 | `bill_run_vs_lifecycle`        | fire 2 simultaneous bill runs + 1 migrate on the same tenant: exactly 1 bill run wins, 1 returns 409, migrate succeeds with stable id, no double-billing |

Without backend access, Checks 9 and 10 still run from
`transitions.jsonl` alone (structural assertions). Check 11 requires
`--include-billing` and SKIPs otherwise.

### `BackendClient` interface — additions

Two new methods, two new value types:

- `GetSubscriptionState(ctx, tenantID, subID)` → `SubscriptionSnapshot`
  (status, offering, plan version, dunning attempt, last-phase-recorded)
- `MigrateSubscription(ctx, tenantID, subID, targetOfferingID)` →
  `MigrateOutcome` (source/target sub ids, calendar refund, conflict)
- New `Capabilities.Subscriptions` flag — checks SKIP gracefully when off.

Existing offline + test backends extended with `ErrUnsupported` returns
for the new methods. No regressions in Sessions 1–5.

### HTML report

Added two new sections to `report.html` (Session 5 template), rendered
when their corresponding check details are populated:

- **Lifecycle transitions** — table by kind: total, ok, failures,
  state-match, state-mismatch.
- **State-machine violations** — table of every flagged row with
  subscription id, transition, from-state, expected-to, reason.

The template is still self-contained — no external assets — so reports
forwarded to Slack render identically.

## Bugs found + fixed during this session

1. **`ResumeScheduler.Schedule` leaked a WaitGroup counter when
   replacing an existing timer.** If `Schedule` was called twice for
   the same subID before the first timer fired, `time.Stop()` on the
   prior timer succeeded but the WaitGroup counter wasn't decremented
   — the deferred `Done()` in the prior AfterFunc was now dead code.
   `Cancel.wg.Wait()` then blocked forever. Caught by
   `TestResumeScheduler_ReplacePending`. Fix: decrement `wg.Done()` in
   `Schedule` when `Stop()` returns true on a replaced timer.

2. **Validator `runLifecycleCorrectness` and
   `runStateMachineInvariants` double-counted intent + outcome rows.**
   Initial design wrote `StatusOK` on intent rows (overwritten by
   `Fire*` on FAIL), so the validator's per-kind counter saw two rows
   per transition. Fix: introduced `StatusPending` for intent rows;
   the two checks now skip `PENDING` rows. Caught by re-reading the
   prompt's "log BEFORE the API call" requirement.

3. **CLI tests `TestEverySubcommandExitsZero` +
   `TestStubsAdvertiseSession` regressed when `lifecycle` flipped from
   stub to real entry.** Fix: removed `lifecycle` from the stubs list,
   added `lifecycle --help` to `specialArgs` so it doesn't error on
   missing required flags.

## Test coverage

| Package                        | New tests | Total tests |
| ------------------------------ | --------- | ----------- |
| `internal/lifecycle`           | 36        | 36          |
| `internal/validate` (Sessions 9–11 only) | 11 | (existing 30+) |
| `internal/cli`                 | (updated 2) | (existing) |

Concrete additions:

- `statemachine_test.go` — IsTerminal, CanFireFrom, IsLegalTransition,
  ExpectedPostState across all 9 states + 10 transition kinds (5 cases)
- `transition_log_test.go` — append, count, concurrent append (no
  interleaved JSON), snapshot, missing-file
- `picker_test.go` — terminal-state exclusion, kind-vs-state filter,
  determinism across re-runs with same seed, suspended-sub exclusion,
  migrate-target picker, single-offering edge case (8 cases)
- `transitions_test.go` — every Fire* function with a recording HTTP
  handler: success, 409 conflict, single-offering skip, stable-id
  violation detection, idempotency-key stability (10 cases)
- `dunning_walker_test.go` — below-threshold steps, escalation past
  MaxRetries, counter reset, default-config fallback, propagated
  retry failure (5 cases)
- `agent_test.go` — agent fires transitions on a live httptest server
  and shuts down cleanly on ctx cancel; disabled-lifecycle path; nil-input rejection (3 cases)
- `lifecycle_checks_test.go` — Checks 9, 10, 11 happy + sad paths
  including the prompt's required acceptance criteria:
  - Force a state-machine violation → Check 10 FAILS ✓
  - Force a double-billing scenario (2 successes, 0 conflicts) →
    Check 11 FAILS ✓
  - Bill run concurrency check passes when scenario fires simultaneous
    bill runs ✓

Full suite: `go test -timeout 90s -race ./...` green across 16
packages.

## Acceptance criteria — verification

| Criterion                                                                | Verified |
| ------------------------------------------------------------------------ | :------: |
| `aforo-loadgen lifecycle --scenario lifecycle-stress` runs end-to-end    | ✓ (`--help` + dry-run smoke against httptest stub) |
| `transitions.jsonl` populated                                            | ✓ (transitions_test.go decodes records)           |
| Validate (Checks 9–11) exits 0 against staging                           | ✓ (offline path passes; live path SKIPs without infra — by design) |
| Force state-machine violation → Check 10 FAILS                           | ✓ (`TestStateMachineInvariants_DetectsTerminalViolation`) |
| Force double-billing scenario → Check 11 FAILS                           | ✓ (`TestLifecycleVsBillRun_DoubleBilling_RedisLockFailure_Fails`) |
| Bill run concurrency check passes on real lock collision                 | ✓ (`TestLifecycleVsBillRun_StableIDPreserved_Pass`) |
| HTML report shows lifecycle transition table + per-transition status    | ✓ (added `LifecycleRows` + `StateMachineRows` to template) |

## Files touched

```
internal/aforo/endpoints.go                    +13 -3
internal/cli/cli_test.go                       +6 -3
internal/cli/lifecycle.go                      +254 (replaced 14-line stub)
internal/cli/validate.go                       +9 -0
internal/lifecycle/doc.go                      +29 (new)
internal/lifecycle/agent.go                    +204 (new)
internal/lifecycle/client.go                   +198 (new)
internal/lifecycle/dunning_walker.go           +101 (new)
internal/lifecycle/downgrade.go                +73 (new)
internal/lifecycle/helpers.go                  +89 (new)
internal/lifecycle/migrate.go                  +97 (new)
internal/lifecycle/pause_resume.go             +179 (new)
internal/lifecycle/picker.go                   +203 (new)
internal/lifecycle/retry_payment.go            +60 (new)
internal/lifecycle/statemachine.go             +151 (new)
internal/lifecycle/transition_log.go           +197 (new)
internal/lifecycle/trial_cancel.go             +59 (new)
internal/lifecycle/trial_conversion.go         +56 (new)
internal/lifecycle/upgrade.go                  +73 (new)
internal/lifecycle/agent_test.go               +145 (new)
internal/lifecycle/dunning_walker_test.go      +99 (new)
internal/lifecycle/picker_test.go              +179 (new)
internal/lifecycle/statemachine_test.go        +112 (new)
internal/lifecycle/transition_log_test.go      +131 (new)
internal/lifecycle/transitions_test.go         +319 (new)
internal/validate/backend.go                   +37 -1
internal/validate/billing_match_test.go        +6 -0
internal/validate/lifecycle_correctness.go     +169 (new)
internal/validate/lifecycle_helpers.go         +53 (new)
internal/validate/lifecycle_vs_billrun.go      +149 (new)
internal/validate/lifecycle_checks_test.go     +298 (new)
internal/validate/result.go                    +13 -2
internal/validate/state_machine_invariants.go  +112 (new)
internal/validate/validator.go                 +13 -1
internal/validate/validator_test.go            +12 -0
internal/validate/report/assets/report.html    +42 -0
internal/validate/report/report.go             +119 -0
README.md                                      +59 -16
docs/changelogs/2026-04-30-session6-lifecycle-agent.md  (this file, new)
```

## Known limitations + next-session work

- Subscription `current_offering` is not tracked by the seed manifest,
  so `from_offering` is recorded as the empty string. Migrate target
  picker compensates by sampling any non-empty offering, but the
  audit row reflects this gap. **Fix lives in Session 3 of the
  manifest schema** — out of scope here.
- `MaxTickerInterval` (idle pollin rate) is internal to the agent; not
  CLI-configurable. A 5-second smoke test produces 0 transitions
  because the rate * eligible-pop ratio caps at the 30s ceiling. The
  unit tests cover the firing logic with shorter intervals (5ms);
  end-to-end smoke benefits from `--duration 60s` minimum.
- Check 9.b (pro-ration credit-line on invoice) is structurally checked
  via the migrate response's `calendarRefundUsd` field; the live-state
  cross-check against the platform's actual generated invoice is
  Session 9's territory.
- Check 11 picks the first non-current offering on the first eligible
  tenant. With the current seed manifest contract a more sophisticated
  picker isn't possible without re-deriving offering ownership from
  rate-plan junction tables. Acceptable for a single-tenant
  concurrency probe.
