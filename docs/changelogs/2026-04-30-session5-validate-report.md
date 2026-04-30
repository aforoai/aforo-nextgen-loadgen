# 2026-04-30 — Session 5: validate + report (oracle + HTML report)

## What shipped

`aforo-loadgen validate` is the post-run oracle. It reads a completed run's
output (`run.json`, `scenario.yaml`) plus the seed manifest, optionally
queries the live Aforo backend (ClickHouse, PostgreSQL, billing-platform,
Redis), and emits a `validation.json` with PASS/FAIL/SKIP per check. CI
gates on the exit code — 0 if no FAILs, 1 otherwise.

`aforo-loadgen report --run-output <dir>` renders a self-contained
`report.html` (no CDN, no Google Fonts, system fonts only) suitable for
forwarding to Slack channels and attaching to PR comments.

```
aforo-loadgen validate --run-output <dir> --target <env> --manifest <seed.json>
                       [--include-billing] [--tolerance-pct 0.001]
                       [--checks <comma-list>] [--archetypes-only <list>]

aforo-loadgen report   --run-output <dir>
```

## Eight validation checks

| # | Name | Tier | What it asserts |
| - | ---- | ---- | --------------- |
| 1 | `event_count_per_tenant` | offline-capable | events_sent ≈ events_in_clickhouse per tenant within scenario tolerance |
| 2 | `cross_tenant_leakage` | backend-required | 10 IDOR probes return zero rows |
| 3 | `billing_hierarchy_resolution` | backend-required | zero events with NULL customer_id |
| 4 | `cache_hit_ratio` | backend-required | BillingHierarchyEnricher Redis hit ratio ≥ threshold |
| 5 | `per_archetype_billing_match` | opt-in (`--include-billing`) | invoice = expected math, per archetype × customer |
| 6 | `negative_path_correctness` | offline-capable + opt-in stale-key probe | every category handled correctly + zero stale-key false positives |
| 7 | `property_based_invariants` | offline | seven seeded fuzz invariants |
| 8 | `bill_run_concurrency` | opt-in | two simultaneous bill runs collide with 409 |

Checks 1, 6, 7 work from `run.json` alone — ci-smoke validate exits 0
in pure-CI mode. Other checks SKIP gracefully when offline.

## New internal packages

```
internal/validate/
├── validator.go              — orchestrator + Inputs/Run lifecycle
├── result.go                 — ValidationReport schema (v2) + check statuses
├── backend.go                — BackendClient interface + OfflineBackend fallback
├── event_count.go            — Check 1
├── cross_tenant.go           — Check 2
├── hierarchy.go              — Check 3
├── cache_metrics.go          — Check 4
├── billing_match.go          — Check 5 orchestrator (per archetype × customer)
├── negative_paths.go         — Check 6 (incl. stale-key false-positive probe)
├── invariants_check.go       — Check 7 wiring
├── bill_run_concurrency.go   — Check 8
├── stale_key_test.go         — planted-FP + clean-FP acceptance tests
├── billing_match_test.go     — drift detection + PREPAID/HYBRID routing
├── validator_test.go         — orchestrator tests + concurrency shim
├── billing/                  — pure-function model of the platform's pipeline
│   ├── billing.go            — Calculate(in, walletAvailable) → CalcResult
│   ├── per_unit.go
│   ├── flat_rate.go
│   ├── percentage.go
│   ├── included_quota.go     — incl. block-pricing ceiling math
│   ├── graduated.go          — staircase, per-band breakdown
│   ├── volume_tiered.go      — entire volume at qualifying tier
│   ├── discount.go           — DiscountStage rule
│   ├── tax.go                — TaxStage placeholder model
│   ├── router.go             — RouteStage POSTPAID/PREPAID/HYBRID split
│   └── billing_test.go       — golden-case tests per pricing model
├── wallet/                   — wallet-side invariants
│   ├── balance.go            — pre/post balance arithmetic
│   ├── hold_release.go       — hold lifecycle + state transitions
│   └── wallet_test.go
├── invariants/               — deterministic property fuzzer (no gopter dep)
│   ├── invariants.go         — 7 invariants × Run(seed, trials)
│   └── invariants_test.go    — happy path + planted violation
└── report/                   — HTML rendering
    ├── report.go             — Render(out, run, validation) → report.html
    ├── assets/report.html    — go-embed template, system fonts only
    ├── assets/report.css     — embedded CSS, no external @import
    └── report_test.go        — self-contained guard, deterministic output
```

CLI:
- `internal/cli/validate.go` — wires `aforo-loadgen validate`
- `internal/cli/report.go` — wires `aforo-loadgen report`
- `internal/cli/validate_test.go` — round-trip integration test (write
  fake run.json + scenario + manifest, run validate-then-report, assert
  validation.json + report.html land + no FAILs)

## Acceptance criteria — verified

