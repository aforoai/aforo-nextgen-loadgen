# aforo-nextgen-loadgen

A Go CLI for load-testing the [Aforo NextGen](https://github.com/aforoai)
platform's ingestion pipeline at scale.

**Target:** sustain 15K TPS across 500 simulated tenants. Crawl-Walk-Run.

**Coverage:** all 4 product types (API, Agentic API, AI Agent, MCP Server),
all 9 gateway adapter types, all 6 pricing models, all 3 billing modes
(POSTPAID / PREPAID / HYBRID), full subscription lifecycle, payment + tax +
ERP flows, and 6 negative-path categories.

This repo lives next to the platform monorepo. For the platform's
architecture, services, and conventions see
[`Nextgen_Aforo/CLAUDE.md`](../CLAUDE.md).

## Status

**v0.1.0 — all 12 sessions complete.** The full command tree is built,
tested, and ready to release. `version`, `scenarios`, `seed`, `run`,
`replay`, `validate`, `report`, `lifecycle`, `doctor`, `e2e`,
`payments`, `coordinator`, and `server` are all production paths. The
release toolchain — Homebrew tap, GoReleaser, GitHub Release tarballs —
is in place and triggered by tagging `v0.1.0`. See
[`docs/release-process.md`](docs/release-process.md) and
[`docs/ci-integration.md`](docs/ci-integration.md) for operator docs,
and [`CHANGELOG.md`](CHANGELOG.md) for the version history. The
scenario YAML schema is defined in
[`docs/scenario-schema.md`](docs/scenario-schema.md).

| Subcommand    | Ships in      | What it does                                                  |
| ------------- | ------------- | ------------------------------------------------------------- |
| `seed`        | _Session 3_ ✓ | Provision tenants per archetype via Aforo's REST APIs.        |
| `scenarios`   | _Session 2_ ✓ | List, describe, validate, and show built-in scenarios.        |
| `run`         | _Session 4_ ✓ | Drive a load-test scenario against a target.                  |
| `replay`      | _Session 4_ ✓ | Replay a recorded run-output against a target.                |
| `validate`    | _Session 5_ ✓ | Validate a completed run vs the platform (18 checks).         |
| `report`      | _Session 5_ ✓ | Render a self-contained HTML run + validation report.         |
| `lifecycle`   | _Session 6_ ✓ | Drive subscription state-machine transitions during a run.    |
| `doctor`      | _Session 7_ ✓ | Diagnose local environment and target reachability.           |
| `e2e`         | _Session 7_ ✓ | doctor → seed → run + lifecycle → validate → report → clean.  |
| `payments`    | _Session 9_ ✓ | Drive payment, tax, FX, ERP, credit-note, and wallet flows.   |
| `coordinator` | Session 11 ✓  | Multi-machine workload coordination + chaos + cost + soak.    |
| `server`      | Session 12 ✓  | Control-plane HTTP server (dashboard + multi-node ops).       |
| `version`     | Session 1 ✓   | Print semver, commit SHA, and build date.                     |

## Install

The loadgen tool ships as a single Go binary on macOS (Apple Silicon +
Intel) and Linux (amd64 + arm64). Pick whichever install path matches
your workflow.

### Homebrew (recommended)

```bash
brew tap aforoai/tap https://github.com/aforoai/aforo-nextgen-homebrew-tap
brew install aforoai/tap/loadgen
```

The tap is regenerated on every release by GoReleaser, so
`brew upgrade aforoai/tap/loadgen` always pulls the latest tagged version.

### GitHub Release tarball

For environments that cannot reach Homebrew, download the per-platform
archive directly from the [Releases page][releases]. Each archive
contains the binary, the `scenarios/` directory, and `LICENSE` /
`README.md` / `CHANGELOG.md`. SHA-256 checksums for every archive are
in `checksums.txt` on the same release page.

```bash
# Replace VERSION + ARCH for your platform.
VERSION=v0.1.0
ARCH=Darwin_arm64    # or Darwin_x86_64, Linux_x86_64, Linux_arm64

curl -L -o loadgen.tar.gz \
  https://github.com/aforoai/aforo-nextgen-loadgen/releases/download/${VERSION}/aforo-loadgen_${ARCH}.tar.gz
tar -xzf loadgen.tar.gz
sudo mv aforo-loadgen /usr/local/bin/
aforo-loadgen version
```

[releases]: https://github.com/aforoai/aforo-nextgen-loadgen/releases

### `go install`

For developers with a Go toolchain who prefer to build from source. The
loadgen repo is private; configure `GOPRIVATE` and a PAT in your
`.netrc` first.

```bash
export GOPRIVATE=github.com/aforoai/*
go install github.com/aforoai/aforo-nextgen-loadgen/cmd/aforo-loadgen@latest
```

### From source

```bash
git clone https://github.com/aforoai/aforo-nextgen-loadgen.git
cd aforo-nextgen-loadgen
make build
./bin/aforo-loadgen --help
```

### Verifying the install

```bash
aforo-loadgen version
# aforo-loadgen v0.1.0 (commit abcdef0, built 2026-04-30T09:35:46Z)
```

The version line records the SemVer tag, the short commit SHA, and the
ISO-8601 build date. A binary built locally from `make build` shows
`0.0.0-dev` until you tag a release.

### CI integration in your service repo

Adding the `loadgen-smoke` GitHub Actions gate to a microservice repo
takes about ten minutes. See
[`docs/ci-integration.md`](docs/ci-integration.md) for the drop-in
workflow template.

## Quickstart

The headline workflow is `aforo-loadgen e2e` — see
[`docs/getting-started.md`](docs/getting-started.md) for a 5-minute walkthrough.

```bash
# Bring the platform up (separate repo).
cd ../aforo-nextgen-docker && docker-compose up -d

# Pre-flight check.
export AFORO_ADMIN_TOKEN=eyJ...
aforo-loadgen doctor --target local

# Full end-to-end flow (~6 minutes).
aforo-loadgen e2e --scenario crawl-e2e --target local \
                  --include-billing --include-lifecycle

# Open the report.
open e2e/crawl-e2e-*/report.html
```

Or piecemeal:

```bash
aforo-loadgen --help                                          # see all subcommands
aforo-loadgen version                                         # print build metadata
aforo-loadgen scenarios list                                  # list built-in scenarios
aforo-loadgen scenarios show ci-smoke                         # print one's YAML
aforo-loadgen scenarios archetypes walk-realistic-50t         # list its archetypes
aforo-loadgen scenarios validate ./my-scenario.yaml           # validate a custom file
aforo-loadgen seed --scenario matrix-billing --dry-run        # plan a seed without sending
aforo-loadgen seed --scenario matrix-billing --target local \
                   --out manifest.json                        # provision against local Aforo
aforo-loadgen seed --clean --out manifest.json --target local # archive everything in manifest
aforo-loadgen run --scenario ci-smoke --manifest manifest.json \
                  --target local --out runs/ci-smoke-$(date +%s) # drive a scenario (Session 4)
aforo-loadgen replay --run-output runs/ci-smoke-... --target local # re-run from recorded output
aforo-loadgen validate --run-output runs/ci-smoke-... \
                       --manifest manifest.json --target local    # 11-check oracle (Session 5)
aforo-loadgen report --run-output runs/ci-smoke-...                # render HTML (Session 5)
```

## End-to-end orchestrator (Session 7)

`aforo-loadgen e2e` chains every prior session into one subcommand. On
a healthy local Docker stack the full flow finishes in under 10 minutes
and produces a self-contained `report.html`.

```
aforo-loadgen e2e --scenario crawl-e2e --target local \
                  --include-billing --include-lifecycle
```

Stages (with per-stage timing in `<out>/e2e.json`):

1. **doctor** — verify every service is reachable + auth works
2. **seed** — provision tenants per archetype
3. **run** — drive event traffic at the seeded population
4. **lifecycle** — fire subscription state transitions in parallel (skipped without `--include-lifecycle`)
5. **validate** — assert post-run invariants and per-archetype billing
6. **report** — render `report.html` (offline-safe, no CDN)
7. **clean** — archive seeded entities (skipped via `--keep-data`)

Even on stage failure, every prior stage's artifacts are preserved on
disk so you can debug in place. See
[`docs/troubleshooting.md`](docs/troubleshooting.md) for symptom →
remedy maps.

`make e2e-local` and `make e2e-staging` wrap the headline invocation;
`make e2e-test` runs a tag-gated Go test that asserts the flow
completes inside its budget.

## Doctor (Session 7)

`aforo-loadgen doctor` is the standalone pre-flight check that the e2e
flow runs first. It probes every microservice's `/actuator/health`,
verifies your bearer token authenticates against organization-service,
and summarizes platform infra (PostgreSQL, Kafka, Redis, ClickHouse)
status reported by the services that expose component health.

```
aforo-loadgen doctor --target local                          # human view
aforo-loadgen doctor --target local --json doctor.json       # both
aforo-loadgen doctor --target local --json-only              # for orchestration
```

Exit code is 0 when every CRITICAL check passes, 1 otherwise.
ai-service is WARNING-only because it isn't required for any base e2e
archetype.

## Run engine (Session 4)

`aforo-loadgen run` drives a scenario against a target, generating events
end-to-end:

- **Generator**: tenant traffic shaped by Pareto 80/20 / Zipf / Uniform;
  product mix sampled from `ProductMix`; per-product-type templates for
  API / AI_AGENT / MCP_SERVER / AGENTIC_API matching Aforo's
  `MetricTemplateRegistry`; payload variation small / medium / large;
  six negative-path injectors (`late_event`, `future_event`, `malformed`,
  `wrong_auth`, `stale_key`, `oversize`).
- **Driver**: REST direct path POSTs to `/v1/ingest` with
  `Authorization: Bearer <key>` + `X-Tenant-Id`. Custom transport
  (MaxIdleConnsPerHost=100, no auto-redirect).
- **Resilience**: rolling-window backpressure (5%/30s → 0.5× TPS) and
  circuit breaker (50%/60s → 30s pause → half-open probe).
  Negative-path-induced failures are tagged "expected" and don't trip
  these — otherwise an `oversize_pct=0.05` scenario would flap the
  breaker.
- **Output dir**:
  - `run.json` — per-archetype, per-tenant, per-negative-path counts +
    p50/p90/p99 latency + state-change timestamps
  - `events.jsonl` — first 1000 events with debug metadata (incl.
    `subscription_id`, `stale_reason`, `stale_since` for stale_key)
  - `latencies.hdr` — HDR distribution
  - `per_archetype.json` — flat archetype counts
  - `scenario.yaml` — copy of the scenario for byte-identical replay
- **Metrics**: `--metrics-addr :9095` exposes Prometheus
  `/metrics` + `/healthz`; `--pprof-port` adds `/debug/pprof/*`.
- **Reproducibility**: `scenario.seed + manifest + scenario` →
  identical event sequence. `replay` reads the recorded scenario.yaml
  and re-runs.

Stale-key safety: a scenario with `negative_paths.stale_keys_pct > 0`
fails at startup if the manifest has zero stale subscriptions — the
load test won't silently skip the fault injection.

## Lifecycle agent (Session 6)

`aforo-loadgen lifecycle` runs the Session-4 event generator AND a
parallel "lifecycle agent" that fires real subscription state-machine
transitions concurrent with the load. Together they exercise the
platform's:

- 9-state subscription state machine (`SubscriptionStateMachine`)
- pro-ration math on offering migration (full / calendar / wallet / usage)
- rate-plan version pinning across upgrades + downgrades
- wallet hold/release lifecycle on PREPAID/HYBRID modes
- dunning escalation (PAST_DUE → SUSPEND/CANCEL after N retries)
- bill-run-vs-migrate Redis lock contention

The agent reads `scenario.lifecycle.*_per_hour_pct` to schedule one
ticker per transition kind. Each ticker samples eligible subscriptions
(deterministically from `scenario.seed`), filters by state-machine
legality, fires the API call, and writes a row to
`<out>/transitions.jsonl`.

| Transition       | Endpoint                                          |
| ---------------- | ------------------------------------------------- |
| UPGRADE          | `POST /api/v1/subscriptions/{id}/upgrade`         |
| DOWNGRADE        | `POST /api/v1/subscriptions/{id}/downgrade`       |
| PAUSE / RESUME   | `POST /pause` + scheduled `POST /resume`          |
| TRIAL_CONVERSION | `POST /api/v1/subscriptions/{id}/convert-trial`   |
| TRIAL_CANCEL     | `POST /api/v1/subscriptions/{id}/cancel`          |
| MIGRATE          | `POST /api/v1/subscriptions/{id}/migrate-with-proration` |
| RETRY_PAYMENT    | `POST /api/v1/subscriptions/{id}/retry-payment`   |
| DUNNING_ESCALATE | (assertion-only — drives `RETRY_PAYMENT` past the configured max-attempts) |

Two rows are appended per transition: a `PENDING` intent row written
**before** the API call (so a hung agent leaves a breadcrumb) and an
`OK`/`FAIL`/`SKIPPED` outcome row after. The validator (Checks 9–11)
counts only outcome rows.

```bash
# Lifecycle alongside event load:
aforo-loadgen lifecycle --scenario lifecycle-stress --manifest manifest.json --target staging

# Lifecycle without event load (focused state-machine probing):
aforo-loadgen lifecycle --scenario lifecycle-stress --target local --no-runner

# Compress real customer pause windows so a 4h run produces both pause + resume signal:
aforo-loadgen lifecycle --scenario lifecycle-stress --target local --pause-resume-delay 30s
```

Output: `<out>/transitions.jsonl` (one record per intent + one per
outcome) plus the standard `run.json` + `scenario.yaml` artifacts. Pass
the same `<out>` to `aforo-loadgen validate` to run Checks 9–11.

## Payments + ERP + tax + FX + wallet (Session 9)

`aforo-loadgen payments` drives the **full post-invoice pipeline** for a
seeded population:

  * **Stripe test-mode** payment execution per
    `scenario.payments.success_pct` mix (decline cards trigger the
    dunning sequence to its terminal `SUSPENDED` state).
  * **Tax** — engines: `mock` (deterministic, default), `avalara`
    (AvaTax v2), `vertex` (O Series). Per-currency jurisdiction
    routing. See [docs/tax-engines.md](docs/tax-engines.md).
  * **Multi-currency FX** with pinned rates in `scenario.fx` so
    cross-run reproducibility is exact. The validator asserts FX is
    applied at bill-run time (not event-ingest time).
  * **ERP sync** verification across all four providers (QuickBooks,
    Xero, NetSuite, custom webhook). Per-tenant single-ERP invariant
    asserted (Check 18). See [docs/erp-onboarding.md](docs/erp-onboarding.md).
  * **Credit notes** — full + partial refunds with `apply-to-invoice`
    flow.
  * **Wallet lifecycle** — pre/post snapshots, hold/release events,
    `HoldExpiryScheduler` convergence asserted.

```bash
# 80% success / 15% decline / 5% insufficient — drives dunning on the
# decline records to SUSPEND.
aforo-loadgen payments \
    --scenario scenarios/payments-stripe-test.yaml \
    --target staging \
    --manifest manifest.json \
    --out runs/pay-$(date +%s)

# Override the tax engine + ERP providers from the CLI:
aforo-loadgen payments --tax-engine avalara --erp-providers quickbooks,xero ...
```

Outputs (under `--out`):
  * `payments.jsonl`     — every charge attempt + outcome (Check 12)
  * `erp_sync.jsonl`     — per-invoice sync record + provider verification (Check 15)
  * `credit_notes.jsonl` — DRAFT → ISSUED → APPLIED transitions (Check 16)
  * `wallet_audit.jsonl` — pre/mid/post snapshots + hold-state events (Check 17)
  * `transitions.jsonl`  — extends the lifecycle agent's log with retry-payment + dunning rows

See [docs/payments-setup.md](docs/payments-setup.md) for env vars.
**Live Stripe and ERP creds are optional** — the binary runs in offline
synthesis mode in CI and the validator's checks still pass.

## Validation oracle (Sessions 5–9)

`aforo-loadgen validate` is the post-run oracle. It reads a completed
run's output (`run.json` + `scenario.yaml` + optional `transitions.jsonl`,
`payments.jsonl`, `erp_sync.jsonl`, `credit_notes.jsonl`,
`wallet_audit.jsonl`) plus the seed manifest and runs **eighteen**
independent checks:

