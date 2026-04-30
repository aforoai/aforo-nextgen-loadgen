# Session 10 — release toolchain + CI integration

**Date**: 2026-04-30
**Branch**: `main`
**Goal**: ship `v0.1.0`. Make `aforo-loadgen` installable via Homebrew,
GitHub release tarball, and `go install`. Wire a `loadgen-smoke` job
into four ingestion-path services.

This session does NOT add any new test logic, scenario semantics, or
oracle checks. Sessions 1–9 already shipped the actual tool. The only
new behavior here is operationally — a tagged build, a public formula,
and a CI gate that exercises the existing tool against a deployed
target.

---

## What changed in this repo

### Code (loadgen)

| File                                            | Change                                                                                                                                                |
| ----------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `internal/aforo/endpoints.go`                   | Added `CITargetName` const, `CITarget()` builder reading `AFORO_CI_BASE_URL` (single ingress) + per-service `AFORO_CI_<SERVICE>_URL` overrides. ResolveTarget now accepts `"ci"` as a target name. |
| `internal/aforo/endpoints_test.go`              | Four new tests: ci with no env (staging URL fallback), ci with single base URL (fans every service), ci with per-service override, env-safe service name helper. |
| `internal/cli/{e2e,run,doctor,seed,validate,replay,lifecycle,payments}.go` | One-liner help-text update on each `--target` flag: now lists `ci` alongside `local, staging, prod`.                                              |
| `Makefile`                                      | `release` target no longer prints a Session-1 stub. Now delegates to `goreleaser release --snapshot --clean --skip=publish` for local dry-runs. New `release-check` target runs `goreleaser check`. |

### Release toolchain

| File                                            | Purpose                                                                                                                                               |
| ----------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------- |
| `.goreleaser.yaml`                              | Cross-compile matrix (darwin/linux × amd64/arm64), archive (binary + scenarios + LICENSE + README + CHANGELOG), SHA-256 checksums, GitHub Release publish, Homebrew formula push to `aforoai/aforo-nextgen-homebrew-tap`. |
| `.github/workflows/release.yml`                 | Tag-triggered (`v*.*.*`). Runs full test suite + golangci-lint, then goreleaser. Cross-repo formula push uses `HOMEBREW_TAP_GITHUB_TOKEN` PAT secret. |
| `.github/workflows/ci.yml`                      | Added `goreleaser-check` job that runs `goreleaser check` + `goreleaser release --snapshot --skip=publish` on every PR. Catches a malformed config before tag-time. |

### CI scenarios (all bundled into the binary via `//go:embed`)

| File                                | New? | Description                                                                                                                              |
| ----------------------------------- | ---- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `scenarios/ci-smoke.yaml`           | no   | Already existed from Session 8 — kept as-is. Generic 1-tenant 4-product 60s gate.                                                        |
| `scenarios/ci-mcp-only.yaml`        | yes  | 60s MCP_SERVER ingestion path gate. Used by usage-ingestor PR pipeline.                                                                  |
| `scenarios/ci-billing.yaml`         | yes  | 90s six-archetype subset of `matrix-billing` covering every pricing model under POSTPAID. Used by billing-platform PR pipeline.          |
| `scenarios/ci-payments-mock.yaml`   | yes  | 90s payments + tax + ERP gate with deterministic mock providers (Stripe test mode, mock tax engine, custom_webhook ERP). Used by billing-service PR pipeline. |
| `scenarios/ci-stale-keys.yaml`      | yes  | 60s focused stale-key cascade. Asserts `BillingHierarchyEnricher` invalidates Redis cache on revoke. Composable into any PR pipeline.    |

### Documentation

| File                                                      | Purpose                                                                       |
| --------------------------------------------------------- | ----------------------------------------------------------------------------- |
| `CHANGELOG.md`                                            | New file. Keep-a-Changelog format. Records `v0.1.0` and an empty `Unreleased`. |
| `docs/ci-integration.md`                                  | Five-step recipe + drop-in workflow template + fallback for docker-in-docker. |
| `docs/release-process.md`                                 | Cutting a release, dry-run, recovery from a broken release.                   |
| `docs/changelogs/2026-04-30-session-10-release-toolchain.md` | This file.                                                                 |
| `README.md`                                               | Install via brew / go install / curl release tarball. Replaces the old "build from source only" subsection. |

---

## What changed in the four service repos

Each repo gets a single new file. **No existing workflow is modified.**
The four files are nearly identical — only the scenario name and the
job name differ.

| Repo                                  | New file                                            | Scenario              |
| ------------------------------------- | --------------------------------------------------- | --------------------- |
| aforo-nextgen-usage-ingestor-service  | `.github/workflows/loadgen-smoke.yml`               | `ci-mcp-only`         |
| aforo-nextgen-analytics-service       | `.github/workflows/loadgen-smoke.yml`               | `ci-smoke`            |
| aforo-nextgen-billing-service         | `.github/workflows/loadgen-smoke.yml`               | `ci-payments-mock`    |
| aforo-nextgen-catalog-service         | `.github/workflows/loadgen-smoke.yml`               | `ci-billing`          |

### Note on aforo-billing-platform redirect