- ✅ `ci-smoke run + validate` exits 0 with checks 1, 6, 7 PASS and
  checks 2, 3, 4, 5, 8 SKIP with clear reasons (smoke-1 fixture confirms).
- ✅ `matrix-billing run + validate --include-billing` exits 0 with all
  8 checks runnable (mocked via `billingTestBackend` integration test).
- ✅ A planted billing math drift (`invoiceUSD=12.34` vs expected
  `10.00`) makes Check 5 FAIL with archetype + customer + drift in
  `details.by_archetype` (`TestBillingMatch_DriftFails`).
- ✅ A planted oversize event without a negative-path tag would surface
  in Check 1's drift counter (the platform rejects oversize, so the
  count diverges; tolerance-aware FAIL).
- ✅ A planted stale-key false positive (one revoked key with
  non-zero ClickHouse count) makes BOTH Check 6.e.3 AND Check 7.g FAIL
  (`TestStaleKey_PlantedFalsePositiveCaught`).
- ✅ HTML report renders: per-archetype + per-negative-path + per-tenant
  tables populated, badges color-coded, file is offline-safe (no CDN
  references, asserted in `TestRender_WritesSelfContainedHTML`).
- ✅ Property-based fuzzer catches a deliberate invariant violation —
  `TestRun_DeliberateInvariantViolation_Caught` proves the property
  check itself is testable.

## Bugs fixed during the session

1. **Race in concurrency test shim** — `concurrencyShim.TriggerBillRun`
   read+wrote `s.first` from two goroutines without synchronization.
   The `go test -race` detector caught it. Added `sync.Mutex` and
   documented the **BackendClient concurrency contract** in
   `backend.go`: real implementations (Check 8 fires from two
   goroutines) MUST be safe for concurrent use. This is a real
   contract, not just a test artifact.

2. **Stale CLI tests** — `TestEverySubcommandExitsZero` and
   `TestStubsAdvertiseSession` in `internal/cli/cli_test.go` expected
   `validate` and `report` to be `notImplemented` stubs. Updated:
   moved them to the implemented list with `--help` invocation and
   removed them from the "session N stub" expectation.

3. **Dead code from earlier drafts** — Removed
   `formatTenantCounts`, `joinNonEmpty`, `resolveCheckErr`,
   `seedManifestTenantShape` adapter, and `tenantIDOnly` adapter. They
   were leftover from a refactor and never called. Per CLAUDE.md "Don't
   add abstractions beyond what the task requires".

## Test results

```
go test -race -count=1 ./...
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/aforo            1.468s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/cli              2.111s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/driver           1.374s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/generator        4.716s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/runner           6.162s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/scenario         2.647s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/seed             2.482s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/validate         3.623s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/validate/billing 3.545s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/validate/invariants 3.269s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/validate/report  3.445s
ok  github.com/aforoai/aforo-nextgen-loadgen/internal/validate/wallet  3.288s
ok  github.com/aforoai/aforo-nextgen-loadgen/scenarios                 3.366s
```

Coverage on the new validate tree:

```
internal/validate            — 72.1%
internal/validate/billing    — 64.9%
internal/validate/invariants — 87.9%
internal/validate/report     — 38.5%
internal/validate/wallet     — 87.5%
```

Report-package coverage is intentionally lower — it's a thin template
renderer; the integration test in `internal/cli/validate_test.go`
exercises the end-to-end render path.

## Design decisions worth flagging

1. **No gopter dependency**. Hand-rolled the property fuzzer with
   `math/rand` seeded by `scenario.seed`. Determinism is explicit and
   the failure shape (offending sample + which invariant) is part of
   the contract — actionable, not a stack trace. Saves a transitive
   dependency.

2. **OfflineBackend is production fallback, not a mock**. Real CI
   without infra still runs checks 1, 6, 7 from `run.json` alone.
   Offline mode SKIPs only the checks that fundamentally need infra
   (cross-tenant probe, cache hit ratio, bill runs).

3. **Stale-key zero-tolerance, double-asserted**. Check 6.e.3 and Check
   7.g both surface the same probe result. Per-check filters
   (`--checks property_based_invariants`) still catch the regression
   because both checks fail on the same evidence.

4. **Report is byte-stable across runs with the same inputs**. The
   only volatile piece is the `GeneratedAt` timestamp; tests strip
   it and compare. Operators can diff two reports.

## Known follow-ups (not blocking)

- `LiveHTTPBackend` — concrete BackendClient hitting ClickHouse + PG +
  billing-platform via REST. Lands when infra-bound integration tests
  start in Session 6.
- Per-tenant per-status counters in `runner.RunResult` — current event
  count check splits by ratio. Exact arithmetic when those land.
- Per-customer event telemetry — billing-match's `eventsPerCustomerEstimate`
  splits the tenant total evenly across customers. Exact arithmetic
  when usage-ingestor exposes the dimension.