| # | Check | What it verifies |
| - | ----- | ---------------- |
| 1 | `event_count_per_tenant` | events_sent ≈ events stored in ClickHouse usage_records (per tenant, within tolerance) |
| 2 | `cross_tenant_leakage` | 10 IDOR probes: query with the wrong `X-Tenant-Id` returns zero rows |
| 3 | `billing_hierarchy_resolution` | every ingested event resolved a `customer_id` (no NULLs from BillingHierarchyEnricher) |
| 4 | `cache_hit_ratio` | BillingHierarchyEnricher Redis cache hit ratio ≥ scenario threshold |
| 5 | `per_archetype_billing_match` | invoice math matches expected per archetype × customer (`--include-billing`) |
| 6 | `negative_path_correctness` | every negative-path category was rejected as expected, incl. **zero stale-key false positives** |
| 7 | `property_based_invariants` | seven seeded fuzz invariants over the billing pipeline |
| 8 | `bill_run_concurrency` | two simultaneous bill runs collide with 409 (`--include-billing`) |
| 9 | `lifecycle_correctness` | per transition in `transitions.jsonl`: post-state matches expected, stable-id preserved on migrate, dunning escalates per config (Session 6) |
| 10 | `state_machine_invariants` | no illegal transitions: CANCELLED → ACTIVE, EXPIRED → anything, GA → BETA regression (Session 6) |
| 11 | `bill_run_vs_lifecycle` | fire 2 simultaneous bill runs + 1 migrate on the same tenant: exactly 1 bill run wins, 1 returns 409, migrate keeps stable id, no double-billing (Session 6, `--include-billing`) |
| 12 | `payment_processing` | success rate within tolerance of `scenario.payments.success_pct`; every PAID record has a Stripe `payment_intent_id`; every declined record has a `failure_code` (Session 9) |
| 13 | `tax_math` | `tax_amount = subtotal × jurisdiction_rate` for every (currency, jurisdiction) pair, within `scenario.tax.tolerance_usd` (Session 9) |
| 14 | `multi_currency` | EUR/GBP customers receive invoices in their currency; pinned FX rates applied at bill-run time, not event-ingest time (Session 9) |
| 15 | `erp_sync` | ≥99% of issued invoices synced to the configured ERP within SLA; provider-side externalDocumentId resolves at the sandbox (Session 9) |
| 16 | `credit_notes` | DRAFT → ISSUED (→ APPLIED) progression for every credit note; `PRORATION` reason; ≤5% errors (Session 9) |
| 17 | `wallet_lifecycle` | sum of pending holds ≤ initial balance; expired holds released by `HoldExpiryScheduler` within `hold_ttl_seconds` (Session 9) |
| 18 | `single_erp_invariant` | second ERP connect on a tenant returns 409 — disabled when `scenario.erp.multi_erp_enabled = true` (Session 9) |