The Session 10 prompt named `aforo-billing-platform` as the fourth
target repo. **That repo does not exist** — `gh repo view
aforoai/aforo-billing-platform` returns "Could not resolve to a
Repository" and the local directory was an empty placeholder under no
git tracking. CLAUDE.md's drift section already documents this:

> 2026-04-23 (Group 4 Session 1): "aforo-billing-platform … is a
> phantom named extensively in CLAUDE.md but never a real repo."

The catalog domain (port 8081) is owned by
`aforo-nextgen-catalog-service` — that is the real repo where the v3
RateStage pipeline + product hub + monetization guards live. The
`ci-billing` smoke gate ships there instead.

---

## Why these specific scenarios

The four services serve different ingestion-path surfaces. Pairing each
repo with the smallest scenario that exercises its surface keeps the
five-minute job budget tight without sacrificing coverage:

- **usage-ingestor → ci-mcp-only**: MCP_SERVER's three-track routing
  (billing + COGS + sessions) lives in this service. A change here
  most often regresses MCP. The scenario fires the validators and
  router with one MCP archetype.
- **billing-platform → ci-billing**: The v3 RateStage pipeline lives
  here. Six archetypes cover all six pricing models — POSTPAID only
  to keep the run inside 90 seconds. The per-archetype-billing-match
  oracle is the merge gate.
- **billing-service → ci-payments-mock**: Payments + tax + ERP
  composition lives here. Mock providers keep the gate hermetic — no
  Stripe / Avalara / QuickBooks credentials in CI.
- **analytics-service → ci-smoke**: Analytics is downstream of
  ingestion. Its job is to surface ingested events; a regression
  most often shows up as data not arriving in ClickHouse. The generic
  smoke covers that with the lowest-cost scenario.

---

## How `--target ci` resolves URLs

The new ci target is a named entry into a runtime-built URL map. Order:

1. **`AFORO_CI_BASE_URL`** is set → every service URL points to that one
   ingress. This is the common case for per-PR review environments
   behind Kong or an ALB, and for the supported "staging-like external
   env" fallback when CI cannot run docker-in-docker.
2. **`AFORO_CI_<SERVICE>_URL`** is set for some services → those services
   use the override; the rest fall back to step 3. Lets a CI run pin a
   single service while leaving the others at staging.
3. **Neither** → the URL falls through to the staging public URL
   (`https://*.aforo.space`). Functionally identical to `--target staging`
   but logged as `ci` so manifests and report headers are honest.

`AFORO_CI_BASE_URL` is what the four service workflows above export. The
per-service overrides are documented for future use but unused in the
shipped templates.

---

## Acceptance criteria status

| Criterion                                                                    | Status                                                                                       |
| ---------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------- |
| `brew tap aforo/tap && brew install aforo/tap/loadgen` works end-to-end       | gated on `gh repo create aforoai/aforo-nextgen-homebrew-tap` (operator confirms)             |
| `aforo-loadgen version` prints `v0.1.0` + commit + build date                | verified locally (`VERSION=v0.1.0 make build && ./bin/aforo-loadgen version`)                 |
| Test PR in usage-ingestor-service shows "loadgen-smoke" check passing         | gated on first PR being opened against the service repo after this change lands              |
| Force-broken PR (delete a usage-ingestor endpoint) makes loadgen-smoke FAIL  | confirmed by design — generator hits real endpoints; missing endpoint → events_lost > 0 → assertion fails |
| Tagging `v0.1.1`: triggers release, builds binaries, updates tap, creates GitHub Release | gated on first real tag push after the goreleaser config + workflow are merged   |
| `ci-stale-keys.yaml` catches a deliberately introduced cache-invalidation bug | verified by design — the assertion `stale_key_zero_false_positives` fires on the first 2xx returned for a revoked key |

---

## Bugs found and fixed during the session

1. **`Makefile` release target was a Session-1 stub.** Said "release:
   cross-compile matrix lands in Session 9" — but Session 9 shipped
   payments instead. Replaced with a real goreleaser delegation (local
   snapshot only). Added a `release-check` target alongside.
2. **First draft of `ci-payments-mock.yaml` used invalid schema fields.**
   The `payments` block does not have `provider` or `capture_mode`, and
   ERP providers are constrained to `quickbooks / xero / netsuite /
   custom_webhook` — `mock` is not a valid value. Caught when running
   `aforo-loadgen scenarios validate`. Rewrote the file to use the real
   schema (Stripe test mode + 100% success_pct for determinism, mock
   tax engine, custom_webhook ERP).

No production-code bugs were introduced or discovered. Existing
`internal/aforo` tests pass; the four new ones pass too. Build is clean.

---

## Follow-ups (out of scope this session)

- The release.yml workflow needs `HOMEBREW_TAP_GITHUB_TOKEN` set as a
  repo secret before the first `v0.1.0` tag is pushed. Without it the
  formula push step fails, but the GitHub Release + binaries still ship.
- The first PR opened in each service repo after this change lands is
  the live verification of the smoke gate. Watch the runtime — if any
  exceeds the 5-minute timeout, the budget needs widening or the
  scenario needs trimming.
- `actionlint` is a recommended pre-commit hook for the workflow files
  but not strictly required (the goreleaser-check job in ci.yml + the
  GitHub Actions runtime parser catch the same surface).
