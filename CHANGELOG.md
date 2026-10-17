# Changelog

All notable changes to `aforo-loadgen` are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> Pre-stability notice: until `v1.0.0`, minor versions may include breaking
> changes to the scenario YAML schema or to flag names. Each such change
> ships a one-step migration note in the `Changed` section below.

## [Unreleased]

### Changed — 2026-10-17 (ingest auth, batch path, production guard)

- **Ingest uses an organization-service key** — `seed` now creates one API key
  per tenant on organization-service (`POST /api/v1/api-keys`, scope
  `usage:ingest`, `SANDBOX`/`sk_test_`) and writes it to the manifest as
  `tenants[].ingest_key`. HTTP drivers send it as `X-API-Key` and no longer
  send the per-subscription key. Why: usage-ingestor authenticates API keys
  through aforo-common `ApiKeyAuthFilter`, which validates against
  organization-service's own `api_keys` table; the pricing-service
  per-subscription keys loadgen used to send are answered 401. Opt out with
  `seed --provision-ingest-keys=false`. `seed --clean` revokes the key.
  Manifests without `ingest_key` keep the old behaviour.
- **New `rest_batch` ingestion path** — `POST /v1/ingest/batch`, one tenant
  per request, sized by the new scenario block `rest_batch: {size, max_wait}`
  (size 1..1000, defaults 100 / 100ms). Per-index `errors` in the response
  become per-event 422s; negative-path events are sent alone via
  `/v1/ingest`.
- **Production guard** — `seed`, `run`, `e2e`, `replay`, `lifecycle`,
  `payments` and `billrun` refuse a target whose URLs are `aforo.ai` /
  `*.aforo.ai` unless `--allow-production` or
  `AFORO_LOADGEN_ALLOW_PRODUCTION=i-understand-this-writes-to-production` is
  given. The `staging` target and an un-overridden `ci` target resolve to the
  production hosts; the June 2026 runs recorded as `target: staging` wrote to
  production. Dry runs and `doctor` are not guarded. Coordinator workers
  accept the env acknowledgement only.
- **`run --target-tps N`** overrides `target_tps`, and `run` now refuses
  `target_tps` above 1000 without `--i-know-what-im-doing` (the same
  threshold the trigger server enforces with `acknowledge_high_tps`).
- **Benchmark scenarios** — `bench-ingest-unbatched`, `bench-ingest-batched`,
  `bench-ingest-replay`, `bench-billrun-1000` for the ingest + bill-run benchmark runbook.

### Changed — 2026-10-01 (billing_match prices every metric)

- **`run.json` carries per-metric usage** — `per_tenant_metric`
  (tenant id → metric name → `{events, quantity, max_quantity, by_status}`),
  counting only accepted (2xx) events. Additive; merged across partitions
  in in-process distributed runs.
- **`billing_match` prices each metric of a plan with its own config** —
  own free allowance, block size, min fee, tiers, bands and outcome
  weights — and sums the lines, instead of pricing the tenant's whole event
  count as one metric. SUM metrics are rated on quantity, COUNT on events;
  OUTCOME_BASED uses the recorded per-status split. A customer's share of
  the tenant's usage follows its usable keys (stale customers expect $0).
  The report adds `method: "per_metric"` and a `metrics[]` breakdown per
  customer. A run result without `per_tenant_metric` (older files, the
  coordinator/worker protocol, catalog-mode manifests) is estimated the old
  way.
- **Outcome mix per rate card** uses the card's `product_filter` (first
  entry) when set, falling back to the archetype's first product type.

### Added — 2026-09-30 (OUTCOME_BASED + STAIRCASE)

- **`STAIRCASE` and `OUTCOME_BASED` pricing models** in the scenario schema
  (`rate_config.staircase_bands`, `rate_config.outcome_weights`; also on
  `metric_configs` and `rate_cards`), with validation mirrored from
  pricing-service. Additive — `schema_version` stays 2. The seeder sends
  `outcomeWeights` (keys upper-cased) and staircase `tiers` (flat fee per
  band, open-ended end as `null`); archetype-wide OUTCOME_BASED prices
  non-SUM/COUNT metrics PER_UNIT so pricing accepts the plan. Catalog mode
  rejects both models for now.
- **`outcome_mix` / `outcome_mix_by_product`** — configurable
  execution-status distribution per product template.