Without backend access, checks 1, 6, 7, 9, 10, 12, 13, 14, 15, 16, 17 still
run from local artifacts alone — ci-smoke validate exits 0 in pure-CI
mode. Checks that need infra (2, 3, 4, 5, 8, 11, 18) `SKIP` with a clear
reason.

The strongest assertion is the stale-key zero-false-positives check
(Check 6.e.3 + Check 7.g): a single successful ingestion on a revoked
api_key in the run window fails the validator loudly. This catches
cache-invalidation regressions in `BillingHierarchyEnricher`.

```bash
# Pure offline (CI smoke):
aforo-loadgen validate --run-output runs/ci-smoke-... --manifest manifest.json --target local

# Full backend coverage:
aforo-loadgen validate --run-output runs/matrix-billing-... \
    --manifest manifest.json --target staging --include-billing \
    --tolerance-pct 0.001
```

Output: `<run-output>/validation.json` (machine-readable, schema v2) +
stdout summary table. Exits 1 on any FAIL — CI gates on this.

## HTML report (Session 5)

`aforo-loadgen report --run-output <dir>` renders a self-contained
`report.html` with run summary, per-tenant table, per-archetype billing
accuracy, per-negative-path table, and any property-based test
violations. **No CDN, no Google Fonts, no external assets** — system
fonts only so the file renders identically when forwarded to Slack
channels or attached to PR comments, including offline.

