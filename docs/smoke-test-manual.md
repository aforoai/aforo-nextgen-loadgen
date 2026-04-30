# End-to-End Manual Smoke Test (Operator Layer)

This procedure validates the Session 12 acceptance criteria. Run it
once after standing up the loadgen server in any new environment
(local, staging, prod).

## Prerequisites

- `aforo-loadgen` binary built (`make build`).
- Optional for full test: Supabase project with migration 014 applied
  (creates `loadgen_runs` table); Grafana with `dashboards/loadgen-run.json`
  imported.
- A seed manifest at `manifest.json` (run `aforo-loadgen seed --scenario
  ci-smoke --dry-run --out manifest.json` to generate one).

## 1. Server boot and health

```bash
./bin/aforo-loadgen server \
  --listen :8095 \
  --work-dir /tmp/loadgen-server \
  --manifests-dir /tmp/loadgen-manifests \
  --allow-anonymous \
  --static-role platform_admin

# Expected stderr (JSON-formatted):
#   {"time":"...","level":"INFO","msg":"loadgen server listening","addr":":8095"}
```

In a second terminal:

```bash
curl -s http://localhost:8095/api/v1/health | jq
# Expected:
#   {
#     "status": "ok",
#     "active_runs": 0,
#     "storage": "local-fs",
#     "index": "memory"
#   }
```

✅ **Acceptance:** `GET /api/v1/health → 200`.

## 2. Scenario catalog

```bash
TOKEN=anything   # --allow-anonymous treats every bearer as the static identity
curl -s -H "Authorization: Bearer $TOKEN" http://localhost:8095/api/v1/scenarios | jq '.scenarios | length'
# Expected: 14 (matches the bundled scenario count from sessions 1-11)
```

✅ **Acceptance:** Bundled scenarios listed.

## 3. Trigger a run

```bash
RESP=$(curl -s -X POST http://localhost:8095/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"scenario": "ci-smoke", "target": "local", "duration_secs": 10, "manifest_path": "manifest.json"}')
echo $RESP | jq
RUN_ID=$(echo $RESP | jq -r .run_id)

# Expected:
#   { "run_id": "ci-smoke-<unix>-<8hex>", "status": "queued" }
```

## 4. Poll status transitions

```bash
for i in 1 2 3 4 5 6 7 8 9 10; do
  curl -s -H "Authorization: Bearer $TOKEN" \
    http://localhost:8095/api/v1/runs/$RUN_ID | jq -r .status
  sleep 2
done

# Expected sequence (timing varies):
#   queued
#   running
#   running
#   ...
#   completed
```

✅ **Acceptance:** Status transitions queued → running → completed.

## 5. Inspect detail

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:8095/api/v1/runs/$RUN_ID | jq
# Expected fields: assertions[], per_archetype_summary, per_negative_path_stats,
# manifest_s3_path (file://<abs path>), p99_ms, events_sent, events_succeeded.
```

## 6. Manifest download

```bash
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:8095/api/v1/runs/$RUN_ID/manifest | head -3
# Expected: pretty-printed run.json (first lines like {"run_id": "..."}
```

## 7. High-TPS guardrail

```bash
# walk-realistic-50t targets ~1500 TPS — exceeds the 1000 TPS cutoff.
curl -s -X POST http://localhost:8095/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"scenario": "walk-realistic-50t", "target": "local", "duration_secs": 30, "manifest_path": "manifest.json"}'
# Expected status 412:
#   { "error": "scenario \"walk-realistic-50t\" is high-TPS (...); set acknowledge_high_tps=true to confirm" }
```

✅ **Acceptance:** High-TPS scenario rejected without acknowledgement.

## 8. Cancel an in-flight run

```bash
RESP=$(curl -s -X POST http://localhost:8095/api/v1/runs \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"scenario": "ci-smoke", "target": "local", "duration_secs": 60, "manifest_path": "manifest.json"}')
RUN_ID=$(echo $RESP | jq -r .run_id)

# Wait until running...
sleep 3

curl -s -X POST -H "Authorization: Bearer $TOKEN" \
  http://localhost:8095/api/v1/runs/$RUN_ID/cancel | jq
# Expected: { "run_id": "...", "status": "cancel-signalled" }

sleep 5
curl -s -H "Authorization: Bearer $TOKEN" \
  http://localhost:8095/api/v1/runs/$RUN_ID | jq -r .status
# Expected: cancelled (or completed if it finished within the cancel window)
```

✅ **Acceptance:** Cancel returns 202; status moves to `cancelled` within 30s.

## 9. RBAC — non-admin gets rejected

```bash
# Restart the server with a non-admin static role:
./bin/aforo-loadgen server \
  --listen :8096 \
  --work-dir /tmp/loadgen-server-2 \
  --manifests-dir /tmp/loadgen-manifests-2 \
  --allow-anonymous \
  --static-role support_agent &

curl -s -o /dev/null -w "%{http_code}\n" -X POST \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"scenario": "ci-smoke", "target": "local"}' \
  http://localhost:8096/api/v1/runs
