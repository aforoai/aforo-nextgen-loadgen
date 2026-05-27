#!/usr/bin/env bash
# sync-openapi.sh — fetch each Aforo backend service's OpenAPI 3.x spec from
# Springdoc and write to openapi/<service>.json.
#
# The committed snapshots are the source of truth for the contract test at
# internal/contract/contract_test.go. Refresh them whenever a backend DTO
# changes and commit both the snapshot AND any loadgen Go-struct changes
# in the same PR so reviewers see the contract drift end-to-end.
#
# Usage:
#   ./scripts/sync-openapi.sh                     # all services (target=local)
#   ./scripts/sync-openapi.sh customer pricing    # subset
#   AFORO_OPENAPI_TARGET=staging ./scripts/sync-openapi.sh
#
# Targets:
#   local    — docker-compose ports on localhost (default)
#   staging  — https://<svc>.aforo.ai (set AFORO_OPENAPI_TARGET=staging)
#
# Exit codes:
#   0 — all requested snapshots successfully fetched and written
#   1 — one or more fetches failed (snapshot left untouched if write would
#       overwrite a good file with empty content)

set -euo pipefail

repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
target="${AFORO_OPENAPI_TARGET:-local}"

# Service → base URL by target. Mirrors internal/aforo/endpoints.go.
declare -A url_local=(
    [organization]="http://localhost:8086"
    [catalog]="http://localhost:8081"
    [pricing]="http://localhost:8083"
    [customer]="http://localhost:8085"
    [billing]="http://localhost:8090"
    [usage-ingestor]="http://localhost:8084"
    [analytics]="http://localhost:8088"
    [storefront]="http://localhost:8089"
)
declare -A url_staging=(
    [organization]="https://org.aforo.ai"
    [catalog]="https://catalog.aforo.ai"
    [pricing]="https://pricing.aforo.ai"
    [customer]="https://customer.aforo.ai"
    [billing]="https://billing.aforo.ai"
    [usage-ingestor]="https://usage-ingestor.aforo.ai"
    [analytics]="https://analytics.aforo.ai"
    [storefront]="https://storefront.aforo.ai"
)

# Pick the map for the chosen target.
case "$target" in
    local)
        urls_var="url_local"
        ;;
    staging | prod)
        urls_var="url_staging"
        ;;
    *)
        echo "Unknown target: $target (use local|staging|prod)" >&2
        exit 1
        ;;
esac

# Default service list = all keys in the chosen map. Override via argv.
if [[ $# -gt 0 ]]; then
    services=("$@")
else
    declare -n url_map="$urls_var"
    services=("${!url_map[@]}")
fi

mkdir -p "$repo_root/openapi"

declare -n url_map="$urls_var"
failures=0
for svc in "${services[@]}"; do
    base="${url_map[$svc]:-}"
    if [[ -z "$base" ]]; then
        echo "skip $svc — no URL configured for target=$target" >&2
        continue
    fi
    out="$repo_root/openapi/$svc.json"
    url="${base}/v3/api-docs"
    echo "fetch $svc → $url"
    # Write to a tmp file first; only replace the committed snapshot if the
    # fetch succeeds AND the body parses as JSON. This guards against an
    # accidental "the service was down so I committed an HTML error page".
    tmp="$(mktemp)"
    trap "rm -f '$tmp'" EXIT
    if ! curl -sS --fail --max-time 30 "$url" -o "$tmp"; then
        echo "  FAIL: HTTP fetch from $url" >&2
        failures=$((failures + 1))
        continue
    fi
    if ! python3 -c "import json,sys; json.load(open('$tmp'))" >/dev/null 2>&1; then
        echo "  FAIL: $url returned non-JSON" >&2
        failures=$((failures + 1))
        continue
    fi
    # Pretty-print for diffable commits. Python's json.tool sorts keys at
    # all levels which makes drift between two snapshots show up as
    # field-level diffs instead of whole-object diffs.
    python3 -m json.tool --sort-keys "$tmp" > "$out"
    echo "  wrote $(wc -c < "$out") bytes to openapi/$svc.json"
done

if [[ $failures -gt 0 ]]; then
    echo "$failures snapshot(s) failed to refresh" >&2
    exit 1
fi
echo "all snapshots refreshed (target=$target)"
