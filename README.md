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

Session 5 of 12. The command tree is in place; `version`, `scenarios`,
`seed`, `run`, `replay`, `validate`, and `report` are fully implemented.
Other subcommands are stubs that announce the session in which they ship.
The scenario YAML schema itself is defined in
[`docs/scenario-schema.md`](docs/scenario-schema.md).

| Subcommand  | Ships in   | What it does                                                      |
| ----------- | ---------- | ----------------------------------------------------------------- |
| `seed`      | _Session 3_ ✓ | Provision tenants per archetype via Aforo's REST APIs.           |
| `scenarios` | _Session 2_ ✓ | List, describe, validate, and show built-in scenarios.          |
| `run`       | _Session 4_ ✓ | Drive a load-test scenario against a target.                    |
| `replay`    | _Session 4_ ✓ | Replay a recorded run-output against a target.                  |
| `validate`  | _Session 5_ ✓ | Validate a completed run vs the platform (8 checks).            |
| `report`    | _Session 5_ ✓ | Render a self-contained HTML run + validation report.           |
| `lifecycle` | Session 6  | Drive subscription lifecycle transitions.                         |
| `payments`  | Session 9  | Drive payment, tax, and ERP integration flows.                    |
| `e2e`       | Session 8  | End-to-end smoke flows against a live target.                     |
| `doctor`    | Session 11 | Diagnose local environment and target reachability.               |
| `server`    | Session 12 | Control-plane server (dashboard + multi-node coordinator).        |
| `version`   | Session 1  | Print semver, commit SHA, and build date.                         |

## Install

### Homebrew (Session 9+)

```bash
brew install aforoai/tap/loadgen
```

The tap is wired in Session 9 once the first signed release ships. Until
then, install via `go install`.

### `go install`

```bash
go install github.com/aforoai/aforo-nextgen-loadgen/cmd/aforo-loadgen@latest
```

### From source

```bash
git clone https://github.com/aforoai/aforo-nextgen-loadgen.git
cd aforo-nextgen-loadgen
make build
./bin/aforo-loadgen --help
```

## Quickstart

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
aforo-loadgen seed --scenario matrix-billing \
                   --archetypes-only mtx-flat-postpaid-cancelled \
                   --target local                             # seed a subset for fast iteration
aforo-loadgen seed --clean --out manifest.json --target local # archive everything in manifest
aforo-loadgen run --scenario ci-smoke --manifest manifest.json \
                  --target local --out runs/ci-smoke-$(date +%s) # drive a scenario (Session 4)
aforo-loadgen replay --run-output runs/ci-smoke-... --target local # re-run from recorded output
aforo-loadgen report --run-id <id>                            # render results (Session 10)
```

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

## Validation oracle (Session 5)

`aforo-loadgen validate` is the post-run oracle. It reads a completed
run's output (`run.json` + `scenario.yaml`) plus the seed manifest and
runs eight independent checks:

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

Without backend access, checks 1, 6, 7 still run from `run.json` alone —
ci-smoke validate exits 0 in pure-CI mode. Checks that need infra (2, 3,
4, 5, 8) `SKIP` with a clear reason.

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
make build      # compile to bin/aforo-loadgen
make test       # run unit tests with -race
make lint       # run golangci-lint (run `make lint-install` first)
make fmt        # gofmt -s
make tidy       # go mod tidy
```

CI runs `make build`, `make test`, and `golangci-lint` on every PR.

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