# Expected: 403

curl -s -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer $TOKEN" \
  http://localhost:8096/api/v1/runs
# Expected: 200 (read access broader than trigger)
```

✅ **Acceptance:** Trigger blocked for support_agent; read works.

## 10. Control Tower UI

After running steps 1-9, with the loadgen server still running on :8095:

```bash
cd ../control-tower
LOADGEN_SERVER_URL=http://localhost:8095 pnpm dev
# Open http://localhost:3001/loadgen
```

Verify:

- ☐ Runs table loads (rows from steps 3, 8 visible).
- ☐ Click any run row → detail page renders with assertions,
  per-archetype, per-negative-path sections.
- ☐ "Trigger Run" → form shows scenarios in dropdown; submitting
  ci-smoke + local + 30s creates a new run that appears in the table.
- ☐ For high-TPS scenarios, the form shows the amber "I understand"
  checkbox and the Start button stays disabled until ticked.
- ☐ Run detail's "Download manifest" link returns the run.json blob.
- ☐ When `--grafana-base-url` is set, "View live metrics in Grafana →"
  deep-links to the dashboard with `var-runId` populated.
- ☐ A non-platform_admin internal_roles row gets a 403 from the
  trigger button (UI shows toast + the row never lands).

## Server-side automated coverage

The endpoint contracts are pinned by Go integration tests:

```
internal/server/server_test.go:
  TestHealth_PublicNoAuth
  TestListScenarios_OpenToAnyInternalRole
  TestListScenarios_RejectsUnrolled
  TestTriggerRun_AcceptsPlatformAdmin
  TestTriggerRun_RejectsSupportAgent
  TestTriggerRun_RejectsUnknownScenario
  TestTriggerRun_HighTPSRequiresAck
  TestTriggerRun_HighTPSWithAckSucceeds
  TestTriggerRun_RejectsEmptyScenario
  TestGetRun_NotFound
  TestGetRun_RoundTrip
  TestListRuns_PaginationAndStatusFilter
  TestCancelRun_NotActive404
  TestCancelRun_RejectsSupport
  TestUnauthorizedWithoutBearer

internal/server/storage_test.go:
  TestLocalStore_PutAndGet
  TestLocalStore_RejectsPathTraversal
  TestLocalStore_RejectsBadRunID
  TestMemoryIndex_BasicCRUD
  TestMemoryIndex_ListPagination
  TestParseContentRangeTotal
  TestS3Locator_BucketMismatch
  TestLocalPathFromLocator_AbsoluteOnly
```

Run with `make test` or `go test ./internal/server/...`.

The Control Tower UI side does not yet have a test framework set up
(no jest/vitest configured in package.json). A future session can
add Vitest + Testing Library to cover the components mentioned in
the Session 12 spec (RunsTable, TriggerRunForm validation,
RunDetailDrawer). The validation logic is already extracted into
the pure `parseDuration` function in `TriggerRunForm.tsx` precisely
so it's testable when that infrastructure lands.
