# openapi/ — backend service contract snapshots

This directory holds **committed snapshots** of each Aforo backend service's
OpenAPI 3.x spec (via Springdoc's `/v3/api-docs` endpoint). The contract test
at `internal/seed/contract_test.go` reflects over loadgen's Go request /
response structs and asserts every `json:"..."` tag is present in the
corresponding backend schema.

## Bootstrap (first-time setup — what to expect when no snapshots exist)

The repo intentionally ships **without pre-populated `<service>.json` files**.
Until a maintainer runs `make sync-openapi` against a live backend stack, the
directory contains only this README. This is by design — committing snapshots
without first verifying them against a real running backend is the original
drift trap that the contract test exists to prevent.

### What this means in practice

| State | `make contract-test` behavior | What you should do |
|---|---|---|
| No `<service>.json` files (default in a fresh clone) | Test PASSES with every per-service subtest SKIPPED, citing `openapi snapshot for "<svc>" not available — run \`make sync-openapi\`` | Nothing — the skip is intentional. CI is green. |
| Snapshots present + match loadgen structs | Test PASSES, contract enforced | Nothing — drift gate is armed. |
| Snapshots present + drift detected | Test FAILS with per-field diagnosis | Decide which side is right; update the loser; refresh snapshot if backend changed. |

The skip-vs-fail distinction matters: a fresh clone or a CI run on a sibling
PR that didn't touch a struct should not have to spin up Docker just to pass
the contract test. The skip path keeps the test cheap-by-default; the moment
snapshots land, the test becomes load-bearing.

### Bootstrap procedure (maintainer runs once per environment)

You need:

- Docker Desktop running
- The `aforo-nextgen-docker` sibling repo on disk (next to this one in
  the workspace)
- Pre-built backend jars in each backend service's `target/` directory
  (the local-dev compose file uses simple Dockerfiles that `COPY` pre-built
  jars — it does NOT run Maven inside the container)

Then:

```bash
# 1. Bring up the full backend stack (8 services + PostgreSQL + Kafka + Redis):
cd ../aforo-nextgen-docker
docker compose -f docker-compose.local-dev.yml up -d

# 2. Wait for each backend to report UP via /actuator/health (typically 60-90s).
#    The compose file's healthcheck timing handles dependency ordering; a
#    manual sanity probe:
for port in 8081 8083 8084 8085 8086 8088 8089 8090; do
  printf "  port %s — " "$port"
  curl -sS -o /dev/null -w '%{http_code}\n' "http://localhost:$port/actuator/health" || echo "down"
done

# 3. Back in the loadgen repo, fetch every snapshot:
cd ../aforo-nextgen-loadgen
make sync-openapi

# 4. Commit the resulting files in the SAME PR as any backend coordination
#    that motivated the bootstrap. The PR description should call out
#    "first snapshot population — contract test now ENFORCING for these
#    services" so reviewers know the test gate just became load-bearing.
git add openapi/*.json
git commit -m "loadgen contract: bootstrap openapi snapshots from local stack"
```

After step 4, the contract test moves from "all subtests skipped" to "all
subtests asserting". Any backend rename from that point forward fails the
test on the next sync until loadgen catches up.

### Why bootstrap is a one-time act (not a hidden recurring chore)

`make sync-openapi` is the same command used for both bootstrap and routine
refresh — the only difference is whether the destination files already
exist. There's no separate "init" flow because there shouldn't be: the
snapshot is the snapshot, and re-fetching is the only way to update it.
Read "Maintenance workflow" in `CONVENTIONS.md` for the recurring-refresh
side of the story (when to refresh, what triggers it, how to land the diff).

## Why committed snapshots, not live fetch?

- **Hermetic CI**: contract test runs without needing a live backend stack
  on every PR. Live fetch would force every PR to spin up the entire backend
  matrix just to verify a struct rename.
- **Deliberate drift surfacing**: if backend renames a field, the contract
  test fails on the PR that re-runs `make sync-openapi` to refresh the
  snapshot — at which point the human reviewing both PRs sees the loadgen
  side that needs to follow. Live fetch would silently pass once backend
  rolls out, then fail loadgen mysteriously when the snapshot diverges.
- **Audit trail**: `git log openapi/<service>.json` shows when a service's
  contract last changed, by whom, and against which loadgen commit.

## How to refresh snapshots (after the initial bootstrap)

```bash
# All services (requires the docker-compose stack up):
make sync-openapi

# Single service:
./scripts/sync-openapi.sh customer

# Custom target (e.g. staging):
AFORO_OPENAPI_TARGET=staging make sync-openapi
```

Then commit the changed JSON files alongside any loadgen Go-struct changes
the snapshot refresh forced. The PR description should explain the field-
rename motivation and reference the backend PR that introduced it.

## What lives here

- `<service>.json` — one file per backend service. Format is Springdoc's
  default OpenAPI 3.x JSON output. Populated by the bootstrap procedure
  above; absent on a fresh clone.

## What does NOT live here

- Storefront BFF endpoints — loadgen's seed harness doesn't call those.
- Internal mesh endpoints (`/internal/v1/*`) — these are exempt from
  Springdoc by convention; loadgen tests them via integration runs.
- ai-service — has no exposed `/v3/api-docs` today (different actuator
  posture). Skipped by the sync script.