```bash
aforo-loadgen report --run-output runs/ci-smoke-1714438800
# → runs/ci-smoke-1714438800/report.html
```

## Seed harness (Session 3)

`aforo-loadgen seed` materializes one tenant per archetype slot end-to-end
via Aforo's REST APIs: tenant → products → billable units → rate plan →
offering → customers → subscriptions (incl. CANCELLED + EXPIRED for stale
key tests) → wallets (PREPAID/HYBRID) → payment methods → discounts → API
keys. Outputs `manifest.json` (schema v2) that downstream subcommands read.

Auth: set `AFORO_ADMIN_TOKEN` in the environment. The harness self-rate-
limits at one request every 200ms (configurable) and caps in-flight HTTP
requests at 50 to keep from DDoS'ing the admin API.

Idempotency: every entity is created with a deterministic `external_id`
(`loadgen-{kind}-{archetype}-{run_id}-{seq}`); a re-run with the same run id
hits the GET-by-externalId cache and never double-POSTs.

Live integration test (against running Aforo):

```bash
docker compose up -d                          # in ../aforo-nextgen-docker
export AFORO_ADMIN_TOKEN=$(...your token...)
go test -tags integration ./internal/seed/... # seeds, picks a stale key, hits ingestor
```

## Global flags

| Flag           | Default | Purpose                                                       |
| -------------- | ------- | ------------------------------------------------------------- |
| `--target`     | _none_  | Base URL of the platform under test.                          |
| `--config`     | _none_  | Path to a loadgen YAML config file.                           |
| `--log-level`  | `info`  | `debug`, `info`, `warn`, `error`.                             |
| `--json-logs`  | `false` | Emit logs as newline-delimited JSON.                          |

