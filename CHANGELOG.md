# Changelog

All notable changes to `aforo-loadgen` are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

> Pre-stability notice: until `v1.0.0`, minor versions may include breaking
> changes to the scenario YAML schema or to flag names. Each such change
> ships a one-step migration note in the `Changed` section below.

## [Unreleased]

(no entries — the next change adds a line here)

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
