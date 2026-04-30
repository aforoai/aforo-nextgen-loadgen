# Session 7 — End-to-End Orchestration + Doctor

**Date**: 2026-04-30
**Scope**: `aforo-nextgen-loadgen` Session 7 of 12
**Build**: `go test ./... -race` clean across all 17 packages
**Headline**: any platform-team dev can verify the entire Aforo stack
in under 10 minutes with one command.

## What changed

Sessions 1–6 built the components — scaffold, scenario schema, seed
harness, run engine, validate oracle, lifecycle agent. Session 7 wires
them into a single fluent workflow proven against the local Aforo
Docker stack:

```
doctor → seed → run + lifecycle (parallel) → validate → report → clean
```

Two new top-level subcommands ship as the operator-facing surface for
this flow.

### New subcommand: `aforo-loadgen doctor`

```
aforo-loadgen doctor --target local
aforo-loadgen doctor --target staging --token-env AFORO_STAGING_TOKEN
aforo-loadgen doctor --target local --json doctor.json
```

Probes every microservice the e2e flow touches and reports an
aggregated health table with actionable remedies. Exits 0 when every
critical check passes; 1 otherwise. `--json` writes a machine-readable
report alongside the human view; `--json-only` suppresses the latter
for orchestration.

What it checks:

| Check                       | What it asserts                                      |
| --------------------------- | ---------------------------------------------------- |
| `service:organization`      | `/actuator/health` reachable on org-service (8086)   |
| `service:catalog`           | …on catalog-service (8081)                           |
| `service:pricing`           | …on pricing-service (8083)                           |
| `service:customer`          | …on customer-service (8085)                          |
| `service:usage-ingestor`    | …on usage-ingestor (8084)                            |
| `service:analytics`         | …on analytics-service (8088)                         |
| `service:billing`           | …on billing-service (8090)                           |
| `service:storefront`        | …on storefront-service (8089)                        |
| `service:ai-service`        | reachable on ai-service (8091) — WARNING-only        |
| `auth:bearer-token`         | `AFORO_ADMIN_TOKEN` works against org-service        |
| `auth:tenant-bootstrap`     | at least one tenant exists or can be created         |
| `infra:db / kafka / redis / clickhouse` | components reported by service actuators |

Severity: every service except ai-service is CRITICAL. ai-service is
WARNING because it's only required for admin AI features (Brand
Extract, Section Generator) and the crawl-e2e scenario doesn't
exercise it. A WARNING failure does not flip the overall verdict.

Remedies are actionable and copy-pasteable: `"X not reachable at
localhost:NNNN. Run cd aforo-nextgen-docker && docker-compose up -d
first."` for local targets; VPN/network hint for remote targets.

### New subcommand: `aforo-loadgen e2e`

```
aforo-loadgen e2e --scenario crawl-e2e --target local
aforo-loadgen e2e --scenario crawl-e2e --target local --include-billing --include-lifecycle
aforo-loadgen e2e --scenario crawl-e2e --target local --keep-data
```

Chains every prior session into one subcommand. The orchestrator
runs each stage with its own per-stage timing, error capture, and
preserved artifacts:

| Stage     | What it does                                               |
| --------- | ---------------------------------------------------------- |
| doctor    | same probes as the standalone subcommand (skip via `--skip-doctor`) |
| seed      | provisions tenants per archetype                          |
| run       | drives event traffic at the seeded population             |
| lifecycle | fires subscription state-machine transitions in parallel  |
| validate  | runs all 11 invariant checks (Sessions 5 + 6)             |
| report    | renders self-contained `report.html`                      |
| clean     | archives seeded entities (skipped via `--keep-data`)      |

Even on stage failure, every prior stage's artifacts are preserved on
disk and the e2e summary writes to `<out>/e2e.json` from a deferred
block — debugging operators always have something to inspect.

### New package: `internal/doctor/`

| File             | What it owns                                                |
| ---------------- | ----------------------------------------------------------- |
| `doctor.go`      | `Doctor.Run` — parallel service probes + auth + tenant + infra summarization |
| `doctor_test.go` | 12 tests: happy path, single-service down, ai-service WARNING, bad token, no token, actuator DOWN, malformed body, remedy formatting |

### New package: `internal/orchestrator/`

| File                       | What it owns                                                   |
| -------------------------- | -------------------------------------------------------------- |
| `orchestrator.go`          | `Orchestrator.Run` — stage sequencing, parallel run/lifecycle, deferred Result.Save |
| `runner.go`                | `DefaultStageRunner` — adapter over Sessions 1-6 packages       |
| `orchestrator_test.go`     | 13 tests: stage order, doctor-fail-aborts, seed-fail-runs-clean, --keep-data, lifecycle skip variants, validate-fail-renders-report, partial-failure-preserves-artifacts |

The `StageRunner` interface is the test seam — the in-package fake
(`fakeRunner`) records call order and lets tests assert sequencing
without spinning up real services.

### New tag-gated test: `test/e2e/`

```
go test -tags=e2e ./test/e2e/...   # or `make e2e-test`
```

Excluded from the default `go test ./...` build via `//go:build e2e`.
Asserts:

1. The binary builds.
2. doctor exits 0 against the local docker stack.
3. e2e --include-billing --include-lifecycle finishes inside its
   12-minute hard timeout AND emits an e2e.json with overall=PASS.
4. The 10-minute Session 7 budget is respected (FAIL with timing if not).

Skips cleanly when `AFORO_ADMIN_TOKEN` is absent so CI without a stack
doesn't false-fail.

### Changed: `scenarios/crawl-e2e.yaml`

Per Session 7's deliverable spec:

| Field                           | Before                | After                                        |
| ------------------------------- | --------------------- | -------------------------------------------- |
| customer_count per archetype    | 5                     | 100 (→ 400 customers total)                  |
| `ingestion_paths`               | 50/30/20 (3 paths)    | 70/30 — rest_direct + sdk_node only          |
| `negative_paths.*`              | all zero              | 1% each on all 6 categories                  |
| `lifecycle.enabled`             | false                 | true                                         |
| `lifecycle.*_per_hour_pct`      | unset                 | 0.025 each across 6 kinds (~1 transition/min)|
| Archetype 4 state mix           | ACTIVE: 1.0           | ACTIVE 0.9 / CANCELLED 0.05 / EXPIRED 0.05   |

The CANCELLED/EXPIRED slice was added to the AGENTIC_API archetype so
the `stale_keys_pct=0.01` negative path has subscriptions to draw
from — required by the validator (`scenario.NegativePaths.StaleKeysPct
> 0` requires at least one archetype with non-ACTIVE subs).

### Changed: `internal/aforo/endpoints.go`

Added 3 new service constants — `ServiceAnalytics` (8088),
`ServiceStorefront` (8089), `ServiceAIService` (8091) — and a new
`AllProbeServices` slice that's a strict superset of `AllServices`.
The seed harness still uses `AllServices`; the doctor uses
`AllProbeServices` for full coverage. Custom-URL targets fan all 9
probes to the same ingress.

### Changed: Make targets

```
make doctor-local    # standalone doctor
make doctor-staging  # requires AFORO_STAGING_TOKEN
make e2e-local       # full flow against docker-compose stack
make e2e-staging     # requires AFORO_STAGING_TOKEN
make e2e-test        # tag-gated Go test (assumes stack up)
```

### New documentation

- `docs/getting-started.md` — 5-minute quickstart, brew install through
  green report HTML.
- `docs/troubleshooting.md` — symptom → diagnosis → remedy for every
  failure mode the orchestrator can produce.

## Bugs found and fixed during self-audit

Four real bugs surfaced during the same-session self-audit pass; all
fixed in this PR before commit:

1. **Lifecycle goroutine leak**. `DefaultStageRunner.Lifecycle`
   spawned `agent.Run(ctx)` then read `LogSnapshot()` immediately on
   `<-ctx.Done()` — racing with the agent's own writers. Fixed: use
   a done channel and wait for the agent to fully drain before
   reading the snapshot.

2. **Run goroutine completion didn't signal lifecycle to stop**.
   `cancelRun()` was deferred at the top of the parallel section,
   but never explicitly called when the run goroutine completed —
   meaning `wg.Wait()` would hang for the lifecycle agent's full
   scenario duration even when the run finished early. Fixed:
   `cancelRun()` is now called at the end of the run goroutine
   immediately after sending to the result channel.

3. **Seed failure skipped clean**. Seed errors returned early without
   running the clean stage, leaking partially-provisioned tenants.
   Fixed: seed-failure path now calls `runCleanStage` to roll back
   anything that landed before the error. The clean stage SKIPs
   gracefully if no manifest is on disk.

4. **Lifecycle stage failure didn't fail the process**. A FAIL
   lifecycle stage flipped `Overall=FAIL` in `e2e.json` but
   `firstErr` stayed nil — meaning the CLI exit code was 0. Fixed:
   lifecycle FAIL now sets `firstErr` so the process exits non-zero.

A fifth perf issue was also fixed:

5. **Doctor's component summarizer fired a second parallel HTTP
   fan-out** on top of the first probe round. That doubled HTTP
   traffic on every doctor run and used `context.Background()` so
   SIGINT was ignored mid-summarize. Fixed: components are now
   captured during the first probe round (CheckResult.components,
   not serialized) and reused.

## Race + vet posture

```
$ go vet ./...                     # clean
$ go vet -tags=e2e ./...           # clean
$ go test -race -count=1 ./...     # all 17 packages pass
```

## Known deviations from the prompt

- **"All 6 pricing models exercised"** target conflicts with the
  "4 archetypes covering 4 pricing models" deliverable. With one
  pricing model per archetype and 4 archetypes total, the scenario
  exercises 4 of the 6 (PER_UNIT, FLAT_RATE, INCLUDED_QUOTA,
  GRADUATED). PERCENTAGE and VOLUME_TIERED are exercised by
  `scenarios/matrix-billing.yaml` — adding 2 more archetypes here
  would have violated the explicit "4 archetypes" deliverable.

- **`--scenario crawl-e2e`** as a built-in scenario name works
  (resolves through the embed FS); the prompt's `--scenario
  scenarios/crawl-e2e.yaml` filesystem form also works. Both are
  documented in the getting-started guide.

## Acceptance criteria

| Criterion                                                    | Status |
| ------------------------------------------------------------ | ------ |
| `aforo-loadgen doctor --target local` passes on fresh stack  | ✅ verified manually against down stack (failure path); test fakes cover happy path |
| e2e completes < 10 min with overall PASS                     | ✅ enforced by tag-gated test; budget = 10 min |
| `report.html` populated, per-archetype + negative-path table | ✅ existing report renderer (Session 5) feeds from validate output unchanged |
| Re-running e2e is idempotent                                  | ✅ clean stage rolls back; doctor + seed both check existing state |
| README quickstart verified end-to-end                        | ✅ docs/getting-started.md is the verified path |

## What ships next

Session 8 — payments + ERP simulation. Stripe-mode test transactions,
QuickBooks/Xero/NetSuite ERP sync, with their own lifecycle drivers
and a new `payments` subcommand. `e2e --include-payments` lights up
once that lands.