- **Billing validator** computes OUTCOME_BASED (billing's OutcomeWeighting,
  then PER_UNIT math) and STAIRCASE (band price) expectations.
- **`scenarios/ci-outcome-based.yaml`** — both models × 3 billing modes,
  plus an outcome + staircase two-card archetype.

### Fixed — 2026-09-30 (review of the OUTCOME_BASED / STAIRCASE change)

- **GRADUATED / VOLUME_TIERED tiers sent in pricing's encoding.** Tiers went
  out 0-based (`(0,1000)`, `(1000,10000)`) with the open top tier as
  `tierEnd: -1`. Pricing sizes a tier as `tierEnd − tierStart + 1` and billing
  treats only `null`/`0` as open-ended, so each band held one unit too many
  and GRADUATED billed nothing above the last bounded tier. Now 1-based
  inclusive with a `null` open end: `(1,1000)`, `(1001,10000)`, `(10001,null)`.
- **A rate card whose only config is `staircase_bands` / `outcome_weights`
  kept it.** The seeder's private "is this card's rate config empty?" check
  didn't know those fields, so such a card silently inherited the
  archetype's config — `ci-outcome-based.yaml`'s `package` card would have
  been sent as STAIRCASE with no tiers (pricing 400). The seeder now uses
  `scenario.IsZeroRateConfig`.
