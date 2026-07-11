# Adding the loadgen smoke gate to a microservice repo

This is the five-step recipe for adding a `loadgen-smoke` GitHub Actions
job to an Aforo backend repo so that every PR to that repo runs a
contract check against the loadgen tool.

The job is **strictly additive** — it ships in its own workflow file and
never touches the existing `deploy.yml`. If the job fails, the merge is
blocked. If the job is healthy, you get a standard "smoke-test" check on
the PR. Total job time is under five minutes.

The four ingestion-path repos already ship with this gate:

| Repo                                  | Scenario              |
| ------------------------------------- | --------------------- |
| aforo-nextgen-usage-ingestor-service  | `ci-mcp-only`         |
| aforo-nextgen-analytics-service       | `ci-smoke`            |
| aforo-nextgen-billing-service         | `ci-payments-mock`    |
| aforo-nextgen-catalog-service         | `ci-billing`          |

Follow the steps below to add the gate to a fifth, sixth, etc. repo.

## Also see: nightly MCP e2e stack (this repo, 2026-07-11)

Separate from the per-PR gate above, this repo also ships a **scheduled
end-to-end workflow** that exercises the whole loadgen → Kong-with-plugin
→ mcp-test-server → capture chain:

- Workflow: [`.github/workflows/mcp-e2e.yml`](../.github/workflows/mcp-e2e.yml)
- Compose stack: [`aforo-nextgen-docker/mcp-e2e/`](https://github.com/aforoai/aforo-nextgen-docker/tree/main/mcp-e2e)
- Scenario: [`scenarios/ci-mcp-jsonrpc.yaml`](../scenarios/ci-mcp-jsonrpc.yaml)
- Driver: `mcp_jsonrpc` (real JSON-RPC 2.0 `tools/call` emission)
- Toy MCP server: [`@aforo/mcp-test-server`](https://github.com/jayaforo/aforo-metering-sdks/tree/main/mcp-test-server)

Runs on:
- `schedule: cron: '0 4 * * *'` — nightly at 04:00 UTC
- `workflow_dispatch` — manual trigger with optional scenario override
- `pull_request` on this repo when the mcp_jsonrpc driver, its scenario,
  or the workflow file itself is touched

Fails if the Kong `aforo-metering` plugin's `detect_mcp_tool_call`
regresses (missing `tool_name` extraction, missing `_meta.agent_id`
extraction, wrong `productType`, or fewer than 3 distinct tools across
a 60s × 50-TPS run). Signals a real regression, not a flake — the
`MIN_EVENTS` floor is deliberately generous to absorb the plugin's
log-phase batch flush timing.

**Coordination note.** This workflow is a per-repo nightly, not the
full multi-service nightly regression pipeline owned by Eswar
(`eswarprasad@aforo.ai`). If Eswar's pipeline wants to include the
MCP e2e path, the `aforo-nextgen-docker/mcp-e2e/docker-compose.yml`
stack is ready to plug in — same three services, same assertion
script, same env vars.

---

## What you need before starting

Three repository secrets:

| Secret                  | Purpose                                                                                                                                  |
| ----------------------- | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `AFORO_LOADGEN_PAT`     | A PAT with `repo` scope on `aforoai/aforo-nextgen-loadgen`. Lets `go install` fetch the private loadgen module.                          |
| `AFORO_CI_BASE_URL`     | The URL of the deployed environment to load-test against (e.g. `https://staging.aforo.ai`). Required when the smoke target is shared. |
| `AFORO_STAGING_TOKEN`   | Bearer token the loadgen binary uses to authenticate as a platform admin against the smoke target. Required.                             |

If your repo is already in `aforoai/`, the loadgen team has likely set
all three at the org level — check `Settings → Secrets → Repository
secrets` first.

---

## Step 1 — Pick a scenario

Decide which CI scenario the smoke job should run. The five built-ins:

| Scenario           | Target                                                | Window |
| ------------------ | ----------------------------------------------------- | ------ |
| `ci-smoke`         | Generic 1-tenant 4-product happy path                 | 60s    |
| `ci-mcp-only`      | MCP_SERVER ingestion path                              | 60s    |
| `ci-billing`       | Six-archetype subset of matrix-billing (POSTPAID)     | 90s    |
| `ci-payments-mock` | Payments + tax + ERP with deterministic mock providers | 90s    |
| `ci-stale-keys`    | BillingHierarchyEnricher cache invalidation cascade   | 60s    |

Pick the one closest to the contract your service exposes. If you are
not sure, start with `ci-smoke` — it is the cheapest and exercises the
broadest happy path.

If none match, drop a new scenario YAML into
`aforo-nextgen-loadgen/scenarios/` (it will be embedded into the binary
on next release) and reference it by name.

---

## Step 2 — Drop in the workflow file

Create `.github/workflows/loadgen-smoke.yml` in your repo. Copy this
template verbatim, then change two values: the scenario name and the
job description.

```yaml
name: loadgen-smoke

# Runs on every PR. Fails the check if the loadgen e2e oracle reports
# any assertion violation. Strictly additive — does not modify the
# existing deploy.yml workflow.

on:
  pull_request:
    branches: [main]

permissions:
  contents: read

concurrency:
  group: loadgen-smoke-${{ github.ref }}
  cancel-in-progress: true

jobs:
  loadgen-smoke:
    name: loadgen smoke (ci-smoke)        # change scenario name here
    runs-on: ubuntu-latest
    timeout-minutes: 5
    steps:
      - uses: actions/checkout@v4

      - uses: actions/setup-go@v5
        with:
          go-version: '1.22'
          cache: false                    # we install loadgen from a
                                          # different module path

      - name: configure git for private aforoai modules
        run: |
          git config --global url."https://x-access-token:${AFORO_LOADGEN_PAT}@github.com/aforoai/".insteadOf "https://github.com/aforoai/"
        env:
          AFORO_LOADGEN_PAT: ${{ secrets.AFORO_LOADGEN_PAT }}

      - name: go install aforo-loadgen
        # Pinned to @main until aforo-nextgen-loadgen ships its first
        # tagged release (v0.1.0). After that, flip to @latest (or
        # @v0.1.0 to pin a known-good version).
        run: go install github.com/aforoai/aforo-nextgen-loadgen/cmd/aforo-loadgen@main
        env:
          GOPRIVATE: github.com/aforoai/*

      - name: print loadgen version
        run: aforo-loadgen version

      - name: smoke
        run: |
          aforo-loadgen e2e \
            --scenario ci-smoke \
            --target ci \
            --duration 60s \
            --strict
        env:
          AFORO_CI_BASE_URL:   ${{ secrets.AFORO_CI_BASE_URL }}
          AFORO_ADMIN_TOKEN:   ${{ secrets.AFORO_STAGING_TOKEN }}
```

---

## Step 3 — Commit + open a PR

Push the new workflow on a branch, open a PR to `main`. The
`loadgen-smoke` check should appear on the PR within ~30 seconds. The
first run is your baseline.

If the check goes green, you have a working smoke gate.

If it goes red, scroll the workflow logs — every assertion failure is
printed by the validate oracle with a clear `CHECK <id> FAIL: <reason>`
prefix. The most common first-time failures:

| Symptom                                                 | Cause                                                                       |
| ------------------------------------------------------- | --------------------------------------------------------------------------- |
| `unknown target "ci"`                                   | Loadgen older than the `--target ci` change — `go install` again with `@main`, or `@v0.1.0` once tagged. |
| `dial tcp: ... connection refused`                      | `AFORO_CI_BASE_URL` is wrong or the staging env is down.                    |
| `target ci: HTTP 401 on /actuator/health`               | `AFORO_STAGING_TOKEN` is missing, expired, or for the wrong env.            |
| `events_lost_max exceeded` on first run                 | Real ingestion regression in your PR. **This is the gate doing its job.**   |

---

## Step 4 — Mark the check required (optional)

Once the smoke gate has been green for ~10 PRs and you are confident it
is stable, mark it required: `Settings → Branches → Branch protection
rule → main → Require status checks → Require branches to be up to
date → Status checks → Add "loadgen smoke (ci-smoke)"`.

Until then, leave it advisory. Required-but-flaky burns trust faster
than red-but-advisory.

---

## Step 5 — Watch for one week, then move on

For the first week the workflow is in place, glance at the most recent
failed runs every couple of days. You are looking for two things:

1. **False positives** — the assertion fired but production is fine.
   These usually trace to the smoke env being saturated by another team's
   load test. The fix is either to dedicate a CI environment or loosen
   the assertion threshold for the relevant scenario.
2. **Job time creep** — the five-minute timeout is the contract.
   `go install` cache misses and slow staging cold starts both eat
   the budget. If the median job time crosses 4m30s, open an issue
   against `aforo-nextgen-loadgen` to investigate.

After the first week, the job should be self-running. The next time it
fails it is genuinely catching a regression.

---

## Fallback: when CI cannot run docker-in-docker

The default workflow above does not need docker-in-docker — it points
loadgen at `--target ci` (a remote staging-like environment). That is
the supported path.

If a future use case needs the smoke job to spin up a docker-compose
stack for the service-under-test:

1. Set up `docker compose` via `docker/setup-buildx-action@v3`.
2. Build the service container locally (`docker compose build`).
3. `docker compose up -d` and wait on `/actuator/health` until ready
   (typically 90 seconds for a Spring Boot service + Postgres + Kafka).
4. Set `AFORO_CI_BASE_URL=http://localhost:<port>` before invoking
   `aforo-loadgen e2e`.

The job time budget tightens to ~3 minutes for the actual scenario in
that mode. Do not enable both modes on the same PR.