## Development

```bash
make build           # compile to bin/aforo-loadgen
make test            # run unit tests with -race
make lint            # run golangci-lint (run `make lint-install` first)
make fmt             # gofmt -s
make tidy            # go mod tidy
make contract-test   # validate loadgen ↔ backend OpenAPI wire-format contract
make sync-openapi    # refresh openapi/<service>.json snapshots from a running backend
```

CI runs `make build`, `make test`, `make contract-test`, and `golangci-lint`
on every PR.

### Backend wire-format contract (sync mechanism)

Loadgen calls 9 backend microservices via REST. The governing rule is:
**loadgen invents no field names — every json tag, every Go identifier, every
manifest column maps 1:1 to a real backend column.** The full convention
lives at [`CONVENTIONS.md`](CONVENTIONS.md) — read that before adding a
new entity or modifying an existing wire surface.

The convention is enforced mechanically: every Go request / response
struct in `internal/seed/` carries `json:"..."` tags that the contract
test at `internal/seed/contract_test.go` reflects against the committed
OpenAPI snapshots under `openapi/`. Drift fails CI.

The flow when a backend DTO changes:

1. Backend rename lands (e.g. `productType` → `type` on
   `ProductResponse.java`).
2. A maintainer runs `make sync-openapi` against a running local
   docker-compose stack to refresh `openapi/<service>.json` from
   Springdoc's `/v3/api-docs` endpoint.
