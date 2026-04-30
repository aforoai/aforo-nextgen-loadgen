# Changelog

All notable changes to `aforo-loadgen` are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> Pre-stability notice: until `v1.0.0`, minor versions may include breaking
> changes to the scenario YAML schema or to flag names. Each such change
> ships a one-step migration note in the `Changed` section below.

## [Unreleased]

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
