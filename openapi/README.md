# openapi/ — backend service contract snapshots

This directory holds **committed snapshots** of each Aforo backend service's
OpenAPI 3.x spec (via Springdoc's `/v3/api-docs` endpoint). The contract test
at `internal/contract/contract_test.go` reflects over loadgen's Go request /
response structs and asserts every `json:"..."` tag is present in the
corresponding backend schema.

## Why committed snapshots, not live fetch?

- **Hermetic CI**: contract test runs without needing a live backend stack.
- **Deliberate drift surfacing**: if backend renames a field, the contract
  test fails on the PR that re-runs `make sync-openapi` to refresh the
  snapshot — at which point the human reviewing both PRs sees the loadgen
  side that needs to follow. Live fetch would silently pass once backend
  rolls out, then fail loadgen mysteriously when the snapshot diverges.
- **Audit trail**: `git log openapi/<service>.json` shows when a service's
  contract last changed, by whom, and against which loadgen commit.

## How to refresh snapshots

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
  default OpenAPI 3.x JSON output.
- `paths.json` — generated index of (service, path, method) → operation id,
  used by the contract test to look up which schemas are involved in a
  given endpoint loadgen calls. Run `make sync-openapi` to regenerate.

## What does NOT live here

- Storefront BFF endpoints — loadgen's seed harness doesn't call those.
- Internal mesh endpoints (`/internal/v1/*`) — these are exempt from
  Springdoc by convention; loadgen tests them via integration runs.
- ai-service — has no exposed `/v3/api-docs` today (different actuator
  posture). Skipped by the sync script.