- **Billing validator:**
  - Multi-card archetypes are estimated per customer with the card the
    subscription is on (manifest offerings now record `rate_plan_id`); each
    customer's billing mode drives the routing check.
  - GRADUATED / VOLUME_TIERED tiers are read from the in-memory manifest too
    (the orchestrator's hand-off); they used to parse as "no tiers" → $0.
  - Block ceiling is computed on billing's 6-dp units (integer micro-units),
    not a float quotient rounded to 6 dp.
  - No usage → no charge for PER_UNIT / OUTCOME_BASED: billing writes no
    line for a metric with no usage, so the min fee doesn't apply.
  - HYBRID uses the funded wallet balance (now recorded in the manifest
    rate-plan config).
- STAIRCASE bands may be written in any order (pricing sorts them before
  validating).

### Fixed — 2026-09-30 (seeding against current pricing-service rules)
- PERCENTAGE plans were seeded 100× too low: `percentage_rate` is a fraction (0.025 = 2.5%) but pricing takes a percent (0–100) and bills base × rate / 100. The seeder now sends `percentOf(rate)` (2.5). The validator rejects `percentage_rate > 1`; `soak-all-pricing-models.yaml` had 2.9 (290%) and now says 0.029.
- FLAT_RATE plans no longer seed: pricing rejects a FLAT_RATE metric rate of 0 ("Flat Fee rate must be greater than 0"), which is what loadgen sent. The fee now rides on the first metric as FLAT_RATE (billed once per period), other metrics go PER_UNIT at $0, and `baseFee` is used only for a plan with no metrics. Per-metric FLAT_RATE overrides ("don't bill this metric") go out PER_UNIT at $0. Invoice totals are unchanged.

### Changed — 2026-09-30

- Templates emit canonical upper-case statuses only (was lowercase
  `success` / `unauthorized` / `rate_limited` …, two of which aren't
  canonical and were dropped by the ingestor). `unauthorized` +
  `rate_limited` now map to `BLOCKED`, `validation_error` to
  `VALIDATION_FAILED`. The API and AGENTIC_API templates now carry a status
  too, and every event sends it as top-level `executionStatus` (the MCP
  template's `metadata.execution_status` was never read by the ingestor).
- `PER_UNIT` now sends `blockSize` / `minFee` when set, and the validator
  applies them (PricingCalculator.perUnit honours both).

### Added — 2026-07-11 (AI_AGENT descriptor per-capability coverage)

- **`ai_agent_rest` ingestion path.** New driver at
  `internal/driver/ai_agent_rest.go` — POSTs AI_AGENT usage events to a
  configurable ingest endpoint (`AFORO_LOADGEN_INGEST_URL`, defaults to
  the env var; missing URL is a loud startup failure). Rejects non-AI_AGENT
  events with a transport-class error so a mis-paired `product_mix` +
  `ingestion_paths` surfaces on the first batch. Registered in
  `driver.AllNames()`, `driver.Registry.construct`, the runner's
  `scenarioReferencedPaths`, `scenario.IngestionPaths.AIAgentREST`, and
  `generator.ingestionWeightsFor`.
- **`capability_name` on `aiAgentTemplate`.** Emits a canonical
  `defaultAgentCapabilities` value (10-entry registry — `summarize_email`,
  `translate_document`, `answer_question`, `classify_intent`,
  `extract_entities`, `generate_response`, `search_knowledge`,
  `plan_actions`, `execute_tool`, `verify_output`) rotated per-event. Now
  finally exercises `ProductTypeEventExtractor.extractAiAgentFields`'s
  bridge to `event.toolName`, and downstream the per-capability dimension
  pricing + per-capability anomaly baseline paths.
- **`ci-ai-agent-rest.yaml`.** New CI scenario — 50 TPS × 60 s smoke,
  `product_mix.AI_AGENT: 1.0`, `ingestion_paths.ai_agent_rest: 1.0`.

### Fixed — 2026-07-11 (AI_AGENT metadata key casing)

- **aiAgentTemplate emits camelCase for extractor-facing metadata keys.**
  The template previously emitted `agent_id`, `execution_status`,
  `execution_duration_ms` under snake_case. But
  `extractAiAgentFields` reads `agentId`, `executionStatus`,
  `executionDurationMs` in camelCase as the metadata fallback path when
  the top-level DTO fields are absent (the loadgen Envelope only carries
  the `@NotBlank` top-level fields). Snake_case silently dropped those
  three at the extractor — no 4xx, just missing analytics data.
  `capability_name` stays snake_case because the extractor reads that
  exact spelling. Regression-locked by
  `TestEachProductTemplate` (asserts camelCase keys present and
  snake_case forms absent), `TestAIAgentREST_EmitsWellFormedIngestEnvelope`
  (asserts wire shape), and `TestAIAgentCapabilityNameFromRegistry`.

### Added — 2026-07-11 (nightly MCP e2e workflow)

- **`.github/workflows/mcp-e2e.yml`** — scheduled + PR-touch workflow
  that brings up the full `aforo-nextgen-docker/mcp-e2e/` compose
  stack (Kong with `aforo-metering` plugin + `@aforo/mcp-test-server`
  + `mock-ingest` capture sidecar), runs `ci-mcp-jsonrpc.yaml`
  against Kong, and asserts the plugin's `detect_mcp_tool_call`
  extracted `tool_name` and `_meta.agent_id` correctly. This is the
  first CI path in the workspace that catches regressions in the
  Kong plugin's MCP detection code — before now, only the
  `ci-mcp-only` gate on `aforo-nextgen-usage-ingestor-service`
  covered any MCP surface, and it only tested the `/v1/ingest`
  endpoint side.
- **Triggers**: `schedule: cron '0 4 * * *'` (nightly at 04:00 UTC),
  `workflow_dispatch` (manual with scenario override), and
  `pull_request` when the mcp_jsonrpc driver, its scenario, or the
  workflow itself is touched.
- **Compose stack lives in aforo-nextgen-docker/mcp-e2e/** — see the
  README there for local dev usage. Includes a Node-based
  `mock-ingest` server that captures every event the Kong plugin
  emits and exposes `GET /events/summary` for the shared
  `assert-events.sh` script (5 fail modes: total below floor,
  missing tool_name, missing agent_id, wrong productType, <3
  distinct tools).
- **Complements Eswar's nightly regression pipeline**, doesn't
  replace it — the compose stack is designed to be plug-in-ready
  if the fleet-wide nightly wants to include MCP e2e.

### Added — 2026-07-11 (MCP testing infrastructure closure)

- **`mcp_jsonrpc` ingestion path.** New driver at
  `internal/driver/mcp_jsonrpc.go` — emits **real** JSON-RPC 2.0
  `tools/call` payloads at a configurable MCP endpoint (env
  `AFORO_LOADGEN_MCP_URL`). Closes the gateway-plugin-detection gap:
  every other path posts a synthetic metering event to `/v1/ingest`
  either directly or through a reverse proxy, none exercised the
  `tools/call` extraction code in the aforo-metering gateway plugins
  (Kong `handler.lua` `detect_mcp_tool_call`, AWS Lambda
  `detectMcpToolCall`, Azure APIM `<when>` branch, Apigee JavaScript
  callout, MuleSoft DataWeave). This driver does.
  - Reads `mcpServerTemplate` metadata (`tool_name`, `agent_id`,
    `session_id`) from `Envelope.Metadata` and builds the canonical
    MCP shape (`params.name`, `params.arguments`,
    `params._meta.{agent_id,session_id}`).
  - Sets `Mcp-Session-Id` header from the event's `session_id`, per
    MCP spec. Fresh monotonic JSON-RPC id per Submit (atomic counter).
  - Non-MCP events short-circuit with a specific transport-class
    error naming the event's product type. Missing URL is a loud
    startup failure — no silent 404 storms.
  - Registered as `mcp_jsonrpc` in `driver.AllNames()` and
    `IngestionPaths.MCPJsonRPC`. Bumps documented path count from 16
    to 17 (see `docs/ingestion-paths.md`).
- **`scenarios/ci-mcp-jsonrpc.yaml`.** 50 TPS × 60s smoke that runs
  against [`@aforo/mcp-test-server`](https://github.com/aforoai/aforo-metering-sdks/tree/main/mcp-test-server)
  (bare) or a gateway fronting one (plugin detection). Companion to
  `ci-mcp-only.yaml` which covers the ingest-endpoint side of MCP.
- **6 driver tests** in `internal/driver/mcp_jsonrpc_test.go` covering
  wire-shape assertion, non-MCP rejection, missing-URL loud failure,
  env-var vs config precedence, monotonic id per Submit, and
  registered-in-AllNames guard.

### Added — Session 12 (operator layer)

- **`aforo-loadgen server`** subcommand: real implementation replacing
  the Session 1 stub. HTTP control plane on `--listen :8095` (default)
  with six endpoints under `/api/v1`:
    - `GET  /health` — liveness, no auth
    - `GET  /scenarios` — built-in catalog
    - `GET  /runs` — paginated list (status, scenario, page, per_page filters)
    - `POST /runs` — async trigger; returns 202 with `run_id`
    - `GET  /runs/{id}` — detail (assertions, per-archetype + per-negative-path)
    - `GET  /runs/{id}/manifest` — streams run.json
    - `POST /runs/{id}/cancel` — graceful SIGINT to worker
- **Auth**: Supabase JWT validated by round-trip to
  `/auth/v1/user` (handles HS256 + RS256 transparently). Internal
  role resolved from `internal_roles` PostgREST table — same source
  Control Tower uses. `--allow-anonymous` flag for dev (refuses to
  ship in prod by default — explicit static role required).
- **RBAC**: read endpoints accept any internal role; trigger and
  cancel require `platform_admin`.
- **Worker spawn**: server re-execs the same binary as
  `aforo-loadgen run` so scenario catalog and run engine versions
  are bit-identical with the CLI. Output written to a per-run
  subdir under `--work-dir`; SIGINT-based cancellation drains
  in-flight events and writes a partial manifest.
- **Storage**: pluggable `ManifestStore` interface with two impls:
  `LocalStore` (default; manifest_s3_path = `file:///abs/path`) and
  `S3Store` (shells out to `aws s3 cp` when `--s3-bucket` is set;
  manifest_s3_path = `s3://bucket/key`). Path-traversal hardening
  rejects locators outside the configured root.
- **Index**: pluggable `RunsIndex` with `MemoryIndex` (default; lost
  on restart) and `SupabaseIndex` (PostgREST against `loadgen_runs`
  via service-role key). 24-byte test row written immediately on
  trigger so the polling UI sees `queued` before the worker starts.
- **High-TPS guardrail**: scenarios with `target_tps > 1000` reject
  trigger requests without `acknowledge_high_tps=true` (matches the
  CLI's `--i-know-what-im-doing` semantic).
- **Grafana deep-link**: `--grafana-base-url` populates each run's
  `grafana_url` field with `?var-runId=<id>` so Control Tower's
  detail page renders a one-click "View live metrics" button.
- **Manifest path is server-side only**: `--manifest-path` flag
  configures it at deploy time; the HTTP API does not accept a
  per-request override (defense against arbitrary file reads even
  by platform_admin).
- **`internal/server/`** package: 4 files (~900 LOC) — auth,
  index (Memory + Supabase), storage (Local + S3), runner (subprocess
  spawn + ProcHandle), server (handlers + lifecycle).
- **23 unit + integration tests** across `internal/server/`:
  storage path-traversal, run id validation, content-range parsing,
  pagination, all 5 endpoint contracts, RBAC matrix.
- **Grafana dashboard** at `dashboards/loadgen-run.json` — 11 panels
  using only existing CLI metrics: TPS sent vs failed, latency
  p50/p95/p99, per-archetype TPS, per-product-type TPS, error rate
  by class, negative-path injections, active tenants, backpressure,
  circuit breaker state, per-ingestion-path topk-10. Provisioning
  YAML at `dashboards/grafana/loadgen-provider.yaml`.
- **Local-dev `docker-compose.yaml`**: spins up server + Prometheus +
  Grafana in one command; mounts the dashboards directory so edits
  hot-reload via Grafana's 30s file provisioning interval.
- **Docs**: `docs/dashboard.md` (operator surface overview),
  `docs/grafana-setup.md` (Prometheus + Grafana wiring playbook),
  `docs/smoke-test-manual.md` (10-step end-to-end acceptance test).

### Added — Session 11 (run tier)

- **Multi-machine distributed mode**: `aforo-loadgen coordinator` and
  `aforo-loadgen worker` subcommands. Coordinator partitions tenants
  deterministically (fnv32a hash) across the worker fleet, dispatches
  assignments, polls heartbeats every 5s, and aggregates final reports.
  Wire format is HTTP/2 + mTLS + JSON via the new `internal/coord`
  package (deliberate deviation from the spec's "gRPC + mTLS" line —
  same security properties, no protoc dependency; rationale at the
  top of `internal/coord/protocol.go`). Worker dropout after
  `--dropout-timeout` (default 30s) is logged and the run continues
  with survivors.
- **Chaos injection** (`internal/chaos`): four reversible scenario
  types — `kafka_kill`, `redis_flush`, `ch_slowdown`, `net_partition`.
  Each ships with an Inject + Recovery pair and routes side effects
  through one `Executor` boundary (production: `ShellExecutor` over
  AWS SSM; tests: `Recorder`). Scheduler refuses to fire on
  non-perf-* targets, tolerates jitter via `JitterTolerance`, and
  always invokes Recovery on close (run abort, panic, ctx cancel).
- **Scenario chaos extensions**: validator now whitelists chaos types
  with per-type required-param checks. Both canonical (`params: {…}`)
  and inline-shorthand YAML forms accepted. `Duration: 0` allowed for
  instantaneous events like `redis_flush`. `scenarios/run-15k-7day.yaml`
  populated with the canonical 7-day chaos timeline.
- **Cost tracking** (`internal/cost`): `Tracker` accumulates run
  inputs and produces a `Breakdown` with worker compute, MSK, Redis,
  NAT, egress, and ClickHouse storage lines plus the headline
  `per_million_events_usd` SLO datum. `PreflightEstimate` powers the
  coordinator's pre-flight confirmation prompt: "About to send 15000
  TPS to perf-aws. Generates ~9.07B events / 168h. Estimated cost:
  $1781.42. Continue? [yes/NO]". Skip with `--yes`. Always labeled
  `is_estimate: true` with link to AWS Cost Explorer for ground
  truth.
- **Soak monitoring** (`internal/soak`): `Monitor` records hourly
  snapshots to `<out>/snapshots/snapshot-<ISO>.json` and runs an
  anomaly detector against a 24h trailing baseline. p99 drift > 10%
  → WARN; > 25% → CRITICAL. Failure-rate > 1% → WARN. Never auto-
  aborts; alerts feed external paging tools.
- **Documentation**: `docs/run-phase.md` (rationale, infra, costs),
  `docs/chaos-engineering.md` (per-type contract + authoring guide),
  `docs/ops-runbook.md` (pre-flight, launch, monitor, abort, triage,
  escalation). Session 11 changelog at
  `docs/changelogs/2026-04-30-session-11-run-tier.md`.

### Fixed

- **Lifecycle test data race + flakiness**: `TestFirePauseAndScheduleResume_Success`
  occasionally failed with "live state = PAUSED, want ACTIVE" and
  "transition kinds = [PAUSE PAUSE RESUME], want [PAUSE PAUSE RESUME
  RESUME]". Root cause: the test polled `resumeCalls` to wait for
  the deferred resume goroutine, but the HTTP response landed
  before the goroutine finished its post-call work (state mutation
  + transition log writes). The test then read the transition log's
  `*bytes.Buffer` directly while the goroutine was still appending —
  a true data race even though `TransitionLog.Append` was already
  mutex-protected. Fix: added `TransitionLog.BytesSnapshot()` that
  takes the Append mutex and returns a defensive copy; updated the
  test to wait on `deps.Log.Count() == 4` and read via the safe
  snapshot. 20-iteration race stress is now clean. Production code
  was correct; the bug was in the test's unsafe read pattern.

## [0.1.0] — 2026-04-30

First public release. Six sessions of build (foundation → run engine → seed
harness → driver → resilience → validate oracle), four sessions of feature
breadth (lifecycle → e2e orchestration → walk tier → payments / tax / FX /
ERP / credit notes), and now a release toolchain to ship the binary.

### Added

- **Release toolchain**
  - GoReleaser configuration cross-compiles for `darwin/amd64`,
    `darwin/arm64`, `linux/amd64`, `linux/arm64`. Each release ships a
    `checksums.txt` (SHA-256) and includes the `scenarios/` directory in
    every archive so the bundled scenarios are accessible from disk too.
  - `.github/workflows/release.yml` triggers on tag push (`v*.*.*`),
    runs the full test suite, then drives GoReleaser. The workflow also
    updates the Homebrew tap formula on every release via the
    `HOMEBREW_TAP_GITHUB_TOKEN` secret.
  - Homebrew tap formula at `aforoai/aforo-nextgen-homebrew-tap`.
    `brew tap aforoai/tap && brew install aforoai/tap/loadgen` works
    end-to-end. The formula is regenerated on every release.

- **CI integration scenarios** (5 total, all bundled in the binary):
  - `ci-smoke.yaml` — generic 1-tenant 4-product 60s gate (Session 8;
    unchanged).
  - `ci-mcp-only.yaml` — MCP_SERVER coverage. Targets the
    usage-ingestor-service PR pipeline.
  - `ci-billing.yaml` — six-archetype subset of `matrix-billing` covering
    every pricing model. Targets the billing-platform PR pipeline.
  - `ci-payments-mock.yaml` — payments + tax + ERP with mock providers,
    short window. Targets the billing-service PR pipeline.
  - `ci-stale-keys.yaml` — focused stale-key revocation cascade test
    that fails when the BillingHierarchyEnricher Redis cache serves a
    revoked key. Used as a regression gate against cache-invalidation
    bugs.

- **`--target ci`** environment.
  - New target name resolves URLs from environment variables at flag
    parse time. Order: `AFORO_CI_BASE_URL` (single ingress, fans every
    service to one URL), then per-service `AFORO_CI_<SERVICE>_URL`
    overrides, then the staging URL as fallback.
  - Available on every existing subcommand that takes `--target` (run,
    e2e, doctor, seed, validate, replay, lifecycle, payments).

- **CI gate for Aforo microservice repos.** A drop-in
  `.github/workflows/loadgen-smoke.yml` workflow ships in each of the
  four ingestion-path services (usage-ingestor, analytics, billing,
  catalog). Each runs a focused scenario as a non-blocking smoke check
  on every pull request. Strictly additive — no existing workflow is
  modified. (The session prompt referenced `aforo-billing-platform` as
  the fourth target; that is the documented phantom repo per CLAUDE.md
  Drift entry 2026-04-23. The catalog-service repo owns the catalog +
  billing-platform domain in reality, so the `ci-billing` smoke gate
  ships there instead.)

- **Documentation**
  - `docs/ci-integration.md` — five-step guide for adding the smoke gate
    to a new Aforo microservice repo.
  - `docs/release-process.md` — semver, changelog, tag, observe.

### Changed

- `Makefile`: `release` target now delegates to `goreleaser release
  --clean` rather than printing a stub message. Local dry runs use
  `goreleaser release --snapshot --clean` (no tag, no publish).

[Unreleased]: https://github.com/aforoai/aforo-nextgen-loadgen/compare/v0.1.0...HEAD
[0.1.0]: https://github.com/aforoai/aforo-nextgen-loadgen/releases/tag/v0.1.0