3. The next `make contract-test` (or `make test`, or CI) reports the
   loadgen struct that still names the old field, along with a fix
   message pointing at the struct file and the snapshot diff.
4. The PR author updates the loadgen struct and commits both the snapshot
   AND the loadgen change together so reviewers see the contract drift
   end-to-end.

Both directions are covered:

- **Loadgen drift** (wrong json tag): contract test fails immediately on PR.
- **Backend drift** (renamed field) without snapshot refresh: undetected
  until the next `make sync-openapi`. Run it at least once per sprint
  even when nothing in loadgen has changed — the diff in
  `openapi/<service>.json` is itself a useful PR review artifact.

When a new request/response struct lands in `internal/seed/`, add a row to
`contractEntries()` in `internal/seed/contract_test.go` so the new struct
is enforced. See `internal/contract/doc.go` for what the contract test
catches + what it does NOT (type-shape drift and @NotBlank validation
remain integration-test territory).

### Adding a subcommand

1. Add a file under `internal/cli/<name>.go` exporting `new<Name>Command`.
2. Register it in `internal/cli/root.go`.
3. Append the name to `expectedSubcommands` in `internal/cli/cli_test.go`.
4. Update the table at the top of this README.

The Session 1 acceptance test (`TestEverySubcommandExitsZero`) enforces that
every registered subcommand exits 0 with output.

### Adding a built-in scenario

1. Drop a `.yaml` file in `scenarios/`. The `embed.FS` picks it up at build time.
2. Run `aforo-loadgen scenarios validate scenarios/<name>.yaml` and fix anything red.
3. Update the catalog list in `internal/scenario/golden_test.go` so the
   golden-file test enforces the new file is present and valid.
4. Document the scenario in [`docs/scenario-schema.md`](docs/scenario-schema.md)
   under "Reference: built-in scenarios".

## License

Apache 2.0 — see [LICENSE](LICENSE).

## Persona model

Three personas show up in scenarios and docs:

- **Aforo** — the platform itself (this repo's _target_).
- **SmartAI** — a simulated tenant of Aforo.
- **Acme** — a simulated end-customer of SmartAI.

These names are used consistently across fixtures, README examples, and the
scenarios catalog.
